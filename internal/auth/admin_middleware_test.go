package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
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
