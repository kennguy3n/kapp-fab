package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/captcha"
)

// stubVerifier is a Verifier double that returns whatever the
// closure produces; lets the test exercise the middleware's
// branching without standing up a real provider HTTP server.
type stubVerifier struct {
	name string
	fn   func(token, ip string) (captcha.Outcome, error)
}

func (s stubVerifier) Provider() string { return s.name }
func (s stubVerifier) Verify(_ context.Context, token, ip string) (captcha.Outcome, error) {
	return s.fn(token, ip)
}

func newOKHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestCaptchaMiddleware_BypassesBearer(t *testing.T) {
	verifier := stubVerifier{
		name: "test",
		fn: func(_, _ string) (captcha.Outcome, error) {
			t.Errorf("verifier should not have been called for bearer-auth request")
			return captcha.Outcome{}, nil
		},
	}
	called := false
	h := captchaMiddleware(verifier, slog.Default())(newOKHandler(&called))
	req := httptest.NewRequest("POST", "/api/v1/auth/sso", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer abc.def.ghi")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !called {
		t.Error("next handler should have been called for bearer-auth request")
	}
}

func TestCaptchaMiddleware_GrantsWhenProviderSaysYes(t *testing.T) {
	verifier := stubVerifier{
		name: "test",
		fn: func(token, _ string) (captcha.Outcome, error) {
			if token != "tok-123" {
				t.Errorf("got token %q, want tok-123", token)
			}
			return captcha.Outcome{Success: true}, nil
		},
	}
	called := false
	h := captchaMiddleware(verifier, slog.Default())(newOKHandler(&called))
	req := httptest.NewRequest("POST", "/api/v1/forms/abc/submit", strings.NewReader(""))
	req.Header.Set("X-Captcha-Token", "tok-123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !called {
		t.Error("next handler should have been called when verifier grants")
	}
	if w.Code != http.StatusOK {
		t.Errorf("got %d want 200", w.Code)
	}
}

func TestCaptchaMiddleware_DeniesWhenProviderSaysNo(t *testing.T) {
	verifier := stubVerifier{
		name: "test",
		fn: func(_, _ string) (captcha.Outcome, error) {
			return captcha.Outcome{
				Success:    false,
				ErrorCodes: []string{"invalid-input-response"},
			}, nil
		},
	}
	called := false
	h := captchaMiddleware(verifier, slog.Default())(newOKHandler(&called))
	req := httptest.NewRequest("POST", "/api/v1/forms/abc/submit", strings.NewReader(""))
	req.Header.Set("X-Captcha-Token", "tok-bad")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if called {
		t.Error("next handler should not have been called on captcha deny")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d want 403", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "invalid-input-response") {
		t.Errorf("error body should carry the provider's error codes, got %s", string(body))
	}
}

func TestCaptchaMiddleware_DeniesOnUpstreamUnavailable(t *testing.T) {
	verifier := stubVerifier{
		name: "test",
		fn: func(_, _ string) (captcha.Outcome, error) {
			return captcha.Outcome{}, captcha.ErrUpstreamUnavailable
		},
	}
	called := false
	h := captchaMiddleware(verifier, slog.Default())(newOKHandler(&called))
	req := httptest.NewRequest("POST", "/api/v1/forms/abc/submit", strings.NewReader(""))
	req.Header.Set("X-Captcha-Token", "tok")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if called {
		t.Error("next handler should not have been called when upstream is down (fail-closed)")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d want 403", w.Code)
	}
}

func TestCaptchaMiddleware_TokenExtractFromQueryString(t *testing.T) {
	var seen string
	verifier := stubVerifier{
		name: "test",
		fn: func(token, _ string) (captcha.Outcome, error) {
			seen = token
			return captcha.Outcome{Success: true}, nil
		},
	}
	h := captchaMiddleware(verifier, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("POST", "/api/v1/forms/abc/submit?captcha_token=qtok", strings.NewReader(""))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if seen != "qtok" {
		t.Errorf("token from query string not picked up, got %q", seen)
	}
}

func TestCaptchaMiddleware_TokenExtractFromForm(t *testing.T) {
	var seen string
	verifier := stubVerifier{
		name: "test",
		fn: func(token, _ string) (captcha.Outcome, error) {
			seen = token
			return captcha.Outcome{Success: true}, nil
		},
	}
	h := captchaMiddleware(verifier, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("POST", "/api/v1/forms/abc/submit",
		strings.NewReader("captcha_token=ftok"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if seen != "ftok" {
		t.Errorf("token from form body not picked up, got %q", seen)
	}
}

func TestCaptchaMiddleware_NilVerifierDefaultsToDisabled(t *testing.T) {
	called := false
	h := captchaMiddleware(nil, slog.Default())(newOKHandler(&called))
	req := httptest.NewRequest("POST", "/foo", strings.NewReader(""))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !called {
		t.Error("nil verifier should default to Disabled (pass-through)")
	}
}

func TestPoWChallengeHandler_NonPoWVerifierReturns404(t *testing.T) {
	v := captcha.DisabledVerifier{}
	h := powChallengeHandler(v)
	req := httptest.NewRequest("GET", "/captcha/challenge", http.NoBody)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d want 404 for non-PoW verifier", w.Code)
	}
}

func TestPoWChallengeHandler_PoWVerifierIssuesChallenge(t *testing.T) {
	v := captcha.NewPoWVerifier([]byte("0123456789abcdef0123456789abcdef"), 16, 0)
	h := powChallengeHandler(v)
	req := httptest.NewRequest("GET", "/captcha/challenge", http.NoBody)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("got %d want 200", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "challenge") {
		t.Errorf("response should include challenge field, got %s", string(body))
	}
}

func TestIsBearerAuthRequest(t *testing.T) {
	cases := map[string]bool{
		"Bearer abc.def.ghi": true,
		"bearer abc":         true,
		"BEARER abc":         true,
		"Basic abcdef":       false,
		"":                   false,
	}
	for header, want := range cases {
		req := httptest.NewRequest("POST", "/", http.NoBody)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if got := isBearerAuthRequest(req); got != want {
			t.Errorf("isBearerAuthRequest(%q) = %v want %v", header, got, want)
		}
	}
}

func TestCaptchaMiddleware_DistinguishesUpstreamFromDeny(t *testing.T) {
	// Regression: a wrapped error joining ErrUpstreamUnavailable
	// with a transport error must still be detected by
	// errors.Is so the audit log can emit the right severity.
	wrapped := errors.Join(captcha.ErrUpstreamUnavailable, errors.New("dial tcp timeout"))
	verifier := stubVerifier{
		name: "test",
		fn: func(_, _ string) (captcha.Outcome, error) {
			return captcha.Outcome{}, wrapped
		},
	}
	if !errors.Is(wrapped, captcha.ErrUpstreamUnavailable) {
		t.Fatal("test setup: wrapped error should match ErrUpstreamUnavailable")
	}
	h := captchaMiddleware(verifier, slog.Default())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next handler should not have been called on upstream failure")
	}))
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	req.Header.Set("X-Captcha-Token", "tok")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d want 403", w.Code)
	}
}
