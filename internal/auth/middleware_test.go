package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// stubTenantResolver returns a single fixed tenant for any UUID. Used
// by Middleware tests to flex the active-vs-inactive branches without
// dragging in a database.
type stubTenantResolver struct {
	out *tenant.Tenant
	err error
}

func (s stubTenantResolver) Get(ctx context.Context, id uuid.UUID) (*tenant.Tenant, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.out, nil
}

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	signer, err := NewSigner(SignerConfig{
		Algorithm:  AlgHS256,
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

// TestMiddleware_PlatformAdminBypassesInactiveHomeTenant locks in the
// recovery-scenario fix: a platform admin whose home tenant is
// suspended/archived can still reach the API. Without this exemption,
// the only operator capable of un-suspending a tenant cannot log in
// when their own home tenant gets caught in the suspend cascade.
//
// The fix lives in middleware.go's home-tenant active check; the
// downstream AdminMiddleware re-checks IsPlatformAdmin on each
// request, and SSO refresh re-queries users.is_platform_admin every
// time, so the bypass cannot be replayed past a refresh window by a
// demoted user.
func TestMiddleware_PlatformAdminBypassesInactiveHomeTenant(t *testing.T) {
	signer := newTestSigner(t)
	adminUserID := uuid.New()
	tenantID := uuid.New()

	suspendedHome := &tenant.Tenant{
		ID:     tenantID,
		Status: tenant.StatusSuspended,
	}

	tests := []struct {
		name            string
		isPlatformAdmin bool
		wantStatus      int
	}{
		{
			name:            "non-admin with suspended home tenant returns 403",
			isPlatformAdmin: false,
			wantStatus:      http.StatusForbidden,
		},
		{
			name:            "platform admin with suspended home tenant passes through",
			isPlatformAdmin: true,
			wantStatus:      http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, err := signer.Issue(Claims{
				UserID:          adminUserID,
				TenantID:        tenantID,
				IsPlatformAdmin: tc.isPlatformAdmin,
			})
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			mw := Middleware(signer, stubTenantResolver{out: suspendedHome}, nil)
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestMiddleware_ActiveTenantStillAllowed guards against regression
// in the happy path — the platform-admin bypass should not change
// the behavior for active tenants.
func TestMiddleware_ActiveTenantStillAllowed(t *testing.T) {
	signer := newTestSigner(t)
	tenantID := uuid.New()
	activeHome := &tenant.Tenant{ID: tenantID, Status: tenant.StatusActive}

	token, err := signer.Issue(Claims{
		UserID:   uuid.New(),
		TenantID: tenantID,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	mw := Middleware(signer, stubTenantResolver{out: activeHome}, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestMiddleware_StampsRecoveryBypassFlag locks in the architectural
// narrowing: when the platform-admin bypass triggers, the request
// context MUST carry IsRecoveryBypass=true so downstream middleware
// (RequireActiveHomeTenant) can refuse non-admin tenant-scoped
// routes. The bypass is intentionally narrow — admin recovery routes
// proceed; anything else 403s when composed with the guard.
func TestMiddleware_StampsRecoveryBypassFlag(t *testing.T) {
	signer := newTestSigner(t)
	tenantID := uuid.New()
	adminUserID := uuid.New()
	suspendedHome := &tenant.Tenant{ID: tenantID, Status: tenant.StatusSuspended}
	activeHome := &tenant.Tenant{ID: tenantID, Status: tenant.StatusActive}

	tests := []struct {
		name          string
		home          *tenant.Tenant
		wantBypassSet bool
	}{
		{
			name:          "active home tenant: flag NOT set",
			home:          activeHome,
			wantBypassSet: false,
		},
		{
			name:          "suspended home tenant + platform admin: flag SET",
			home:          suspendedHome,
			wantBypassSet: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, err := signer.Issue(Claims{
				UserID:          adminUserID,
				TenantID:        tenantID,
				IsPlatformAdmin: true,
			})
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			var observed bool
			mw := Middleware(signer, stubTenantResolver{out: tc.home}, nil)
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed = IsRecoveryBypass(r.Context())
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/isolation-audit", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if observed != tc.wantBypassSet {
				t.Fatalf("IsRecoveryBypass = %t, want %t", observed, tc.wantBypassSet)
			}
		})
	}
}

// TestRequireActiveHomeTenant_RefusesBypassedRequests is the
// defense-in-depth guard for non-admin tenant-scoped routes. When
// auth.Middleware admitted the request via the platform-admin
// recovery bypass, this middleware MUST 403 so the recovering admin
// cannot also mutate tenant-scoped data on the suspended tenant.
//
// The admin chain (auth.Middleware + AdminMiddleware) intentionally
// does NOT mount this guard — it is exactly the recovery path the
// bypass exists to enable.
func TestRequireActiveHomeTenant_RefusesBypassedRequests(t *testing.T) {
	tests := []struct {
		name       string
		bypass     bool
		wantStatus int
	}{
		{
			name:       "no bypass: passes through",
			bypass:     false,
			wantStatus: http.StatusOK,
		},
		{
			name:       "bypass set: refused with 403",
			bypass:     true,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			guard := RequireActiveHomeTenant()
			handler := guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/records", nil)
			if tc.bypass {
				ctx := context.WithValue(req.Context(), ctxKeyRecoveryBypass, true)
				req = req.WithContext(ctx)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}
