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

func TestVerify_SkipperBypass(t *testing.T) {
	called := 0
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		Skipper: func(r *http.Request) bool {
			called++
			return strings.HasPrefix(r.URL.Path, "/public/")
		},
	}
	// Skipper returns true → request passes despite missing Origin.
	req := httptest.NewRequest("POST", "/public/forms/abc/submit", strings.NewReader("body"))
	if err := Verify(req, cfg); err != nil {
		t.Errorf("skipper-matched path should bypass CSRF, got %v", err)
	}
	if called != 1 {
		t.Errorf("expected skipper to be called once, got %d", called)
	}

	// Skipper returns false → request goes through origin check.
	req2 := httptest.NewRequest("POST", "/private/x", strings.NewReader("body"))
	if err := Verify(req2, cfg); err == nil {
		t.Error("non-matched path with missing origin should deny")
	}
	if called != 2 {
		t.Errorf("expected skipper to be called twice, got %d", called)
	}
}

func TestVerify_SkipperRunsBeforeBearerCheck(t *testing.T) {
	// Skipper has priority over bearer bypass — a path-level
	// exemption applies even if Authorization is set.
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		SkipBearerAuth: false,
		Skipper:        func(r *http.Request) bool { return r.URL.Path == "/exempt" },
	}
	req := httptest.NewRequest("POST", "/exempt", strings.NewReader("body"))
	if err := Verify(req, cfg); err != nil {
		t.Errorf("skipper match should bypass, got %v", err)
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

func TestMiddleware_AutoIssuesCookieOnSafeMethod(t *testing.T) {
	// When double-submit is enabled, the middleware must self-
	// bootstrap by issuing the cookie on a safe-method response.
	// Closes the previously half-wired gap where IssueCookie was
	// defined but never called from production code (Devin Review
	// finding ANALYSIS_pr-review-job-d967d70cf92e4cc0b9ba19353db36214_0005).
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "__Host-kapp-csrf",
		CookieSecure:   true,
	}
	mw := Middleware(cfg, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/foo", nil)
	handler.ServeHTTP(w, req)
	cookies := w.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == "__Host-kapp-csrf" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatalf("expected auto-issued CSRF cookie on GET, got cookies: %+v", cookies)
	}
	if found.Value == "" {
		t.Error("auto-issued cookie has empty value")
	}
	if !found.Secure {
		t.Error("auto-issued cookie missing Secure flag (cfg.CookieSecure=true)")
	}
}

func TestMiddleware_NoReissueWhenCookiePresent(t *testing.T) {
	// Cookie rotation on every safe-method tick is intentionally
	// avoided so a long-lived SPA can cache the token. Confirm
	// that when the request already carries the cookie, the
	// middleware does NOT overwrite it.
	cfg := Config{
		AllowedOrigins: []string{"https://kapp.example"},
		CookieName:     "__Host-kapp-csrf",
	}
	mw := Middleware(cfg, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/foo", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-kapp-csrf", Value: "preexisting-tok"})
	handler.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-kapp-csrf" {
			t.Errorf("expected no re-issued cookie when one already present, got %q", c.Value)
		}
	}
}

func TestMiddleware_DoesNotAutoIssueWhenCookieNameUnset(t *testing.T) {
	// Default Config (CookieName empty) means double-submit is
	// disabled — Middleware must not set ANY cookie in that mode.
	cfg := Config{AllowedOrigins: []string{"https://kapp.example"}}
	mw := Middleware(cfg, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/foo", nil)
	handler.ServeHTTP(w, req)
	if cookies := w.Result().Cookies(); len(cookies) > 0 {
		t.Errorf("expected no auto-issued cookie when CookieName empty, got %+v", cookies)
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
