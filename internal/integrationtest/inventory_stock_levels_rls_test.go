//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestStockLevelsViewSecurityInvoker is the regression guard for
// migration 000095: the `stock_levels` view must run with
// security_invoker = true so it scans the append-only inventory_moves
// ledger as the *invoking* role (kapp_app) and is therefore subject to
// the inventory_moves tenant_isolation RLS policy.
//
// Before 000095 the view ran as its owner `kapp` (which also owns
// inventory_moves and is exempt from RLS since the table is not FORCE
// ROW LEVEL SECURITY), so querying the view as kapp_app bypassed RLS
// and exposed every tenant's stock. This test asserts:
//   - querying the view as kapp_app with NO app.tenant_id GUC returns
//     zero rows (default-deny), proving the policy is now enforced; and
//   - with the GUC set to tenant A, only A's positions are visible —
//     none of tenant B's leak through.
func TestStockLevelsViewSecurityInvoker(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping stock_levels RLS test")
	}
	if os.Getenv("KAPP_TEST_ADMIN_DB_URL") == "" {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set")
	}
	ctx := context.Background()

	tnA, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("si-a"), Name: "Stock A", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	tnB, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("si-b"), Name: "Stock B", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}

	// Seed one move per tenant via the BYPASSRLS admin pool (it can
	// write any tenant_id). Distinct (item, warehouse) keys so each
	// tenant has exactly one stock_levels row.
	itemA, whA := uuid.New(), uuid.New()
	itemB, whB := uuid.New(), uuid.New()
	for _, m := range []struct {
		tenant, item, wh uuid.UUID
		qty              string
	}{
		{tnA.ID, itemA, whA, "7"},
		{tnB.ID, itemB, whB, "9"},
	} {
		if _, err := h.adminPool.Exec(ctx,
			`INSERT INTO inventory_moves (tenant_id, item_id, warehouse_id, qty)
			 VALUES ($1, $2, $3, $4::numeric)`,
			m.tenant, m.item, m.wh, m.qty); err != nil {
			t.Fatalf("seed move for %s: %v", m.tenant, err)
		}
	}

	// As kapp_app with NO app.tenant_id GUC set, the RLS policy on
	// inventory_moves defaults to deny — the security_invoker view must
	// therefore expose zero rows. (A non-security_invoker view would run
	// as the owner and leak every tenant's stock here.)
	var leakedAll int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM stock_levels
		  WHERE (item_id = $1 AND warehouse_id = $2)
		     OR (item_id = $3 AND warehouse_id = $4)`,
		itemA, whA, itemB, whB).Scan(&leakedAll); err != nil {
		t.Fatalf("query stock_levels without GUC: %v", err)
	}
	if leakedAll != 0 {
		t.Fatalf("stock_levels leaked %d rows to kapp_app with no tenant context; "+
			"the view is not security_invoker (RLS bypassed)", leakedAll)
	}

	// With the GUC bound to tenant A, only A's position is visible and
	// none of B's leaks through.
	var mine, leaked int
	err = dbutil.WithTenantTx(ctx, h.pool, tnA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE tenant_id = $1),
			        count(*) FILTER (WHERE tenant_id <> $1)
			   FROM stock_levels
			  WHERE (item_id = $2 AND warehouse_id = $3)
			     OR (item_id = $4 AND warehouse_id = $5)`,
			tnA.ID, itemA, whA, itemB, whB).Scan(&mine, &leaked)
	})
	if err != nil {
		t.Fatalf("query stock_levels for tenant A: %v", err)
	}
	if mine != 1 {
		t.Fatalf("tenant A sees %d of its own stock_levels rows, want 1", mine)
	}
	if leaked != 0 {
		t.Fatalf("tenant A sees %d of tenant B's stock_levels rows, want 0 (cross-tenant leak)", leaked)
	}
}
