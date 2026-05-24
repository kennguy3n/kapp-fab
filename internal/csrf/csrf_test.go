package csrf

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerify_SafeMethodBypass(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://kapp.example"}}
	for _, m := range []string{"GET", "HEAD", "OPTIONS", "TRACE"} {
		req := httptest.NewRequest(m, "/foo", http.NoBody)
		// No Origin header on purpose — safe methods should still
		// pass.
		if err := Verify(req, cfg); err != nil {
			t.Errorf("safe method %s should bypass CSRF check, got %v", m, err)
		}
	}
}

func TestVerify_BearerBypass(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "csrf",
		SkipBearerAuth: true,
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Authorization", "Bearer some.jwt.value")
	// Deliberately no Origin / no cookie — bearer bypass should
	// still pass.
	if err := Verify(req, cfg); err != nil {
		t.Errorf("bearer-auth request should bypass CSRF, got %v", err)
	}
}

func TestVerify_OriginMissingDenies(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	if err := Verify(req, cfg); err == nil {
		t.Error("missing Origin / Referer should deny")
	}
}

func TestVerify_OriginNotInAllowlist(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://evil.example")
	if err := Verify(req, cfg); err == nil {
		t.Error("disallowed origin should deny")
	}
}

func TestVerify_OriginAllowed_NoDoubleSubmit(t *testing.T) {
	// With CookieName empty, the double-submit check is disabled
	// and Origin allowlist is the only defence.
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://kapp.example")
	if err := Verify(req, cfg); err != nil {
		t.Errorf("allowed origin should pass without double-submit, got %v", err)
	}
}

func TestVerify_RefererFallback(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	// No Origin; Referer fallback should be parsed.
	req.Header.Set("Referer", "https://kapp.example/some/page?with=query")
	if err := Verify(req, cfg); err != nil {
		t.Errorf("Referer fallback should pass when scheme://host matches, got %v", err)
	}
}

func TestVerify_DoubleSubmitMismatch(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "csrf",
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://kapp.example")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "abc"})
	req.Header.Set("X-CSRF-Token", "xyz")
	if err := Verify(req, cfg); err == nil {
		t.Error("cookie/header mismatch should deny")
	}
}

func TestVerify_DoubleSubmitMatch(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "csrf",
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://kapp.example")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "abc"})
	req.Header.Set("X-CSRF-Token", "abc")
	if err := Verify(req, cfg); err != nil {
		t.Errorf("matching cookie/header should pass, got %v", err)
	}
}

func TestVerify_DoubleSubmitMissingCookie(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "csrf",
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://kapp.example")
	req.Header.Set("X-CSRF-Token", "abc")
	if err := Verify(req, cfg); err == nil {
		t.Error("missing cookie should deny")
	}
}

func TestVerify_DoubleSubmitMissingHeader(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "csrf",
	}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://kapp.example")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "abc"})
	if err := Verify(req, cfg); err == nil {
		t.Error("missing header should deny")
	}
}

func TestIssueCookie_TokenSize(t *testing.T) {
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "csrf",
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", http.NoBody)
	tok, err := IssueCookie(w, req, cfg)
	if err != nil {
		t.Fatalf("IssueCookie err: %v", err)
	}
	if len(tok) < 32 {
		t.Errorf("token too short: %d chars", len(tok))
	}
	resp := w.Result()
	t.Cleanup(func() { _ = resp.Body.Close() })
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "csrf" {
		t.Errorf("expected csrf cookie set, got %+v", cookies)
	}
}

func TestMiddleware_RejectsForbidden(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://kapp.example"}}
	var capturedErr error
	mw := Middleware(cfg, func(_ *http.Request, err error) { capturedErr = err })
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://evil.example")
	handler.ServeHTTP(w, req)
	if called {
		t.Error("next handler should not have been called")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "forbidden") {
		t.Errorf("expected generic forbidden body, got %s", string(body))
	}
	if capturedErr == nil {
		t.Error("expected logger to be called with non-nil err")
	}
}

func TestMiddleware_PassesAllowed(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://kapp.example"}}
	mw := Middleware(cfg, nil)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://kapp.example")
	handler.ServeHTTP(w, req)
	if !called {
		t.Error("next handler should have been called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestVerify_EmptyAllowlistAllowsAnyOrigin(t *testing.T) {
	// Document the explicit zero-config behaviour: with no
	// origins configured, Origin check is disabled (so the
	// middleware degrades to "no defence" rather than
	// "always-deny" which would make local dev unbootable).
	// This is intentional, but production deployments MUST
	// populate AllowedOrigins.
	cfg := Config{}
	req := httptest.NewRequest("POST", "/foo", strings.NewReader("body"))
	req.Header.Set("Origin", "https://anywhere.example")
	if err := Verify(req, cfg); err != nil {
		t.Errorf("empty allowlist should not deny, got %v", err)
	}
}

func TestVerify_NeverPanicsOnNilRequestFields(t *testing.T) {
	// Defensive: a malformed request that constructs incorrectly
	// shouldn't panic the middleware.  A bare POST without
	// Origin / Referer / cookies / Authorization should deny
	// rather than crash.
	cfg := Config{AllowedOrigins: []string{"https://kapp.example"}}
	req := &http.Request{
		Method: "POST",
		Header: http.Header{},
	}
	if err := Verify(req, cfg); err == nil {
		t.Error("bare POST with no auth, origin, or cookie should deny")
	}
}

func TestIsSafeMethod(t *testing.T) {
	cases := map[string]bool{
		"GET":     true,
		"HEAD":    true,
		"OPTIONS": true,
		"TRACE":   true,
		"POST":    false,
		"PUT":     false,
		"PATCH":   false,
		"DELETE":  false,
		// case insensitivity
		"get":  true,
		"post": false,
	}
	for m, want := range cases {
		if got := IsSafeMethod(m); got != want {
			t.Errorf("IsSafeMethod(%q) = %v want %v", m, got, want)
		}
	}
}
