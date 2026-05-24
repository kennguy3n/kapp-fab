// Package captcha verifies bot-resistance tokens produced by one of
// several anti-abuse providers (Cloudflare Turnstile, hCaptcha,
// Google reCAPTCHA v3) or by an internally-issued Hashcash-style
// proof-of-work challenge.
//
// # Why this package exists
//
// Kapp's public-facing endpoints — KChat-less SSO bootstrap, portal
// magic-link request, KType form submission — accept POSTs from
// unauthenticated clients. The IP rate limiter (internal/platform/
// ip_rate_limiter.go) blunts per-IP brute force, but a determined
// attacker rotating residential proxies can defeat IP-keyed limits
// alone. The honeypot strategy in services/api/forms.go catches
// drive-by scrapers but not adversaries who specifically target the
// form by inspecting the rendered HTML.
//
// Captcha verification is the third layer:
//
//  1. IP rate limit — high-volume, broad-spectrum filter.
//  2. Honeypot — drive-by scraper filter (free, no UX cost).
//  3. Captcha — interactive challenge that scales with adversary
//     resources (Turnstile/hCaptcha solve farms cost ~$1/1000
//     solves) and provides a quantified abuse signal (score, error
//     code, hostname binding).
//
// # Provider abstraction
//
// All four supported providers (Turnstile, hCaptcha, reCAPTCHA v3,
// PoW) implement the same Verifier interface and return the same
// Outcome shape. The HTTP gateway code therefore doesn't have to
// branch on provider — it asks the Verifier to verify a token and
// inspects Success / Score / Errors. Provider-specific tuning
// (minimum score for reCAPTCHA v3, expected hostname for Turnstile,
// difficulty for PoW) is supplied at construction time so the hot
// path stays branch-free.
//
// # Threat model
//
// The package defends against three classes of attack:
//
//  1. Token replay. Each provider's siteverify endpoint marks a
//     token as consumed on its end; the package additionally records
//     the last 1024 consumed tokens in a small LRU so a token that
//     somehow leaked from the provider's audit can't be replayed
//     against Kapp once it's been used.
//
//  2. Forged tokens. The siteverify call is the canonical
//     authentication of token authenticity — we never trust a token
//     by inspecting its bytes. PoW tokens carry an HMAC-SHA256
//     signature over the challenge envelope so a client can't fake a
//     successful solve without the server's HMAC key.
//
//  3. Provider outage. When the upstream siteverify is unreachable
//     we default to fail-closed: VerifyClosed=true means "I cannot
//     prove this is a real human" and the calling middleware denies.
//     An operator who wants graceful degradation can wire a
//     FailOpenIfUpstreamDown=true config but the default is the
//     conservative posture.
package captcha

import (
	"context"
	"errors"
	"time"
)

// Outcome is the result of verifying a captcha token. Fields are
// populated according to the provider's response shape, with
// provider-specific fields (Score, Action) left zero by providers
// that don't carry that data.
type Outcome struct {
	// Success is true when the token was issued by the provider,
	// has not been used before, and satisfies any threshold checks
	// (e.g. reCAPTCHA v3 minimum score). All callers MUST check
	// this field; Score and Action are advisory only.
	Success bool
	// Score is the v3-style confidence (0.0 bot ... 1.0 human).
	// Populated by reCAPTCHA v3 and by PoW (where it carries the
	// realised difficulty / requested difficulty ratio). Zero for
	// providers that don't emit a score.
	Score float64
	// Action is the v3-style action label the client claimed the
	// token was minted for (e.g. "login", "submit"). Populated by
	// reCAPTCHA v3 only. Callers comparing against an expected
	// action MUST do the compare server-side; the field is not
	// reliable across provider boundaries.
	Action string
	// Hostname is the origin the token was minted on, as reported
	// by the provider. Populated by Turnstile and reCAPTCHA v3.
	// Callers MAY compare against an allowlist to detect tokens
	// minted on an unexpected site key.
	Hostname string
	// ChallengeTS is the time the provider issued the challenge.
	// Populated by Turnstile and reCAPTCHA v3. Used to enforce a
	// freshness window (default 5 minutes).
	ChallengeTS time.Time
	// ErrorCodes carries provider-specific failure reasons (e.g.
	// "invalid-input-response", "timeout-or-duplicate"). Populated
	// when Success=false. Logging these helps operators tell apart
	// "user took too long" (legitimate, soft-fail UX) from "this
	// token was already used" (replay attempt, hard-fail audit).
	ErrorCodes []string
}

// Verifier is the abstraction every captcha provider implements.
// Verify is called on the hot path of every public POST that's
// gated by captcha; implementations MUST be safe for concurrent
// use and MUST honour ctx cancellation.
//
// The clientIP argument is the remote address as seen by the
// gateway; it's passed to the provider's siteverify so the
// provider can compare against the IP that solved the challenge.
// Pass "" when the IP can't be determined.
type Verifier interface {
	// Verify checks a token against the provider and returns the
	// outcome. A nil error with Outcome.Success=false means "the
	// provider rejected this token" (use ErrorCodes to diagnose);
	// a non-nil error means "the verifier could not reach a
	// decision" (upstream outage, malformed config). Callers MUST
	// treat the error case as a denial unless their config opts
	// into fail-open behaviour.
	Verify(ctx context.Context, token, clientIP string) (Outcome, error)
	// Provider returns a stable identifier for the provider
	// ("turnstile", "hcaptcha", "recaptcha_v3", "pow", "disabled")
	// suitable for structured-log fields. The string is also used
	// to drive provider-specific error mapping in the HTTP layer.
	Provider() string
}

// ErrUpstreamUnavailable is returned by Verify when the verifier
// could not reach the provider's siteverify endpoint. Callers
// SHOULD treat this as a denial by default; an operator that
// explicitly opts into fail-open via config can swallow it.
var ErrUpstreamUnavailable = errors.New("captcha: upstream verifier unavailable")

// ErrTokenReplay is returned when the verifier's local replay
// cache rejects a token that has already been verified within the
// freshness window. Distinct from a provider's
// "timeout-or-duplicate" error code so the HTTP layer can audit-
// log replay attempts even when the upstream's own replay
// detection would have flagged it.
var ErrTokenReplay = errors.New("captcha: token already consumed")

// ErrTokenStale is returned when a token's ChallengeTS is older
// than the configured freshness window (default 5 minutes).
// Providers' own freshness windows are 2 minutes (Turnstile) to
// 2 minutes (hCaptcha) so this is mostly belt-and-braces for
// reCAPTCHA v3 which doesn't enforce a server-side freshness
// limit at all.
var ErrTokenStale = errors.New("captcha: token outside freshness window")
