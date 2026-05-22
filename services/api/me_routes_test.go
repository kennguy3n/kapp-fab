package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// stubTenantResolver returns a fixed tenant regardless of the supplied
// UUID. Mirrors the helper in internal/auth/middleware_test.go without
// re-exporting it — keeps the cross-package coupling minimal.
type stubMeTenantResolver struct {
	out *tenant.Tenant
}

func (s stubMeTenantResolver) Get(ctx context.Context, id uuid.UUID) (*tenant.Tenant, error) {
	return s.out, nil
}

func newMeTestSigner(t *testing.T) *auth.Signer {
	t.Helper()
	signer, err := auth.NewSigner(auth.SignerConfig{
		Algorithm:  auth.AlgHS256,
		HMACKey:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer:     "kapp-test",
		Audience:   "kapp-test",
		AccessTTL:  5 * time.Minute,
		RefreshTTL: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

// meRouter mirrors the production tenantChain composition for the
// /api/v1/tenants/me sub-tree: auth.Middleware → auth.RequireActiveHomeTenant
// → handler. The handler is a stub that just emits the tenant UUID it
// resolved from the request context — that is what production handlers
// (changePlanMe, listMe, usageMe) read via tenantFromCtx /
// platform.TenantFromContext.
func meRouter(signer *auth.Signer, resolver auth.TenantResolver, handler http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1/tenants", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(signer, resolver, nil))
			r.Use(auth.RequireActiveHomeTenant())
			r.Route("/me", func(r chi.Router) {
				r.Get("/features", handler)
				r.Post("/plan", handler)
			})
		})
	})
	return r
}

// TestMeRoutes_RequireJWT locks in the Phase 1 fix for the
// X-Tenant-ID header-only privilege escalation. Before Phase 1, /me
// routes used platform.TenantMiddleware (which honored the
// X-Tenant-ID header) so any caller could send
//
//	POST /api/v1/tenants/me/plan
//	X-Tenant-ID: <victim-uuid>
//	{"plan": "free"}
//
// and downgrade another tenant's plan — changePlan reads the tenant
// from URL params (populated from header-derived ctx by changePlanMe)
// with no user-identity check of its own. tenantChain replaces the
// header path with auth.Middleware so the only tenant a caller can
// act on via /me is the one their JWT claims name.
func TestMeRoutes_RequireJWT(t *testing.T) {
	signer := newMeTestSigner(t)
	tenantID := uuid.New()
	activeHome := &tenant.Tenant{ID: tenantID, Status: tenant.StatusActive}

	tests := []struct {
		name       string
		mutate     func(r *http.Request)
		wantStatus int
		wantBody   string
	}{
		{
			name: "no Authorization header: 401",
			mutate: func(r *http.Request) {
				// no header set
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "X-Tenant-ID header alone without JWT: 401",
			mutate: func(r *http.Request) {
				// Pre-Phase-1 path: header only. Must be refused
				// outright by tenantChain — there is no fallback.
				r.Header.Set("X-Tenant-ID", uuid.NewString())
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "garbage Authorization header: 401",
			mutate: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer not-a-real-token")
			},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := meRouter(signer, stubMeTenantResolver{out: activeHome}, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/me/features", nil)
			tc.mutate(req)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestMeRoutes_TenantComesFromJWTNotHeader locks in the architectural
// guarantee that the tenant the handler operates on is derived from
// the JWT claim — NOT the X-Tenant-ID request header. Even when a
// caller sends an X-Tenant-ID header naming a different tenant, the
// handler MUST see the JWT's tenant.
func TestMeRoutes_TenantComesFromJWTNotHeader(t *testing.T) {
	signer := newMeTestSigner(t)
	jwtTenant := uuid.New()
	victimTenant := uuid.New()
	activeHome := &tenant.Tenant{ID: jwtTenant, Status: tenant.StatusActive}

	token, err := signer.Issue(auth.Claims{
		UserID:   uuid.New(),
		TenantID: jwtTenant,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var observedTenant uuid.UUID
	r := meRouter(signer, stubMeTenantResolver{out: activeHome}, func(w http.ResponseWriter, req *http.Request) {
		if t := tenantFromCtx(req); t != nil {
			observedTenant = t.ID
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/me/features", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	// Adversarial header — tenantChain must ignore it.
	req.Header.Set("X-Tenant-ID", victimTenant.String())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if observedTenant != jwtTenant {
		t.Fatalf("handler saw tenant=%s, want %s (header tried to override to %s)", observedTenant, jwtTenant, victimTenant)
	}
}

// TestMeRoutes_RecoveryBypassedAdminCannotMutateViaMe locks in the
// defense-in-depth narrowing from finding #3285349302: a platform
// admin admitted via the home-tenant recovery bypass MUST still be
// refused on /me — they can recover the tenant via the admin chain,
// but they cannot also downgrade their own plan or mutate features
// on the inactive tenant via /me. The admin chain intentionally does
// NOT mount RequireActiveHomeTenant; tenantChain does, which is what
// produces the 403 here.
func TestMeRoutes_RecoveryBypassedAdminCannotMutateViaMe(t *testing.T) {
	signer := newMeTestSigner(t)
	tenantID := uuid.New()
	suspendedHome := &tenant.Tenant{ID: tenantID, Status: tenant.StatusSuspended}

	token, err := signer.Issue(auth.Claims{
		UserID:          uuid.New(),
		TenantID:        tenantID,
		IsPlatformAdmin: true,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	r := meRouter(signer, stubMeTenantResolver{out: suspendedHome}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/me/plan", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%q) — recovery bypass must NOT let an admin mutate via /me", rec.Code, rec.Body.String())
	}
}
