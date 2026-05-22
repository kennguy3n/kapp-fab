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
	"github.com/kennguy3n/kapp-fab/internal/authz"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// stubChainTenantResolver returns a fixed tenant for any UUID, mirroring
// the helper in me_routes_test.go. Kept local to this test file so the
// two file's stubs can evolve independently if their needs diverge.
type stubChainTenantResolver struct{ out *tenant.Tenant }

func (s stubChainTenantResolver) Get(_ context.Context, _ uuid.UUID) (*tenant.Tenant, error) {
	return s.out, nil
}

// permitEvaluator implements authz.Evaluator and permits every request,
// recording the userID it saw so the test can prove the user_id came
// from the JWT-derived context (set by auth.Middleware) — NOT from any
// X-User-ID header (whose fallback Phase 1 removed).
type permitEvaluator struct{ sawUserID uuid.UUID }

func (e *permitEvaluator) Authorize(_ context.Context, _, userID uuid.UUID, _, _ string) error {
	e.sawUserID = userID
	return nil
}
func (e *permitEvaluator) AuthorizeRecord(_ context.Context, _, userID uuid.UUID, _, _ string, _ map[string]any) error {
	e.sawUserID = userID
	return nil
}
func (e *permitEvaluator) ListPermissions(_ context.Context, _, _ uuid.UUID) ([]authz.Permission, error) {
	return nil, nil
}
func (e *permitEvaluator) ListRoles(_ context.Context, _, _ uuid.UUID) ([]string, error) {
	return nil, nil
}
func (e *permitEvaluator) InvalidateUser(_, _ uuid.UUID) {}
func (e *permitEvaluator) InvalidateTenant(_ uuid.UUID)  {}

func newChainTestSigner(t *testing.T) *auth.Signer {
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

// tenantChainRouter mirrors the production tenantChain composition for
// a representative tenant-scoped data route — auth.Middleware (stamps
// tenant + user_id from JWT) → auth.RequireActiveHomeTenant (rejects
// recovery-bypass requests) → authz.Middleware (gates on RBAC). This
// is exactly what main.go now mounts for /api/v1/records,
// /api/v1/finance, /api/v1/inventory, /api/v1/agents, etc. after the
// Phase 1 migration off platform.TenantMiddleware.
func tenantChainRouter(signer *auth.Signer, resolver auth.TenantResolver, eval authz.Evaluator, handler http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1/records", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(signer, resolver, nil))
			r.Use(auth.RequireActiveHomeTenant())
			r.Use(authz.Middleware(eval, "krecord.read", ""))
			r.Get("/{ktype}", handler)
		})
	})
	return r
}

// TestTenantChain_FullStack_AdmitsValidJWT is the integration test
// for the Phase 1 finding the Devin Review bot caught: with authz
// enforcement defaulted to ON and the X-User-ID fallback removed
// from authz.Middleware, the tenant-scoped routes MUST flow user_id
// through the JWT path (auth.Middleware → tenantChain) — otherwise
// every gated route would 401 because UserIDFromContext returns nil.
//
// The test issues a real signed JWT (HS256), hits a representative
// route under the production chain composition, and asserts:
//   - 200 status (full stack admits the request)
//   - the userID the authz Evaluator was called with matches the
//     JWT-derived user_id, NOT any header value
//   - a spoofed X-User-ID header is ignored
//   - a request without an Authorization header is rejected at the
//     auth.Middleware step (401), never reaching authz
func TestTenantChain_FullStack_AdmitsValidJWT(t *testing.T) {
	signer := newChainTestSigner(t)
	tenantID := uuid.New()
	userID := uuid.New()
	spoofID := uuid.New()
	activeHome := &tenant.Tenant{ID: tenantID, Status: tenant.StatusActive}

	eval := &permitEvaluator{}
	called := false
	r := tenantChainRouter(signer, stubChainTenantResolver{out: activeHome}, eval, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	token, err := signer.Issue(auth.Claims{
		UserID:   userID,
		TenantID: tenantID,
		Audience: "kapp-test",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// 1) Valid JWT only — happy path.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/records/items", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("happy path status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("handler not invoked under happy path")
	}
	if eval.sawUserID != userID {
		t.Fatalf("authz saw userID = %s, want %s (JWT-derived)", eval.sawUserID, userID)
	}

	// 2) Valid JWT + spoofed X-User-ID header — header must be ignored.
	called = false
	eval.sawUserID = uuid.Nil
	req = httptest.NewRequest(http.MethodGet, "/api/v1/records/items", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-ID", spoofID.String())
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("spoof-with-JWT status = %d, want 200", rec.Code)
	}
	if eval.sawUserID != userID {
		t.Fatalf("authz saw spoofed userID = %s, want JWT-derived %s — X-User-ID header was honored", eval.sawUserID, userID)
	}

	// 3) No Authorization header at all — 401, must not reach authz.
	called = false
	eval.sawUserID = uuid.Nil
	req = httptest.NewRequest(http.MethodGet, "/api/v1/records/items", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-JWT status = %d, want 401", rec.Code)
	}
	if eval.sawUserID != uuid.Nil {
		t.Fatal("authz Evaluator was called on an unauthenticated request")
	}
	if called {
		t.Fatal("handler was invoked on an unauthenticated request")
	}

	// 4) No Authorization header but X-User-ID set — must still 401.
	// This is the Phase-1 fix proper: the header-only path is dead.
	called = false
	eval.sawUserID = uuid.Nil
	req = httptest.NewRequest(http.MethodGet, "/api/v1/records/items", nil)
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("X-Tenant-ID", tenantID.String())
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("header-only status = %d, want 401 — pre-Phase-1 header path must be closed", rec.Code)
	}
	if eval.sawUserID != uuid.Nil {
		t.Fatal("authz Evaluator was called on a header-only request — the X-User-ID fallback was reachable")
	}
}

// TestTenantChain_RejectsSuspendedHomeTenant pins the
// RequireActiveHomeTenant gate that tenantChain mounts. A user whose
// home tenant has been suspended must not be able to mutate that
// tenant's data via any tenant-scoped route (the recovery-bypass path
// is reserved for adminChain).
func TestTenantChain_RejectsSuspendedHomeTenant(t *testing.T) {
	signer := newChainTestSigner(t)
	tenantID := uuid.New()
	userID := uuid.New()
	suspended := &tenant.Tenant{ID: tenantID, Status: tenant.StatusSuspended}

	eval := &permitEvaluator{}
	called := false
	r := tenantChainRouter(signer, stubChainTenantResolver{out: suspended}, eval, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	token, err := signer.Issue(auth.Claims{
		UserID:   userID,
		TenantID: tenantID,
		Audience: "kapp-test",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records/items", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("suspended-home status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("handler invoked on a suspended-home request")
	}
	if eval.sawUserID != uuid.Nil {
		t.Fatal("authz Evaluator was called for a request blocked at RequireActiveHomeTenant")
	}
}
