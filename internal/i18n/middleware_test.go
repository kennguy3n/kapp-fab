package i18n

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeTenantProvider returns whatever locale is configured on it
// regardless of the request context. Production wiring would
// resolve through platform.TenantFromContext.
type fakeTenantProvider struct{ locale string }

func (f fakeTenantProvider) LocaleFromContext(context.Context) string { return f.locale }

func TestMiddleware_AcceptLanguageHeader(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(b)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Accept-Language", "fr-CA,fr;q=0.9,en;q=0.5")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "fr" {
		t.Fatalf("captured locale = %q, want %q", captured, "fr")
	}
}

func TestMiddleware_NoHeaderFallsBackToDefault(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(b)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != DefaultLocale {
		t.Fatalf("captured locale = %q, want %q", captured, DefaultLocale)
	}
}

func TestMiddleware_TenantBeatsHeader(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(b, WithTenantLocaleProvider(fakeTenantProvider{locale: "ja"}))(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			captured = FromContext(r.Context())
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Accept-Language", "de,fr;q=0.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "ja" {
		t.Fatalf("tenant override failed: captured = %q, want %q", captured, "ja")
	}
}

func TestMiddleware_QueryBeatsTenantBeatsHeader(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(
		b,
		WithTenantLocaleProvider(fakeTenantProvider{locale: "ja"}),
		WithQueryParam("lang"),
	)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = FromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/?lang=de", http.NoBody)
	req.Header.Set("Accept-Language", "fr,en;q=0.5")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "de" {
		t.Fatalf("query override failed: captured = %q, want %q", captured, "de")
	}
}

func TestMiddleware_CookieBeatsTenantBeatsHeader(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(
		b,
		WithTenantLocaleProvider(fakeTenantProvider{locale: "ja"}),
		WithCookie("kapp_locale"),
	)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = FromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.AddCookie(&http.Cookie{Name: "kapp_locale", Value: "th"})
	req.Header.Set("Accept-Language", "fr,en;q=0.5")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "th" {
		t.Fatalf("cookie override failed: captured = %q, want %q", captured, "th")
	}
}

func TestMiddleware_QueryBeatsCookie(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(
		b,
		WithQueryParam("lang"),
		WithCookie("kapp_locale"),
	)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = FromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/?lang=zh", http.NoBody)
	req.AddCookie(&http.Cookie{Name: "kapp_locale", Value: "th"})
	req.Header.Set("Accept-Language", "en")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "zh" {
		t.Fatalf("query should beat cookie: captured = %q, want %q", captured, "zh")
	}
}

func TestMiddleware_UnsupportedTenantLocaleDowngrades(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	// "hi" is not a shipped catalogue today; the middleware must
	// downgrade through Resolve so downstream T() always sees a
	// supported tag.
	h := Middleware(b, WithTenantLocaleProvider(fakeTenantProvider{locale: "hi"}))(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			captured = FromContext(r.Context())
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Accept-Language", "ja") // would beat tenant if used
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != DefaultLocale {
		t.Fatalf("tenant 'hi' did not downgrade through Resolve: captured = %q, want %q",
			captured, DefaultLocale)
	}
}

func TestMiddleware_EmptyTenantLocaleFallsThrough(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(b, WithTenantLocaleProvider(fakeTenantProvider{locale: ""}))(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			captured = FromContext(r.Context())
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Accept-Language", "ja")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "ja" {
		t.Fatalf("empty tenant locale did not fall through to header: captured = %q, want %q",
			captured, "ja")
	}
}

func TestMiddleware_NilTenantProviderTreatedAsNoop(t *testing.T) {
	b := mustDefault(t)
	captured := ""
	h := Middleware(b, WithTenantLocaleProvider(nil))(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			captured = FromContext(r.Context())
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Accept-Language", "ja")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "ja" {
		t.Fatalf("nil provider should be inert: captured = %q, want %q", captured, "ja")
	}
}

// TestMiddleware_VaryAcceptLanguageAlwaysSet pins the CDN-safety
// contract: every response that flows through the middleware must
// carry "Vary: Accept-Language" so a cache in front of the API
// keys English and German responses separately. The contract holds
// even when the handler itself never reads the locale — the cache
// has no way to know in advance whether a downstream handler will
// emit a translated body, so the middleware always declares the
// variance.
func TestMiddleware_VaryAcceptLanguageAlwaysSet(t *testing.T) {
	b := mustDefault(t)
	h := Middleware(b)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	vary := rr.Header().Values("Vary")
	if !containsToken(vary, "Accept-Language") {
		t.Fatalf("Vary header missing Accept-Language token, got %v", vary)
	}
}

// TestMiddleware_VaryCookieAddedWhenCookieSourceEnabled covers the
// second arm of the Vary contract: a cookie-based locale switcher
// changes the response body per-browser, and CDNs that don't strip
// the Cookie header before caching would serve the wrong locale to
// a different cookie holder. The middleware must declare "Vary:
// Cookie" whenever WithCookie is configured.
func TestMiddleware_VaryCookieAddedWhenCookieSourceEnabled(t *testing.T) {
	b := mustDefault(t)
	h := Middleware(b, WithCookie("kapp_locale"))(
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}),
	)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	vary := rr.Header().Values("Vary")
	if !containsToken(vary, "Accept-Language") {
		t.Fatalf("Vary header missing Accept-Language token, got %v", vary)
	}
	if !containsToken(vary, "Cookie") {
		t.Fatalf("Vary header missing Cookie token when WithCookie is configured, got %v", vary)
	}
}

// TestMiddleware_VaryQueryParamDoesNotAddVary confirms the
// query-param source does NOT contribute a Vary entry. CDNs already
// bucket distinct query strings as distinct cache keys, so adding
// Vary: query-param-name would be both wrong (Vary names HTTP
// request headers, not query parameters) and redundant.
func TestMiddleware_VaryQueryParamDoesNotAddVary(t *testing.T) {
	b := mustDefault(t)
	h := Middleware(b, WithQueryParam("lang"))(
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}),
	)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?lang=de", http.NoBody)
	h.ServeHTTP(rr, req)

	vary := rr.Header().Values("Vary")
	for _, v := range vary {
		if v == "lang" || v == "query" {
			t.Fatalf("query param should NOT contribute to Vary, got %v", vary)
		}
	}
}

// TestMiddleware_VaryAppendsPreserveExisting confirms the middleware
// uses Header.Add semantics so an upstream handler that already set
// e.g. "Vary: Cookie" for an auth-session response keeps that
// signal, and we layer "Vary: Accept-Language" on top rather than
// clobbering it.
func TestMiddleware_VaryAppendsPreserveExisting(t *testing.T) {
	b := mustDefault(t)
	h := Middleware(b)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Vary", "Authorization")
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	vary := rr.Header().Values("Vary")
	if !containsToken(vary, "Accept-Language") {
		t.Fatalf("Vary header missing Accept-Language token, got %v", vary)
	}
	if !containsToken(vary, "Authorization") {
		t.Fatalf("upstream Vary: Authorization was clobbered, got %v", vary)
	}
}

// containsToken returns true if any of the supplied Vary header
// values contains the given token. Vary may be emitted either as a
// single combined header ("Vary: A, B") or as multiple separate
// headers; net/http preserves separate-header form via
// Header.Values, so for the middleware's Header.Add path each token
// appears as its own value.
func containsToken(values []string, token string) bool {
	for _, v := range values {
		if v == token {
			return true
		}
	}
	return false
}

func mustDefault(t *testing.T) *Bundle {
	t.Helper()
	b, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	return b
}
