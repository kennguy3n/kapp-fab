package i18n

import (
	"context"
	"net/http"
	"strings"
)

// TenantLocaleProvider exposes the persisted UI locale for the
// authenticated tenant, if any. The Accept-Language middleware
// consults it before falling back to the request header so an
// operator's explicit choice in the wizard / admin surface takes
// precedence over the browser's language preference. Returning the
// empty string means "no opinion, use the header"; returning a tag
// not present in the bundle is harmless because Resolve runs
// afterwards and downgrades to the best supported match.
//
// The contract is intentionally narrower than tenant.Tenant so the
// i18n package does not need to import internal/tenant (which would
// create a cycle with anything that builds the Bundle near the top
// of the call graph). A two-method adapter in services/api wires
// platform.TenantFromContext through this interface.
type TenantLocaleProvider interface {
	// LocaleFromContext returns the BCP 47 tag the authenticated
	// tenant has stored. Returns "" when no tenant is on the
	// context or the tenant has no stored locale.
	LocaleFromContext(ctx context.Context) string
}

// noopTenantLocaleProvider is the zero-config provider for
// middleware mounted before any tenant resolution (public auth
// routes, healthchecks). It always returns "" so the middleware
// falls through to Accept-Language parsing.
type noopTenantLocaleProvider struct{}

// LocaleFromContext always returns "" so the middleware drops to the
// next source in the resolution chain. Satisfies the
// TenantLocaleProvider interface so the middleware never has to
// nil-check before calling.
func (noopTenantLocaleProvider) LocaleFromContext(context.Context) string {
	return ""
}

// MiddlewareOption configures Middleware. Future expansions (a
// query-param override for accessibility testing, a cookie source
// for sticky locale switching) plug in here without renaming the
// constructor's signature.
type MiddlewareOption func(*middlewareConfig)

type middlewareConfig struct {
	tenantProvider TenantLocaleProvider
	queryParam     string
	cookieName     string
}

// WithTenantLocaleProvider wires a TenantLocaleProvider into the
// middleware so the persisted tenant locale beats Accept-Language.
// Pass nil to opt out (equivalent to the default).
func WithTenantLocaleProvider(p TenantLocaleProvider) MiddlewareOption {
	return func(c *middlewareConfig) {
		if p == nil {
			c.tenantProvider = noopTenantLocaleProvider{}
			return
		}
		c.tenantProvider = p
	}
}

// WithQueryParam enables a per-request locale override via the
// supplied query parameter (commonly "lang" or "locale"). Useful
// for accessibility QA and for support staff replicating a user's
// experience without changing their stored preference. Pass "" to
// disable the override.
func WithQueryParam(name string) MiddlewareOption {
	return func(c *middlewareConfig) {
		c.queryParam = strings.TrimSpace(name)
	}
}

// WithCookie enables a per-browser locale override via the supplied
// cookie name. Set by the frontend when the user picks a locale
// from a switcher; persists across requests for the same browser
// without requiring auth. Pass "" to disable.
func WithCookie(name string) MiddlewareOption {
	return func(c *middlewareConfig) {
		c.cookieName = strings.TrimSpace(name)
	}
}

// Middleware returns a chi-compatible http.Handler middleware that
// resolves the request's locale and stashes the result on the
// request context via WithLocale. Downstream handlers retrieve it
// via FromContext(r.Context()).
//
// Resolution precedence (highest first):
//  1. Query parameter (if WithQueryParam configured and present).
//  2. Cookie (if WithCookie configured and present).
//  3. The authenticated tenant's persisted locale (when a
//     TenantLocaleProvider is configured and returns non-empty).
//  4. The request's Accept-Language header.
//  5. Bundle.DefaultLocale ("en").
//
// At every step the candidate is passed through Bundle.Resolve so
// the resulting tag is guaranteed to be one the bundle can serve.
// This is what stops a malformed query string or a stale tenant
// locale from making downstream T() calls fall through to the key
// literal.
//
// The middleware never returns 4xx and never writes to the response
// body — a missing or malformed locale source simply drops down the
// precedence chain. The only response surface it touches is the
// Vary header (appended, never replaced) so a CDN or HTTP cache sat
// in front of the API keys on every signal we consulted. Without
// Vary, a CDN that cached an English /api/v1/errors response would
// serve it back to a German Accept-Language client — the canonical
// i18n CDN-poisoning hazard. The Vary append is unconditional even
// before any handler emits a translated body because (1) PR-5 wires
// translated bodies in immediately after this PR lands, and (2) a
// fresh Vary contract introduced atomically with handler changes
// would leave stale cache entries from PR-4-only deployments in
// play during the rollout window. Setting it now means caches
// shed Vary-incorrect entries the moment PR-4 ships, before any
// translated body exists to be miskeyed.
//
// Downstream handlers must use Header().Add("Vary", token) rather
// than Header().Set("Vary", token) when contributing their own
// Vary signals — Set replaces every existing value and would drop
// the middleware's Accept-Language / Cookie tokens, defeating the
// CDN-safety guarantee for that response. Per RFC 9110 §12.5.5
// every Vary token combines into a single effective set, so Add
// is the correct primitive for every chain-mounted middleware and
// handler; Set is reserved for the rare authoritative case where a
// handler is the single source of truth for cache-keying signals
// (none exist in this codebase today).
func Middleware(b *Bundle, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	cfg := &middlewareConfig{
		tenantProvider: noopTenantLocaleProvider{},
	}
	for _, opt := range opts {
		opt(cfg)
	}
	varyValues := buildVaryHeader(cfg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			candidate := resolveCandidate(r, cfg)
			locale := b.Resolve(candidate)
			// Append to any existing Vary the handler chain has
			// already set (auth.Middleware adds "Cookie" for
			// session-bearing responses, for example). Header.Add
			// preserves the existing values, so an upstream
			// "Vary: Cookie" stays intact and we layer
			// "Vary: Accept-Language" on top.
			for _, v := range varyValues {
				w.Header().Add("Vary", v)
			}
			ctx := WithLocale(r.Context(), locale)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// buildVaryHeader returns the deduplicated list of Vary tokens this
// middleware contributes for the configured precedence chain. The
// list is computed once at Middleware() construction so the per-
// request hot path is a single range loop with no allocations.
//
// Tokens are added only when the corresponding source is enabled:
//
//   - Accept-Language is always present (it's the floor of the
//     resolution chain and PR-5 will translate bodies based on it).
//   - Cookie is added when WithCookie is configured. CDNs that
//     don't strip Cookie before caching (most don't) need this to
//     avoid serving a German-cookie response back to an English
//     cookie holder.
//   - The query-param source does NOT need a Vary entry: URLs with
//     different query strings are distinct cache keys by default,
//     so a CDN already buckets ?lang=de and ?lang=fr separately.
func buildVaryHeader(cfg *middlewareConfig) []string {
	values := []string{"Accept-Language"}
	if cfg.cookieName != "" {
		values = append(values, "Cookie")
	}
	return values
}

// resolveCandidate walks the precedence chain and returns the first
// non-empty source. Empty return falls through to Resolve's own
// empty-string handling which yields DefaultLocale.
func resolveCandidate(r *http.Request, cfg *middlewareConfig) string {
	if cfg.queryParam != "" {
		if v := strings.TrimSpace(r.URL.Query().Get(cfg.queryParam)); v != "" {
			return v
		}
	}
	if cfg.cookieName != "" {
		if c, err := r.Cookie(cfg.cookieName); err == nil {
			if v := strings.TrimSpace(c.Value); v != "" {
				return v
			}
		}
	}
	if cfg.tenantProvider != nil {
		if v := strings.TrimSpace(cfg.tenantProvider.LocaleFromContext(r.Context())); v != "" {
			return v
		}
	}
	return r.Header.Get("Accept-Language")
}
