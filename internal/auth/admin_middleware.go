package auth

import (
	"net/http"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// AdminMiddleware gates the wrapped handler behind the
// IsPlatformAdmin JWT claim. It MUST be mounted after Middleware so a
// verified Claims object is available on the context — without the
// claims it returns 401, with claims missing the flag it returns 403.
//
// The middleware is the single source of truth for "platform admin"
// — there is no env-var override, no header-based promotion, and no
// per-tenant role that satisfies it. The claim is set at JWT
// issuance time (see SSOService.Exchange) and cannot be replayed
// across users because the signature covers it.
//
// On success, the middleware ALSO scrubs the tenant attached to the
// request context by the upstream auth.Middleware. The rationale:
// auth.Middleware stamps the JWT's `tid` claim (the admin's HOME
// tenant) on ctx so ordinary tenant-scoped routes can call
// platform.TenantFromContext. Control-plane routes mounted under
// adminChain operate on a DIFFERENT tenant — the one named in the
// URL (e.g. /api/v1/tenants/{id}). If a control-plane handler ever
// fell back to TenantFromContext (intentionally or by absent-minded
// reuse of a shared helper) it would silently scope its work to the
// admin's own tenant rather than the URL target. RLS would not catch
// this because the admin's own row IS visible to itself.
//
// Scrubbing here converts that footgun from "documented invariant"
// into "runtime guarantee": admin handlers MUST resolve the target
// tenant explicitly (chi.URLParam / dbutil.WithTenantTx with an
// explicit tenant ID); a call to platform.TenantFromContext returns
// nil and the handler must handle that branch explicitly. The
// admin's home tenant is still recoverable from the JWT claims
// (auth.ClaimsFromContext) if a handler genuinely needs it.
func AdminMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := ClaimsFromContext(r.Context())
			if claims == nil {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			if !claims.IsPlatformAdmin {
				http.Error(w, "forbidden: platform admin required", http.StatusForbidden)
				return
			}
			ctx := platform.ClearTenant(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
