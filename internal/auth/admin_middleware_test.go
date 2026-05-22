package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

func TestAdminMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		wantStatus int
		wantBody   string
	}{
		{
			name:       "no claims on context returns 401",
			ctx:        context.Background(),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "non-admin claims returns 403",
			ctx: context.WithValue(context.Background(), ctxKeyClaims, &Claims{
				UserID:          uuid.New(),
				TenantID:        uuid.New(),
				IsPlatformAdmin: false,
			}),
			wantStatus: http.StatusForbidden,
		},
		{
			name: "admin claims pass through",
			ctx: context.WithValue(context.Background(), ctxKeyClaims, &Claims{
				UserID:          uuid.New(),
				TenantID:        uuid.New(),
				IsPlatformAdmin: true,
			}),
			wantStatus: http.StatusOK,
			wantBody:   "ok",
		},
	}
	mw := AdminMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants", nil).WithContext(tc.ctx)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestAdminMiddleware_ScrubsTenantContext verifies the runtime-enforced
// invariant that AdminMiddleware clears the tenant a previous middleware
// stamped on the context. Without the scrub, control-plane handlers
// could silently scope operations to the admin's home tenant when they
// should be targeting the URL-supplied tenant — see the long coupling
// note in services/api/deps_build.go::adminChain and the docstring on
// platform.ClearTenant for the architectural rationale.
func TestAdminMiddleware_ScrubsTenantContext(t *testing.T) {
	adminHomeTenant := &tenant.Tenant{
		ID:     uuid.New(),
		Slug:   "admin-home",
		Status: tenant.StatusActive,
	}
	ctx := context.WithValue(context.Background(), ctxKeyClaims, &Claims{
		UserID:          uuid.New(),
		TenantID:        adminHomeTenant.ID,
		IsPlatformAdmin: true,
	})
	ctx = platform.WithTenant(ctx, adminHomeTenant)

	if got := platform.TenantFromContext(ctx); got == nil || got.ID != adminHomeTenant.ID {
		t.Fatalf("precondition: expected admin's home tenant on context, got %+v", got)
	}

	var observed *tenant.Tenant
	mw := AdminMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = platform.TenantFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/some-other-id/suspend", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if observed != nil {
		t.Fatalf("TenantFromContext after AdminMiddleware = %+v, want nil (scrubbed)", observed)
	}
}
