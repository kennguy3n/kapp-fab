//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestLeaveBalancesViewSecurityInvoker is the regression guard for the
// leave_balances half of migration 000096. The view rolls up
// SUM(delta_days) from the RLS-protected leave_ledger; before 000096 it
// ran as its owner kapp (which also owns leave_ledger, and the table is
// not FORCE ROW LEVEL SECURITY) and bypassed tenant_isolation. With
// security_invoker = true the policy is a hard backstop: no app.tenant_id
// GUC -> zero rows; GUC set to tenant A -> only A's balances.
func TestLeaveBalancesViewSecurityInvoker(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping leave_balances RLS test")
	}
	ctx := context.Background()

	tnA := mustTenant(t, ctx, h, "lb-a")
	tnB := mustTenant(t, ctx, h, "lb-b")

	empA, empB := uuid.New(), uuid.New()
	for _, r := range []struct {
		tenant, emp uuid.UUID
	}{{tnA.ID, empA}, {tnB.ID, empB}} {
		if _, err := h.adminPool.Exec(ctx,
			`INSERT INTO leave_ledger
			   (tenant_id, employee_id, leave_type, delta_days, effective_on, created_by)
			 VALUES ($1, $2, 'annual', 5, current_date, $2)`,
			r.tenant, r.emp); err != nil {
			t.Fatalf("seed leave_ledger for %s: %v", r.tenant, err)
		}
	}

	// kapp_app with no tenant context must see zero rows (default-deny).
	var leaked int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM leave_balances WHERE employee_id IN ($1, $2)`,
		empA, empB).Scan(&leaked); err != nil {
		t.Fatalf("query leave_balances without GUC: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("leave_balances leaked %d rows to kapp_app with no tenant context; "+
			"the view is not security_invoker (RLS bypassed)", leaked)
	}

	// With the GUC bound to tenant A, only A's balance is visible.
	var mine, cross int
	if err := dbutil.WithTenantTx(ctx, h.pool, tnA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE tenant_id = $1),
			        count(*) FILTER (WHERE tenant_id <> $1)
			   FROM leave_balances WHERE employee_id IN ($2, $3)`,
			tnA.ID, empA, empB).Scan(&mine, &cross)
	}); err != nil {
		t.Fatalf("query leave_balances for tenant A: %v", err)
	}
	if mine != 1 {
		t.Fatalf("tenant A sees %d of its own leave_balances rows, want 1", mine)
	}
	if cross != 0 {
		t.Fatalf("tenant A sees %d of tenant B's leave_balances rows, want 0 (cross-tenant leak)", cross)
	}
}

// TestEnrollmentProgressViewSecurityInvoker is the regression guard for
// the enrollment_progress half of migration 000096. Same defect class as
// leave_balances/stock_levels: a view over the RLS-protected
// lesson_progress table that, pre-000096, ran as owner kapp and bypassed
// tenant_isolation.
func TestEnrollmentProgressViewSecurityInvoker(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping enrollment_progress RLS test")
	}
	ctx := context.Background()

	tnA := mustTenant(t, ctx, h, "ep-a")
	tnB := mustTenant(t, ctx, h, "ep-b")

	enrA, enrB := uuid.New(), uuid.New()
	for _, r := range []struct {
		tenant, enr uuid.UUID
	}{{tnA.ID, enrA}, {tnB.ID, enrB}} {
		if _, err := h.adminPool.Exec(ctx,
			`INSERT INTO lesson_progress
			   (tenant_id, enrollment_id, lesson_id, status)
			 VALUES ($1, $2, $3, 'completed')`,
			r.tenant, r.enr, uuid.New()); err != nil {
			t.Fatalf("seed lesson_progress for %s: %v", r.tenant, err)
		}
	}

	var leaked int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM enrollment_progress WHERE enrollment_id IN ($1, $2)`,
		enrA, enrB).Scan(&leaked); err != nil {
		t.Fatalf("query enrollment_progress without GUC: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("enrollment_progress leaked %d rows to kapp_app with no tenant context; "+
			"the view is not security_invoker (RLS bypassed)", leaked)
	}

	var mine, cross int
	if err := dbutil.WithTenantTx(ctx, h.pool, tnA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE tenant_id = $1),
			        count(*) FILTER (WHERE tenant_id <> $1)
			   FROM enrollment_progress WHERE enrollment_id IN ($2, $3)`,
			tnA.ID, enrA, enrB).Scan(&mine, &cross)
	}); err != nil {
		t.Fatalf("query enrollment_progress for tenant A: %v", err)
	}
	if mine != 1 {
		t.Fatalf("tenant A sees %d of its own enrollment_progress rows, want 1", mine)
	}
	if cross != 0 {
		t.Fatalf("tenant A sees %d of tenant B's enrollment_progress rows, want 0 (cross-tenant leak)", cross)
	}
}

func mustTenant(t *testing.T, ctx context.Context, h *harness, prefix string) *tenant.Tenant {
	t.Helper()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug(prefix), Name: prefix, Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("create tenant %s: %v", prefix, err)
	}
	return tn
}
