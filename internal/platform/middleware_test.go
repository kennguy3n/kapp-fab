package platform

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestTenantFromContext_TypedNilReturnsNil pins the architectural contract
// that ClearTenant relies on: when ctxKeyTenant carries a typed-nil
// (*tenant.Tenant)(nil), TenantFromContext must collapse that to an untyped
// nil rather than returning the typed nil interface — otherwise downstream
// `if t == nil` checks would silently pass through and the first field
// access on `t` would nil-deref. The defensive `&& t != nil` branch in
// TenantFromContext is what guarantees this collapse; this test fails if
// that branch is ever removed in a future refactor.
func TestTenantFromContext_TypedNilReturnsNil(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyTenant, (*tenant.Tenant)(nil))
	if got := TenantFromContext(ctx); got != nil {
		t.Fatalf("TenantFromContext on typed-nil ctxKeyTenant = %+v, want nil", got)
	}
}

// TestTenantFromContext_MissingKeyReturnsNil pins the other shape of the
// "no tenant on context" contract: a ctx that never had ctxKeyTenant set
// must also return untyped nil. Together with the typed-nil test above,
// this proves that downstream handlers see one and only one nil shape
// regardless of how the tenant was (or wasn't) cleared upstream.
func TestTenantFromContext_MissingKeyReturnsNil(t *testing.T) {
	if got := TenantFromContext(context.Background()); got != nil {
		t.Fatalf("TenantFromContext on empty ctx = %+v, want nil", got)
	}
}

// TestClearTenant_RemovesUpstreamTenant proves the canonical use case:
// WithTenant stamps a tenant on ctx, ClearTenant scrubs it, and a
// subsequent TenantFromContext call returns nil. This is the runtime
// guarantee that auth.AdminMiddleware relies on to prevent admin handlers
// from silently scoping their work to the admin's home tenant; see the
// long doc block on ClearTenant for the architectural rationale.
func TestClearTenant_RemovesUpstreamTenant(t *testing.T) {
	staged := &tenant.Tenant{
		ID:     uuid.New(),
		Slug:   "admin-home",
		Status: tenant.StatusActive,
	}
	ctx := WithTenant(context.Background(), staged)
	if got := TenantFromContext(ctx); got == nil || got.ID != staged.ID {
		t.Fatalf("precondition: WithTenant did not stamp the tenant; got %+v", got)
	}
	cleared := ClearTenant(ctx)
	if got := TenantFromContext(cleared); got != nil {
		t.Fatalf("TenantFromContext after ClearTenant = %+v, want nil", got)
	}
}
