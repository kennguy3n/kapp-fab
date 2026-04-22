package platform

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// ctxKey is an unexported type for context keys defined by this package to
// avoid collisions with keys defined elsewhere.
type ctxKey int

const (
	ctxKeyTenant ctxKey = iota
	ctxKeyUser
)

// TenantMiddleware extracts the tenant identifier from the X-Tenant-ID header
// (either a UUID or a slug), looks it up via the Service, verifies that the
// tenant is active, and stores it on the request context for downstream
// handlers. Inactive tenants (suspended/archived/deleting) are rejected with
// 403 Forbidden.
//
// This middleware is intentionally small and header-driven for Phase A. Once
// JWT auth lands, the tenant claim will move into the token and this middleware
// will decode it from there instead.
func TenantMiddleware(svc TenantLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("X-Tenant-ID")
			if raw == "" {
				http.Error(w, "X-Tenant-ID header required", http.StatusBadRequest)
				return
			}

			ctx := r.Context()
			var (
				t   *tenant.Tenant
				err error
			)
			if id, perr := uuid.Parse(raw); perr == nil {
				t, err = svc.Get(ctx, id)
			} else {
				t, err = svc.GetBySlug(ctx, raw)
			}
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

			ctx = context.WithValue(ctx, ctxKeyTenant, t)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TenantLookup is the subset of tenant.Service the middleware needs. Keeping
// it narrow avoids a direct import cycle if Service grows new methods that the
// middleware should not depend on.
type TenantLookup interface {
	Get(ctx context.Context, id uuid.UUID) (*tenant.Tenant, error)
	GetBySlug(ctx context.Context, slug string) (*tenant.Tenant, error)
}

// TenantFromContext returns the tenant stored on the request context by
// TenantMiddleware, or nil if the context has no tenant.
func TenantFromContext(ctx context.Context) *tenant.Tenant {
	if t, ok := ctx.Value(ctxKeyTenant).(*tenant.Tenant); ok {
		return t
	}
	return nil
}

// WithTenant returns a new context carrying the supplied tenant. Useful for
// tests and for propagating the tenant from the HTTP request context into
// background goroutines or transaction helpers.
func WithTenant(ctx context.Context, t *tenant.Tenant) context.Context {
	return context.WithValue(ctx, ctxKeyTenant, t)
}

// UserIDFromContext returns the user id stored on the request context, or
// uuid.Nil if none is present.
func UserIDFromContext(ctx context.Context) uuid.UUID {
	if id, ok := ctx.Value(ctxKeyUser).(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}

// WithUserID returns a new context carrying the supplied user id.
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyUser, id)
}
