package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// siteVerifyResponse is the shared shape returned by Turnstile,
// hCaptcha, and reCAPTCHA v3's siteverify endpoints. The three
// vendors converged on essentially the same JSON envelope so we
// share a single decoder and let the per-provider wrapper post-
// process the fields it cares about (score thresholding for
// reCAPTCHA, hostname matching for Turnstile, etc.).
//
// Field tags use the snake_case the providers emit. ChallengeTS
// is the timestamp the challenge was solved (provider-side); we
// parse it in the per-provider wrapper to apply the freshness
// window.
type siteVerifyResponse struct {
	Success     bool     `json:"success"`
	Score       float64  `json:"score,omitempty"`
	Action      string   `json:"action,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	ChallengeTS string   `json:"challenge_ts,omitempty"`
	ErrorCodes  []string `json:"error-codes,omitempty"`
}

// siteVerifyClient is the shared HTTP client used by every provider
// that posts to a remote siteverify endpoint. We keep one client
// per provider instance (not per call) so HTTP keep-alives reduce
// the per-verification latency from a fresh-handshake worst-case
// ~150ms to ~15ms inside the same datacentre.
//
// Timeout is short on purpose: a captcha verification that takes
// longer than 5 seconds is functionally indistinguishable from a
// provider outage at the user's experience level, and the request
// budget on most callers (Forms.Submit, Portal.RequestLink) is
// already 30 seconds total — leaving room for the actual handler
// to run after the captcha check completes.
type siteVerifyClient struct {
	url         string
	secret      string
	httpClient  *http.Client
	replayCache *platform.LRUCache
	provider    string
	// freshnessWindow is the resolved (post-sentinel) staleness
	// cap. 0 means "client-side freshness check disabled" — see
	// Options.freshnessWindowEffective for the input mapping.
	freshnessWindow time.Duration
	// minScore is the resolved (post-sentinel) score floor.
	// 0 means score thresholding is disabled. Read by verify()
	// for siteverify-style providers (reCAPTCHA v3); Turnstile
	// and hCaptcha report Score=0 and so always pass this gate.
	minScore float64
}

func newSiteVerifyClient(provider, verifyURL, secret string, opts Options) *siteVerifyClient {
	replayCache := platform.NewLRUCache(opts.ReplayCacheSize, opts.ReplayCacheTTL)
	return &siteVerifyClient{
		url:             verifyURL,
		secret:          secret,
		httpClient:      &http.Client{Timeout: opts.HTTPTimeout},
		replayCache:     replayCache,
		provider:        provider,
		freshnessWindow: opts.freshnessWindowEffective(),
		minScore:        opts.minScoreEffective(),
	}
}

// verify is the shared siteverify call shape. It posts secret+
// response+remoteip as application/x-www-form-urlencoded (every
// provider accepts this; some accept multipart too but form-
// encoded is the lowest common denominator), parses the response,
// applies replay and freshness checks, and returns a normalised
// Outcome. Per-provider wrappers wrap this and apply their own
// post-checks (score threshold, hostname match) on top.
func (c *siteVerifyClient) verify(ctx context.Context, token, clientIP string) (Outcome, error) {
	if token == "" {
		// Caller forgot to attach a token. Distinct from a
		// provider rejection: the provider is never reached.
		return Outcome{
			Success:    false,
			ErrorCodes: []string{"missing-input-response"},
		}, nil
	}
	// Token replay guard. We key on the token bytes themselves so
	// a leaked token (e.g. via a proxy log) cannot be reused even
	// before the provider's own dedup window closes. The cache
	// entry stores the consumed-at timestamp purely for debug
	// inspection; the gate is presence/absence.
	if _, hit := c.replayCache.Get(token); hit {
		return Outcome{
			Success:    false,
			ErrorCodes: []string{"timeout-or-duplicate"},
		}, ErrTokenReplay
	}
	form := url.Values{}
	form.Set("secret", c.secret)
	form.Set("response", token)
	if clientIP != "" {
		form.Set("remoteip", clientIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Outcome{}, fmt.Errorf("captcha: build verify req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Distinguish upstream-unavailable (network error,
		// timeout) from upstream-decided-no so the HTTP layer
		// can audit-log differently. Both deny by default.
		return Outcome{}, errors.Join(ErrUpstreamUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Outcome{}, fmt.Errorf("%w: status %d", ErrUpstreamUnavailable, resp.StatusCode)
	}
	var decoded siteVerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return Outcome{}, fmt.Errorf("captcha: decode verify response: %w", err)
	}
	out := Outcome{
		Success:    decoded.Success,
		Score:      decoded.Score,
		Action:     decoded.Action,
		Hostname:   decoded.Hostname,
		ErrorCodes: decoded.ErrorCodes,
	}
	if decoded.ChallengeTS != "" {
		if t, err := time.Parse(time.RFC3339Nano, decoded.ChallengeTS); err == nil {
			out.ChallengeTS = t
		}
	}
	if !decoded.Success {
		// Provider already said no — return the outcome, don't
		// mark the token as consumed. (Some providers reject
		// before they would have consumed; keeping the cache
		// clean of failed attempts avoids burning a slot per
		// scan probe.)
		return out, nil
	}
	// Client-side freshness check. Belt-and-braces: most providers
	// already enforce a window (Turnstile 2min, hCaptcha 2min),
	// but reCAPTCHA v3 deliberately doesn't, so we do it
	// ourselves to keep the threat model symmetric across
	// providers.
	if c.freshnessWindow > 0 && !out.ChallengeTS.IsZero() {
		if time.Since(out.ChallengeTS) > c.freshnessWindow {
			return Outcome{
				Success:    false,
				ErrorCodes: []string{"timeout-or-duplicate"},
				Hostname:   out.Hostname,
			}, ErrTokenStale
		}
	}
	// Mark the token as consumed for the local replay window.
	// We do this AFTER all post-checks so a token that failed a
	// post-check can be retried by the user without burning the
	// replay slot (the provider will reject the second attempt
	// on its end, which is the right authority).
	c.replayCache.Set(token, time.Now())
	return out, nil
}

// Options is the shared construction-time configuration for the
// siteverify-style providers. Sane defaults are filled in by
// withDefaults when zero values are passed (a verifier with all
// fields zeroed still works).
type Options struct {
	// HTTPTimeout caps how long we wait for the upstream
	// siteverify call before declaring the provider unavailable.
	// Default 5s.
	HTTPTimeout time.Duration
	// ReplayCacheSize bounds the local replay-detection LRU.
	// Default 1024 entries — enough to cover the freshness
	// window's worth of traffic at moderate QPS.
	ReplayCacheSize int
	// ReplayCacheTTL is the entry expiry in the replay LRU. Must
	// be >= FreshnessWindow so a token can't be replayed inside
	// its own freshness window. Default 10 minutes.
	ReplayCacheTTL time.Duration
	// FreshnessWindow caps how stale a challenge_ts can be
	// before the verifier rejects it client-side.
	//
	// Tri-state sentinel:
	//   - Zero (the Go default for time.Duration): the verifier
	//     applies the 5-minute default. This is what every
	//     in-code `Options{}` construction gets and what
	//     production callers want — a defence-in-depth window
	//     on top of the provider's own dedup.
	//   - Negative: explicitly disables the client-side
	//     freshness check. Use this when a test stubs the
	//     provider clock to a far-future or far-past time, or
	//     when an operator deliberately wants the provider's
	//     own dedup window to be the sole authority.
	//   - Positive: used verbatim as the staleness cap.
	//
	// Why the polarity is the inverse of MinScore: there is no
	// KAPP_CAPTCHA_FRESHNESS_WINDOW env var — every caller
	// constructs Options{} programmatically. Choosing zero as
	// "default 5min" means the natural Go zero-value lands on
	// the safe production behaviour. MinScore, by contrast,
	// flows from getenvFloat which emits -1 for an unset env
	// var — so negative is the "unset" sentinel for that field.
	// Different input pipelines, different conventions; both
	// documented at the field where they apply.
	FreshnessWindow time.Duration
	// ExpectedHostname, when non-empty, is checked against the
	// provider-reported hostname; mismatch denies. Useful when
	// multiple Kapp deployments share a single site key by
	// accident and the operator wants to detect that case.
	ExpectedHostname string
	// MinScore is the lower bound on score (reCAPTCHA v3 / PoW)
	// for a token to be accepted. Tokens with Score below the
	// threshold are denied even when the provider reports
	// success=true.
	//
	// Tri-state sentinel matching FreshnessWindow above:
	//   - Negative: "unset" — minScoreEffective resolves to the
	//     0.5 default (Google's recommended starting point).
	//   - Zero: explicitly disables score thresholding (every
	//     successful-from-provider token is accepted regardless
	//     of score). Honouring this literally is the whole point
	//     of the sentinel: an operator who sets KAPP_CAPTCHA_
	//     MIN_SCORE=0 wants the threshold off, not the default.
	//   - Positive: used verbatim as the score floor.
	//
	// Read by newSiteVerifyClient via minScoreEffective and
	// applied in the reCAPTCHA v3 Verify path. Was previously
	// read by per-provider constructors via a separate
	// argument; consolidating here removes a misleading
	// "unused dead-code" appearance and makes the configuration
	// uniform across the siteverify providers. Turnstile and
	// hCaptcha report Score=0 unconditionally so this field is
	// a no-op for them.
	MinScore float64
}

// freshnessWindowEffective resolves the tri-state FreshnessWindow
// sentinel into the concrete duration the verifier should use, or
// 0 when the client-side check is disabled. Pulled out as a method
// so withDefaults() can keep the doc comments authoritative and
// the verify path doesn't have to re-interpret the sentinel.
func (o Options) freshnessWindowEffective() time.Duration {
	switch {
	case o.FreshnessWindow < 0:
		return 0
	case o.FreshnessWindow == 0:
		return 5 * time.Minute
	default:
		return o.FreshnessWindow
	}
}

// minScoreEffective resolves the tri-state MinScore sentinel into
// the concrete threshold the verifier should apply, or 0 when
// score thresholding is disabled. The 0.5 default matches
// Google's recommended starting point for reCAPTCHA v3.
func (o Options) minScoreEffective() float64 {
	switch {
	case o.MinScore < 0:
		return 0.5
	case o.MinScore == 0:
		return 0
	default:
		return o.MinScore
	}
}

func (o Options) withDefaults() Options {
	if o.HTTPTimeout == 0 {
		o.HTTPTimeout = 5 * time.Second
	}
	if o.ReplayCacheSize == 0 {
		o.ReplayCacheSize = 1024
	}
	if o.ReplayCacheTTL == 0 {
		o.ReplayCacheTTL = 10 * time.Minute
	}
	// FreshnessWindow and MinScore intentionally not collapsed
	// here — withDefaults pre-dates the tri-state sentinel and
	// callers downstream consult freshnessWindowEffective /
	// minScoreEffective directly so the sentinel survives.
	return o
}
