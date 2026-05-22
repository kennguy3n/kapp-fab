package auth

import (
	"net/http"
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
			next.ServeHTTP(w, r)
		})
	}
}
