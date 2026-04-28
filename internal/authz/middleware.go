package authz

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Middleware authorizes the actor on the request context against the
// supplied action + resource. It must be mounted after platform.TenantMiddleware
// because it reads the tenant from the request context.
//
// On success the actor's resolved role list is attached to the context via
// platform.WithUserRoles so downstream handlers (record store field
// filtering, role-aware UI gates) can read it without an extra round trip.
//
// For Phase A the user id comes from an X-User-ID header fallback when no
// context user is present; a later auth middleware will populate the context
// directly from a verified JWT.
func Middleware(eval Evaluator, action, resource string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := platform.TenantFromContext(r.Context())
			if t == nil {
				http.Error(w, "tenant context missing", http.StatusInternalServerError)
				return
			}
			userID := platform.UserIDFromContext(r.Context())
			if userID == uuid.Nil {
				if hdr := r.Header.Get("X-User-ID"); hdr != "" {
					if id, err := uuid.Parse(hdr); err == nil {
						userID = id
					}
				}
			}
			if userID == uuid.Nil {
				http.Error(w, "user context missing", http.StatusUnauthorized)
				return
			}
			if err := eval.Authorize(r.Context(), t.ID, userID, action, resource); err != nil {
				if errors.Is(err, ErrDenied) {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				http.Error(w, "authorization failed", http.StatusInternalServerError)
				return
			}
			ctx := platform.WithUserID(r.Context(), userID)
			if roles, err := eval.ListRoles(ctx, t.ID, userID); err == nil {
				ctx = platform.WithUserRoles(ctx, roles)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MethodMiddleware is a convenience wrapper that picks the action based on
// the HTTP method: read-style methods (GET, HEAD, OPTIONS) use readAction;
// mutation methods (POST, PUT, PATCH, DELETE) use writeAction. Mount this
// on route groups that mix read and write operations under a single
// authorization gate (e.g. /api/v1/records).
func MethodMiddleware(eval Evaluator, readAction, writeAction, resource string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			action := readAction
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				action = writeAction
			}
			Middleware(eval, action, resource)(next).ServeHTTP(w, r)
		})
	}
}
