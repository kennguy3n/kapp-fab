package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newStubVerifyServer returns an httptest.Server that mimics a
// siteverify endpoint. The supplied responder runs against the
// posted form values and returns the JSON body the test wants
// the verifier to see.
func newStubVerifyServer(t *testing.T, responder func(form url.Values) siteVerifyResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := responder(r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestSiteVerify_Success(t *testing.T) {
	srv := newStubVerifyServer(t, func(form url.Values) siteVerifyResponse {
		if form.Get("secret") != "shh" || form.Get("response") != "tok" {
			t.Errorf("verifier sent unexpected form: %v", form)
		}
		return siteVerifyResponse{
			Success:     true,
			Hostname:    "kapp.example.com",
			ChallengeTS: time.Now().UTC().Format(time.RFC3339),
		}
	})
	t.Cleanup(srv.Close)
	c := newSiteVerifyClient("test", srv.URL, "shh", Options{}.withDefaults())
	out, err := c.verify(context.Background(), "tok", "192.0.2.1")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if !out.Success {
		t.Errorf("expected Success=true, got %+v", out)
	}
	if out.Hostname != "kapp.example.com" {
		t.Errorf("hostname not propagated: %+v", out)
	}
}

func TestSiteVerify_ReplayCacheBlocksSecondAttempt(t *testing.T) {
	srv := newStubVerifyServer(t, func(_ url.Values) siteVerifyResponse {
		return siteVerifyResponse{Success: true}
	})
	t.Cleanup(srv.Close)
	c := newSiteVerifyClient("test", srv.URL, "shh", Options{}.withDefaults())
	first, err := c.verify(context.Background(), "tok", "")
	if err != nil {
		t.Fatalf("first verify err: %v", err)
	}
	if !first.Success {
		t.Fatalf("first verify should have succeeded, got %+v", first)
	}
	_, err = c.verify(context.Background(), "tok", "")
	if !errors.Is(err, ErrTokenReplay) {
		t.Errorf("expected ErrTokenReplay on second verify, got %v", err)
	}
}

func TestSiteVerify_StaleChallengeRejected(t *testing.T) {
	stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	srv := newStubVerifyServer(t, func(_ url.Values) siteVerifyResponse {
		return siteVerifyResponse{
			Success:     true,
			ChallengeTS: stale,
		}
	})
	t.Cleanup(srv.Close)
	c := newSiteVerifyClient("test", srv.URL, "shh", Options{
		FreshnessWindow: 5 * time.Minute,
	}.withDefaults())
	out, err := c.verify(context.Background(), "tok", "")
	if !errors.Is(err, ErrTokenStale) {
		t.Errorf("expected ErrTokenStale, got %v", err)
	}
	if out.Success {
		t.Errorf("stale outcome should not be Success=true: %+v", out)
	}
}

func TestSiteVerify_UpstreamUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := newSiteVerifyClient("test", srv.URL, "shh", Options{}.withDefaults())
	_, err := c.verify(context.Background(), "tok", "")
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("expected ErrUpstreamUnavailable, got %v", err)
	}
}

func TestSiteVerify_MissingTokenShortCircuits(t *testing.T) {
	c := newSiteVerifyClient("test", "http://should-not-reach", "shh", Options{}.withDefaults())
	out, err := c.verify(context.Background(), "", "")
	if err != nil {
		t.Errorf("missing token should not error, got %v", err)
	}
	if out.Success {
		t.Errorf("missing token should not Success=true")
	}
	if len(out.ErrorCodes) == 0 || out.ErrorCodes[0] != "missing-input-response" {
		t.Errorf("expected missing-input-response error code, got %v", out.ErrorCodes)
	}
}

func TestSiteVerify_FailedTokenDoesNotConsumeReplaySlot(t *testing.T) {
	var attempts int
	srv := newStubVerifyServer(t, func(_ url.Values) siteVerifyResponse {
		attempts++
		if attempts == 1 {
			// First attempt: provider returns failure (e.g.
			// user fat-fingered the captcha widget).
			return siteVerifyResponse{
				Success:    false,
				ErrorCodes: []string{"invalid-input-response"},
			}
		}
		// Second attempt with the same token: provider should
		// be reachable. Verifier MUST NOT have cached the first
		// rejection.
		return siteVerifyResponse{Success: true}
	})
	t.Cleanup(srv.Close)
	c := newSiteVerifyClient("test", srv.URL, "shh", Options{}.withDefaults())
	first, err := c.verify(context.Background(), "tok", "")
	if err != nil || first.Success {
		t.Fatalf("first attempt should be reachable-but-denied, got success=%v err=%v", first.Success, err)
	}
	second, err := c.verify(context.Background(), "tok", "")
	if err != nil {
		t.Fatalf("second verify err: %v", err)
	}
	if !second.Success {
		t.Errorf("second verify should succeed after first failed (no replay-slot consumed), got %+v", second)
	}
	if attempts != 2 {
		t.Errorf("expected upstream to be hit twice, got %d", attempts)
	}
}

func TestOptions_FreshnessWindowEffectiveSentinel(t *testing.T) {
	// FreshnessWindow uses zero=default-5min, negative=disabled
	// because the field has no env-var pipeline and the Go
	// zero-value must land on safe production behaviour.
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero is default-5min", 0, 5 * time.Minute},
		{"negative disables", -1, 0},
		{"negative_arbitrary disables", -42 * time.Hour, 0},
		{"positive is verbatim", 90 * time.Second, 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Options{FreshnessWindow: tc.in}.freshnessWindowEffective()
			if got != tc.want {
				t.Errorf("FreshnessWindow=%v effective=%v want=%v", tc.in, got, tc.want)
			}
		})
	}
}

func TestOptions_MinScoreEffectiveSentinel(t *testing.T) {
	// MinScore uses negative=default-0.5, zero=literal-disabled
	// because getenvFloat emits -1 for unset env vars and we want
	// KAPP_CAPTCHA_MIN_SCORE=0 to be honoured as "no threshold".
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"negative is default-0.5", -1, 0.5},
		{"negative_arbitrary is default-0.5", -0.001, 0.5},
		{"zero disables threshold", 0, 0},
		{"positive is verbatim", 0.7, 0.7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Options{MinScore: tc.in}.minScoreEffective()
			if got != tc.want {
				t.Errorf("MinScore=%v effective=%v want=%v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSiteVerify_FreshnessDisableViaNegative(t *testing.T) {
	// Confirm the public sentinel actually disables the
	// client-side freshness check end-to-end: stale challenge
	// stays Success=true when FreshnessWindow=-1.
	stale := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	srv := newStubVerifyServer(t, func(_ url.Values) siteVerifyResponse {
		return siteVerifyResponse{
			Success:     true,
			ChallengeTS: stale,
		}
	})
	t.Cleanup(srv.Close)
	c := newSiteVerifyClient("test", srv.URL, "shh", Options{
		FreshnessWindow: -1,
	}.withDefaults())
	out, err := c.verify(context.Background(), "tok", "")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if !out.Success {
		t.Errorf("expected stale token to pass with FreshnessWindow=-1, got %+v", out)
	}
}

func TestRecaptchaV3_NegativeMinScoreResolvesToDefault(t *testing.T) {
	// Operator left KAPP_CAPTCHA_MIN_SCORE unset → getenvFloat
	// returns -1 → Options.MinScore=-1 → minScoreEffective
	// resolves to the 0.5 default.
	v := NewRecaptchaV3Verifier("shh", Options{MinScore: -1})
	if v.c.minScore != 0.5 {
		t.Errorf("expected negative minScore to resolve to default 0.5, got %v", v.c.minScore)
	}
}

func TestRecaptchaV3_ZeroMinScoreIsLiteralNotDefault(t *testing.T) {
	// Operator set KAPP_CAPTCHA_MIN_SCORE=0 → verifier accepts
	// every non-negative score (no threshold), NOT the legacy
	// 0→0.5 sentinel behaviour. Stored as 0 in the resolved
	// siteVerifyClient.minScore so the > 0 guard in the Verify
	// path short-circuits.
	v := NewRecaptchaV3Verifier("shh", Options{MinScore: 0})
	if v.c.minScore != 0 {
		t.Errorf("expected explicit minScore=0 to be honoured, got %v", v.c.minScore)
	}
}

func TestRecaptchaV3_BelowMinScoreDenied(t *testing.T) {
	srv := newStubVerifyServer(t, func(_ url.Values) siteVerifyResponse {
		return siteVerifyResponse{
			Success: true,
			Score:   0.3,
			Action:  "login",
		}
	})
	t.Cleanup(srv.Close)
	v := &RecaptchaV3Verifier{
		c: newSiteVerifyClient("recaptcha_v3", srv.URL, "shh", Options{MinScore: 0.5}.withDefaults()),
	}
	out, err := v.Verify(context.Background(), "tok", "")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if out.Success {
		t.Errorf("expected deny on score=0.3 minScore=0.5, got %+v", out)
	}
	if !strings.Contains(strings.Join(out.ErrorCodes, ","), "score-below-threshold") {
		t.Errorf("expected score-below-threshold error code, got %v", out.ErrorCodes)
	}
}

func TestTurnstile_HostnameMismatchDenied(t *testing.T) {
	srv := newStubVerifyServer(t, func(_ url.Values) siteVerifyResponse {
		return siteVerifyResponse{
			Success:  true,
			Hostname: "evil.example.com",
		}
	})
	t.Cleanup(srv.Close)
	v := &TurnstileVerifier{
		c: newSiteVerifyClient("turnstile", srv.URL, "shh", Options{
			ExpectedHostname: "kapp.example.com",
		}.withDefaults()),
		opts: Options{ExpectedHostname: "kapp.example.com"},
	}
	out, err := v.Verify(context.Background(), "tok", "")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if out.Success {
		t.Errorf("expected deny on hostname mismatch, got %+v", out)
	}
}
