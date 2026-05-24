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

func mustDefault(t *testing.T) *Bundle {
	t.Helper()
	b, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	return b
}
