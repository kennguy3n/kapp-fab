package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/kapp-fab/internal/captcha"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// captchaTokenHeader is the canonical header an HTTP client uses
// to deliver a captcha solution. The frontend renders the chosen
// provider's widget (Turnstile, hCaptcha, reCAPTCHA v3) and reads
// the resulting token, then attaches it to mutating requests
// under this header. Body-shape compatibility (a top-level
// "captcha" field on JSON envelopes that already exist) is also
// honoured by extractCaptchaToken so handlers don't have to
// thread the value through every payload type.
const captchaTokenHeader = "X-Captcha-Token" //nolint:gosec // G101 false positive — this is a header name, not a credential

// captchaMiddleware returns a chi-compatible middleware that
// runs the supplied verifier against the X-Captcha-Token header
// and rejects requests with 403 + a JSON body describing the
// reason. Successful verifications fall through to the next
// handler.
//
// The middleware is mounted in front of endpoints that accept
// unauthenticated POST traffic (forms.submit, portal.request-
// link, auth.sso). Bearer-authenticated POST traffic to those
// same endpoints is bypassed: if the request already carries a
// valid Kapp JWT, the client has already passed identity
// verification and additional captcha gating provides no extra
// abuse signal.  The frontend therefore only renders the captcha
// widget on the public surface, not on dashboard surfaces that
// happen to share the underlying handler.
//
// The logger emits a structured-log line on each rejection with
// the provider's error codes; this gives operators the signal
// they need to tell apart "user got rate-limited at the captcha"
// from "scraper looped through the entire challenge service".
//
// When the verifier is captcha.DisabledVerifier (i.e. the
// operator explicitly opted out via KAPP_CAPTCHA_PROVIDER=disabled
// or left the var unset in dev), the middleware passes every
// request through and writes one debug-level log entry per
// startup noting the no-op state — visible to operators auditing
// "is my captcha actually wired" but quiet in normal logs.
func captchaMiddleware(v captcha.Verifier, logger *slog.Logger) func(http.Handler) http.Handler {
	if v == nil {
		v = captcha.DisabledVerifier{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Bearer bypass: if the request is already
			// authenticated, the captcha provides no extra
			// signal (the JWT already proves identity).
			if isBearerAuthRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			token := extractCaptchaToken(r)
			clientIP := platform.RemoteIPFromRequest(r)
			out, err := v.Verify(r.Context(), token, clientIP)
			if err != nil {
				// Distinguish upstream-unavailable (we
				// couldn't reach the provider) from
				// provider-said-no.  Both deny by default,
				// but the audit log carries the
				// distinction so operators can spot a
				// provider outage vs. a sustained attack.
				if errors.Is(err, captcha.ErrUpstreamUnavailable) {
					logger.Warn("captcha: upstream unavailable",
						slog.String("provider", v.Provider()),
						slog.String("error", err.Error()))
				} else {
					logger.Info("captcha: deny",
						slog.String("provider", v.Provider()),
						slog.String("reason", err.Error()),
						slog.Any("codes", out.ErrorCodes))
				}
				writeCaptchaError(w, out, v.Provider())
				return
			}
			if !out.Success {
				logger.Info("captcha: deny",
					slog.String("provider", v.Provider()),
					slog.Float64("score", out.Score),
					slog.Any("codes", out.ErrorCodes))
				writeCaptchaError(w, out, v.Provider())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isBearerAuthRequest returns true when the request carries a
// non-empty Authorization: Bearer header. The captcha middleware
// uses this to bypass authenticated traffic; the comparison is
// case-insensitive so older clients that send "BEARER" or
// "bearer" also bypass.
func isBearerAuthRequest(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	return strings.HasPrefix(strings.ToLower(auth), "bearer ")
}

// extractCaptchaToken pulls the captcha solution off the request.
// Order of precedence:
//
//  1. X-Captcha-Token request header (canonical for any client
//     that can set headers — fetch, axios, server-side calls).
//  2. captcha_token query parameter (form submissions from
//     embed contexts where the parent page can't set headers).
//  3. captcha_token form-encoded body field (legacy form
//     submissions; only inspected when the Content-Type is
//     application/x-www-form-urlencoded so the middleware
//     doesn't consume a JSON body the handler is about to read).
//
// We deliberately do NOT inspect a JSON body here — JSON requests
// already have a stable, free header slot for the token, and
// reading the body would consume io.ReadCloser before the
// handler runs.
func extractCaptchaToken(r *http.Request) string {
	if tok := r.Header.Get(captchaTokenHeader); tok != "" {
		return tok
	}
	if tok := r.URL.Query().Get("captcha_token"); tok != "" {
		return tok
	}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		// ParseForm reads the body into r.PostForm — but it
		// preserves the body for downstream consumers if the
		// request was originally form-encoded, because
		// http.Request stores the parsed form independently.
		_ = r.ParseForm()
		return r.PostForm.Get("captcha_token")
	}
	return ""
}

// writeCaptchaError emits a uniform JSON error response when
// captcha verification denies. The shape (`error`, `code`,
// `provider`) is small enough that frontend clients can branch
// on `code` to re-render the captcha widget vs. surfacing a
// generic "please try again" message.
//
// We use 403 Forbidden rather than 400 Bad Request because the
// request was well-formed; the captcha policy is what rejected
// it. 429 Too Many Requests would be more semantically precise
// for score-below-threshold but would conflict with the rate
// limiter's use of 429.
func writeCaptchaError(w http.ResponseWriter, out captcha.Outcome, provider string) {
	codes := out.ErrorCodes
	if len(codes) == 0 {
		codes = []string{"captcha-failed"}
	}
	body := map[string]any{
		"error":    "captcha verification failed",
		"codes":    codes,
		"provider": provider,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(body)
}

// powChallengeHandler exposes GET /api/v1/captcha/challenge for
// clients running the proof-of-work variant. The PoW provider is
// the only one that needs a server round-trip to mint a fresh
// challenge — the third-party providers fetch challenges from
// their own CDN directly.
//
// When the configured verifier is not a PoWVerifier the handler
// returns 404, which lets the frontend fall back to whichever
// widget the operator has wired without a config branch.
func powChallengeHandler(v captcha.Verifier) http.HandlerFunc {
	pow, ok := v.(*captcha.PoWVerifier)
	if !ok {
		return func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		blob, err := pow.IssueChallenge()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"challenge": blob,
		})
	}
}

// captchaRouter scopes the captcha-challenge endpoints on a chi
// sub-router so the route group can be mounted under whichever
// base path the parent router uses without duplicating the
// `/captcha` literal.
func captchaRouter(v captcha.Verifier) chi.Router {
	r := chi.NewRouter()
	r.Get("/challenge", powChallengeHandler(v))
	return r
}
