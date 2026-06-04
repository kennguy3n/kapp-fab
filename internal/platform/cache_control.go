package platform

import (
	"net/http"
	"strings"
)

// Cache-Control header name and the three policy values the platform
// applies across its public surface. Defined as constants so the
// middleware, its wiring in services/api, and the tests all reference
// one spelling.
const (
	headerCacheControl = "Cache-Control"

	// CacheControlNoStore forbids any storage of the response by
	// browsers or shared caches (CDNs, reverse proxies). Applied to the
	// dynamic, tenant-scoped API surface — those responses are
	// authenticated and isolated by Postgres RLS on app.tenant_id, so
	// they must never be served from a cache to a different request.
	CacheControlNoStore = "no-store"

	// CacheControlNoCache permits storage but forces revalidation with
	// the origin before every reuse. Applied to the SPA shell
	// (index.html) so a new deploy — which rewrites index.html to point
	// at freshly content-hashed bundles — is picked up on the next
	// navigation instead of being pinned by a stale cached document.
	CacheControlNoCache = "no-cache"

	// CacheControlImmutable marks a response as safe to cache for a year
	// and never revalidate. Applied to Vite's content-hashed bundles
	// under /assets/: the hash in the filename changes whenever the
	// bytes change, so the URL itself is the cache key and the old URL
	// is simply never requested again after a deploy.
	CacheControlImmutable = "public, max-age=31536000, immutable"
)

// CacheControlRule maps a request path to the Cache-Control value the
// CacheControlMiddleware should set as the default for that path.
//
// A rule matches when the request path is exactly Path (Exact == true)
// or has Path as a prefix (Exact == false). Rules are evaluated in
// order and the first match wins, so more specific prefixes must be
// listed before more general ones.
type CacheControlRule struct {
	// Path is the request path (Exact) or path prefix (prefix match)
	// the rule applies to.
	Path string
	// Exact requires the request path to equal Path. When false the
	// rule matches any path that has Path as a prefix.
	Exact bool
	// Value is the Cache-Control header value set when the rule matches.
	Value string
}

// DefaultCacheControlRules returns the platform's standard CDN/edge
// caching policy, ordered most-specific-first:
//
//   - /assets/* → immutable (Vite content-hashed bundles)
//   - /api/*    → no-store  (dynamic, tenant-scoped, must never cache)
//   - /*        → no-cache  (everything else: the SPA shell at "/",
//     client-routed deep-links like /dashboard, and any other path)
//
// The trailing rule is a prefix match on "/", so it matches every path
// that the earlier, more specific /assets/ and /api/ rules did not.
// That deliberate catch-all is what makes no-cache the origin's answer
// for the SPA shell AND its deep-link routes (e.g. /dashboard), not just
// the exact root: the SPA fallback serves index.html for arbitrary
// paths, so each of them must revalidate to pick up a new deploy. It
// also means this origin policy genuinely mirrors the edge (Caddy's
// @spa matcher), so the no-cache contract holds even if the API is ever
// exposed without Caddy in front. Because earlier rules win, /api keeps
// no-store and /assets keeps immutable — the catch-all never weakens
// them.
func DefaultCacheControlRules() []CacheControlRule {
	return []CacheControlRule{
		{Path: "/assets/", Value: CacheControlImmutable},
		{Path: "/api/", Value: CacheControlNoStore},
		{Path: "/", Value: CacheControlNoCache},
	}
}

// matchCacheControl returns the Cache-Control value for the first rule
// matching path, and whether any rule matched.
func matchCacheControl(rules []CacheControlRule, path string) (string, bool) {
	for _, rule := range rules {
		if rule.Exact {
			if path == rule.Path {
				return rule.Value, true
			}
			continue
		}
		if strings.HasPrefix(path, rule.Path) {
			return rule.Value, true
		}
	}
	return "", false
}

// CacheControlMiddleware sets a default Cache-Control header on every
// matching response so the browser, any CDN, and Caddy at the edge
// agree on cacheability. With no arguments it uses
// DefaultCacheControlRules; callers may pass an explicit rule set
// (e.g. in tests) to override the policy.
//
// The header is written BEFORE next.ServeHTTP so it behaves as a
// default: a handler that needs a different policy (for example the
// marketplace bundle handler, which serves content-addressed,
// immutable downloads, or the SSE stream, which sets no-cache) can
// override it with a later w.Header().Set — the last write before the
// response is flushed wins. This keeps the security-critical default
// (no-store on the tenant-scoped /api/ surface) in force for every
// handler that does not deliberately opt out.
//
// Following the package convention (SecurityHeadersMiddleware,
// RequestIDMiddleware) the constructor returns a
// func(http.Handler) http.Handler so it composes with chi's Use().
func CacheControlMiddleware(rules ...CacheControlRule) func(http.Handler) http.Handler {
	if len(rules) == 0 {
		rules = DefaultCacheControlRules()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v, ok := matchCacheControl(rules, r.URL.Path); ok {
				w.Header().Set(headerCacheControl, v)
			}
			next.ServeHTTP(w, r)
		})
	}
}
