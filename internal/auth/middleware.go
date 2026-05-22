package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// ctxKey avoids string-collision on the request context. Exported
// helpers (ClaimsFromContext, IsRecoveryBypass) are the blessed
// accessors.
type ctxKey int

const (
	ctxKeyClaims ctxKey = iota
	ctxKeyRecoveryBypass
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
			recoveryBypass := false
			if t.Status != tenant.StatusActive {
				// Platform admins are exempt from the home-tenant
				// active check. Without this exemption, a platform
				// admin whose only tenant membership is in a
				// suspended/archived tenant cannot reach ANY route —
				// including the very admin routes that would let
				// them re-activate a tenant. That is the locked-out-
				// last-admin scenario operators hit during recovery
				// (e.g., a billing-driven suspend cascades to the
				// admin's home tenant, or a misclick archives the
				// wrong one). Admin authorization itself is enforced
				// downstream by AdminMiddleware against the
				// IsPlatformAdmin claim, which is re-queried from
				// users.is_platform_admin on every SSO refresh, so a
				// demoted user does not retain this bypass past the
				// refresh window.
				//
				// The bypass is intentionally narrow: we proceed,
				// but we ALSO stamp ctxKeyRecoveryBypass on the
				// context so non-admin tenant-scoped routes can
				// refuse the request via RequireActiveHomeTenant.
				// The admin chain intentionally does NOT mount that
				// guard — it is exactly the recovery path the
				// bypass exists to enable. Any handler that runs
				// under auth.Middleware OUTSIDE the admin chain
				// MUST mount RequireActiveHomeTenant so an admin
				// recovering an inactive home tenant cannot also
				// mutate tenant-scoped data on that tenant via the
				// same bypass.
				//
				// We still emit a WARN so the operator audit log
				// makes the unusual login visible — this path is
				// expected to be rare and a sustained pattern is a
				// sign the actual tenant lifecycle is broken.
				if !claims.IsPlatformAdmin {
					http.Error(w, "tenant is not active", http.StatusForbidden)
					return
				}
				log.Printf("auth: WARN platform admin user=%s logged in via inactive home tenant=%s status=%s; allowing for recovery", claims.UserID, t.ID, t.Status)
				recoveryBypass = true
			}
			ctx := r.Context()
			ctx = platform.WithTenant(ctx, t)
			ctx = platform.WithUserID(ctx, claims.UserID)
			ctx = context.WithValue(ctx, ctxKeyClaims, claims)
			if recoveryBypass {
				ctx = context.WithValue(ctx, ctxKeyRecoveryBypass, true)
			}
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

// IsRecoveryBypass reports whether the supplied request context was
// admitted via the platform-admin recovery bypass in Middleware — i.e.
// the home tenant was suspended/archived but the user is a platform
// admin so the auth gate let the request proceed for the locked-out-
// last-admin scenario.
//
// Tenant-scoped routes that mount auth.Middleware outside the admin
// chain should consult this (via the RequireActiveHomeTenant middleware
// helper) so a recovering admin can reach the admin re-activation
// endpoints but cannot also mutate tenant-scoped data on the same
// inactive tenant via the bypass.
func IsRecoveryBypass(ctx context.Context) bool {
	if v, ok := ctx.Value(ctxKeyRecoveryBypass).(bool); ok {
		return v
	}
	return false
}

// RequireActiveHomeTenant refuses requests admitted via the
// platform-admin recovery bypass. Mount AFTER Middleware on any
// chain that is NOT the admin-recovery path.
//
// The admin chain (auth.Middleware + auth.AdminMiddleware) intentionally
// omits this guard — the whole point of the recovery bypass is to let
// a platform admin reach the admin endpoints that re-activate a
// suspended home tenant. Every OTHER chain that mounts auth.Middleware
// (regular user routes, /me routes, future tenant-scoped routes that
// want JWT auth) must mount this so a recovering admin cannot also
// perform tenant-scoped mutations on the suspended tenant via the
// same bypass.
func RequireActiveHomeTenant() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsRecoveryBypass(r.Context()) {
				http.Error(w, "home tenant is not active", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
