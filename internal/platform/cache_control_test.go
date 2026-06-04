package platform

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCacheControlMiddleware_DefaultPolicy verifies the default rule
// set maps each surface to the intended Cache-Control value: dynamic
// API responses are no-store, content-hashed assets are immutable,
// and the SPA shell revalidates with no-cache.
func TestCacheControlMiddleware_DefaultPolicy(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"api_root", "/api/v1/", CacheControlNoStore},
		{"api_nested", "/api/v1/records/123", CacheControlNoStore},
		{"assets_js", "/assets/index-a1b2c3.js", CacheControlImmutable},
		{"assets_css", "/assets/app-deadbeef.css", CacheControlImmutable},
		{"spa_root", "/", CacheControlNoCache},
		{"index_html", "/index.html", CacheControlNoCache},
	}

	mw := CacheControlMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, http.NoBody)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if got := rec.Header().Get(headerCacheControl); got != tc.want {
				t.Errorf("path %q: want Cache-Control %q, got %q", tc.path, tc.want, got)
			}
		})
	}
}

// TestCacheControlMiddleware_CatchAllDefaultsToNoCache verifies that any
// path which is neither a hashed asset nor the API — SPA deep-link
// routes (/dashboard, /settings/users) served as index.html by the SPA
// fallback, and any other origin path — defaults to no-cache. This is
// the catch-all that makes the origin mirror the edge so deep-links
// still revalidate even if the API is exposed without Caddy in front.
func TestCacheControlMiddleware_CatchAllDefaultsToNoCache(t *testing.T) {
	mw := CacheControlMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/dashboard", "/settings/users", "/healthz", "/favicon.ico"} {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get(headerCacheControl); got != CacheControlNoCache {
			t.Errorf("path %q: want Cache-Control %q, got %q", path, CacheControlNoCache, got)
		}
	}
}

// TestCacheControlMiddleware_CatchAllDoesNotWeakenAPIorAssets guards the
// rule ordering: the trailing "/" catch-all must not override the more
// specific /api/ (no-store) and /assets/ (immutable) rules, which are
// listed first and therefore win.
func TestCacheControlMiddleware_CatchAllDoesNotWeakenAPIorAssets(t *testing.T) {
	mw := CacheControlMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := map[string]string{
		"/api/v1/records":    CacheControlNoStore,
		"/assets/app-abc.js": CacheControlImmutable,
	}
	for path, want := range cases {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get(headerCacheControl); got != want {
			t.Errorf("path %q: want %q, got %q", path, want, got)
		}
	}
}

// TestCacheControlMiddleware_HandlerOverridesDefault verifies the
// middleware sets the value as a DEFAULT: a handler that writes its own
// Cache-Control (as the marketplace bundle download and the SSE stream
// do) wins, because the header map is only flushed when the handler
// writes the response. This is the contract that lets content-addressed
// immutable downloads opt out of the /api/ no-store default.
func TestCacheControlMiddleware_HandlerOverridesDefault(t *testing.T) {
	mw := CacheControlMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerCacheControl, CacheControlImmutable)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/marketplace/bundles/abc", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(headerCacheControl); got != CacheControlImmutable {
		t.Errorf("handler override: want %q, got %q", CacheControlImmutable, got)
	}
}

// TestCacheControlMiddleware_CustomRulesFirstMatchWins verifies that an
// explicit rule set is honoured and that evaluation stops at the first
// matching rule, so more specific prefixes listed first take precedence.
func TestCacheControlMiddleware_CustomRulesFirstMatchWins(t *testing.T) {
	rules := []CacheControlRule{
		{Path: "/api/v1/public/", Value: "public, max-age=60"},
		{Path: "/api/", Value: CacheControlNoStore},
	}
	mw := CacheControlMiddleware(rules...)
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	cases := map[string]string{
		"/api/v1/public/plans": "public, max-age=60",
		"/api/v1/records":      CacheControlNoStore,
	}
	for path, want := range cases {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get(headerCacheControl); got != want {
			t.Errorf("path %q: want %q, got %q", path, want, got)
		}
	}
}

// TestMatchCacheControl_ExactVsPrefix guards the matcher's exact/prefix
// distinction on a custom rule set: an Exact rule matches only its exact
// path, while a non-Exact rule matches any path with that prefix. (The
// default rules use prefix matching, so this exercises the Exact branch
// that custom callers rely on.)
func TestMatchCacheControl_ExactVsPrefix(t *testing.T) {
	rules := []CacheControlRule{
		{Path: "/exact", Exact: true, Value: CacheControlNoStore},
		{Path: "/prefix", Value: CacheControlNoCache},
	}

	if v, ok := matchCacheControl(rules, "/exact"); !ok || v != CacheControlNoStore {
		t.Errorf("exact path: want (%q,true), got (%q,%v)", CacheControlNoStore, v, ok)
	}
	// An exact rule must NOT match a longer path that merely shares the prefix.
	if v, ok := matchCacheControl(rules, "/exact/child"); ok {
		t.Errorf("exact rule must not match /exact/child, got %q", v)
	}
	// A non-Exact rule matches any path under its prefix.
	if v, ok := matchCacheControl(rules, "/prefix/deep/path"); !ok || v != CacheControlNoCache {
		t.Errorf("prefix path: want (%q,true), got (%q,%v)", CacheControlNoCache, v, ok)
	}
}
