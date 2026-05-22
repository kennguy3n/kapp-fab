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
