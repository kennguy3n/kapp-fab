package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// recordingEvaluator captures the inputs to Authorize so the tests
// can assert which user id the middleware passes through. A nil
// returns from Authorize means "allowed".
//
// This is the minimum surface needed to exercise the middleware —
// the production Evaluator (PostgreSQL-backed) is covered by its
// own integration tests and is not the subject of these unit tests.
type recordingEvaluator struct {
	calledUser uuid.UUID
	err        error
	roles      []string
}

func (e *recordingEvaluator) Authorize(_ context.Context, _, userID uuid.UUID, _, _ string) error {
	e.calledUser = userID
	return e.err
}

func (e *recordingEvaluator) AuthorizeRecord(_ context.Context, _, userID uuid.UUID, _, _ string, _ map[string]any) error {
	e.calledUser = userID
	return e.err
}

func (e *recordingEvaluator) ListPermissions(_ context.Context, _, _ uuid.UUID) ([]Permission, error) {
	return nil, nil
}

func (e *recordingEvaluator) ListRoles(_ context.Context, _, _ uuid.UUID) ([]string, error) {
	return e.roles, nil
}

func (e *recordingEvaluator) InvalidateUser(_, _ uuid.UUID) {}

func (e *recordingEvaluator) InvalidateTenant(_ uuid.UUID) {}

func newRequestWithCtx(method string, headers map[string]string, tenantOnCtx *tenant.Tenant, jwtUserID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, "/api/v1/records/items", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx := req.Context()
	if tenantOnCtx != nil {
		ctx = platform.WithTenant(ctx, tenantOnCtx)
	}
	if jwtUserID != uuid.Nil {
		ctx = platform.WithUserID(ctx, jwtUserID)
	}
	return req.WithContext(ctx)
}

// TestMiddleware_RejectsWithoutUserContext verifies that the
// X-User-ID header fallback removed in Phase 1.2 is actually gone:
// a request that supplies the header but no JWT-derived user on the
// context must be rejected with 401, not silently impersonate the
// header value.
func TestMiddleware_RejectsWithoutUserContext(t *testing.T) {
	eval := &recordingEvaluator{}
	mw := Middleware(eval, "records.read", "records")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not be called when user context is missing")
		w.WriteHeader(http.StatusOK)
	}))

	tnt := &tenant.Tenant{ID: uuid.New(), Slug: "acme", Status: "active"}
	headerUserID := uuid.New().String()
	req := newRequestWithCtx(http.MethodGet, map[string]string{"X-User-ID": headerUserID}, tnt, uuid.Nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%q)", rec.Code, rec.Body.String())
	}
	if eval.calledUser != uuid.Nil {
		t.Fatalf("Evaluator.Authorize was called with userID=%s; X-User-ID fallback is NOT removed", eval.calledUser)
	}
}

// TestMiddleware_UsesJWTUserIgnoresHeader confirms the user id passed
// to the evaluator is the JWT-derived one, even when an X-User-ID
// header is also present. The header must never override the JWT.
func TestMiddleware_UsesJWTUserIgnoresHeader(t *testing.T) {
	eval := &recordingEvaluator{}
	mw := Middleware(eval, "records.read", "records")
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	tnt := &tenant.Tenant{ID: uuid.New(), Slug: "acme", Status: "active"}
	jwtUserID := uuid.New()
	spoofedUserID := uuid.New()
	req := newRequestWithCtx(http.MethodGet, map[string]string{"X-User-ID": spoofedUserID.String()}, tnt, jwtUserID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("handler not invoked, body=%q status=%d", rec.Body.String(), rec.Code)
	}
	if eval.calledUser != jwtUserID {
		t.Fatalf("Evaluator received userID=%s, want JWT userID=%s (spoof header was %s)",
			eval.calledUser, jwtUserID, spoofedUserID)
	}
}

// TestMiddleware_TenantMissingReturns500 keeps the existing contract:
// missing tenant on context is an internal misconfiguration, not an
// auth failure.
func TestMiddleware_TenantMissingReturns500(t *testing.T) {
	eval := &recordingEvaluator{}
	mw := Middleware(eval, "records.read", "records")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := newRequestWithCtx(http.MethodGet, nil, nil, uuid.New())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
