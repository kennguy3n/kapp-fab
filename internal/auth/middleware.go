package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// ctxKey avoids string-collision on the request context. Exported
// helpers (ClaimsFromContext) are the blessed accessors.
type ctxKey int

const (
	ctxKeyClaims ctxKey = iota
)

// Middleware validates a Bearer JWT against the supplied Signer and
// stores the decoded Claims on the request context. It ALSO fulfils
// the contract platform.TenantMiddleware used to provide — when the
// tenant lookup succeeds, the active tenant is placed on the context
// so downstream handlers can call platform.TenantFromContext and
// platform.UserIDFromContext without a second round-trip.
//
// Sessions (when configured) are revalidated on every request: a
// revoked or expired row fails the request with 401 even if the JWT
// itself has not expired yet. This lets operators force-logout a user
// by deleting their session rows without waiting for JWT TTL.
func Middleware(
	signer *Signer,
	tenantSvc TenantResolver,
	sessions SessionStore,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, err := extractBearer(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			claims, err := signer.Verify(tok)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			if sessions != nil && claims.SessionID != uuid.Nil {
				if _, err := sessions.Get(r.Context(), claims.TenantID, claims.SessionID); err != nil {
					if errors.Is(err, ErrSessionNotFound) {
						http.Error(w, "session revoked", http.StatusUnauthorized)
						return
					}
					http.Error(w, "session lookup failed", http.StatusInternalServerError)
					return
				}
			}
			t, err := tenantSvc.Get(r.Context(), claims.TenantID)
			if err != nil {
				if errors.Is(err, tenant.ErrNotFound) {
					http.Error(w, "tenant not found", http.StatusNotFound)
					return
				}
				http.Error(w, "tenant lookup failed", http.StatusInternalServerError)
				return
			}
			if t.Status != tenant.StatusActive {
				http.Error(w, "tenant is not active", http.StatusForbidden)
				return
			}
			ctx := r.Context()
			ctx = platform.WithTenant(ctx, t)
			ctx = platform.WithUserID(ctx, claims.UserID)
			ctx = context.WithValue(ctx, ctxKeyClaims, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TenantResolver is the narrow slice of tenant.Service the middleware
// needs. Kept here rather than importing tenant.Service directly so
// the package can also accept mocks.
type TenantResolver interface {
	Get(ctx context.Context, id uuid.UUID) (*tenant.Tenant, error)
}

// ClaimsFromContext returns the verified JWT claims stored on the
// request context by Middleware, or nil when the request never
// traversed the middleware.
func ClaimsFromContext(ctx context.Context) *Claims {
	if c, ok := ctx.Value(ctxKeyClaims).(*Claims); ok {
		return c
	}
	return nil
}

func extractBearer(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", errors.New("missing Authorization header")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", errors.New("authorization must be Bearer")
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if tok == "" {
		return "", errors.New("authorization Bearer empty")
	}
	return tok, nil
}
