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
		c:        newSiteVerifyClient("recaptcha_v3", srv.URL, "shh", Options{}.withDefaults()),
		minScore: 0.5,
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
