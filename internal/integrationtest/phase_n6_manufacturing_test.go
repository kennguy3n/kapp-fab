//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/manufacturing"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// newTenantForManufacturing builds a fresh tenant with the
// inventory + manufacturing KTypes registered, a finished-good item,
// two component items, a warehouse, and the manufacturing store
// wired against the same inventory store the work-order completion
// engine will emit moves through. Mirrors newTenantForInventory.
func newTenantForManufacturing(t *testing.T, h *harness) (
	*tenant.Tenant,
	*inventory.PGStore,
	*manufacturing.PGStore,
	inventory.Item, // finished good
	inventory.Item, // component A
	inventory.Item, // component B
	inventory.Warehouse,
) {
	t.Helper()
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phasen6"), Name: "Phase N6 Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := finance.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register finance ktypes: %v", err)
	}
	if err := inventory.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register inventory ktypes: %v", err)
	}
	if err := manufacturing.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register manufacturing ktypes: %v", err)
	}

	invStore := inventory.NewPGStore(h.pool, h.publisher, h.auditor)
	mfgStore := manufacturing.NewPGStore(h.pool, invStore)

	fg, err := invStore.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "SKU-FG-WIDGET", Name: "Finished Widget", UOM: "each", Active: true,
	})
	if err != nil {
		t.Fatalf("seed finished good: %v", err)
	}
	compA, err := invStore.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "SKU-COMP-A", Name: "Component A", UOM: "each", Active: true,
	})
	if err != nil {
		t.Fatalf("seed component A: %v", err)
	}
	compB, err := invStore.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "SKU-COMP-B", Name: "Component B", UOM: "each", Active: true,
	})
	if err != nil {
		t.Fatalf("seed component B: %v", err)
	}
	wh, err := invStore.UpsertWarehouse(ctx, inventory.Warehouse{
		TenantID: tn.ID, Code: "WH-MFG", Name: "Manufacturing Warehouse",
	})
	if err != nil {
		t.Fatalf("seed warehouse: %v", err)
	}
	return tn, invStore, mfgStore, *fg, *compA, *compB, *wh
}

// preReceiptStock posts a goods-receipt move so the manufacturing
// engine has stock to consume from. Mirrors how a real shop would
// stage components before releasing a work order.
func preReceiptStock(t *testing.T, ctx context.Context, invStore *inventory.PGStore, tenantID, actorID, itemID, warehouseID uuid.UUID, qty string) {
	t.Helper()
	q, err := decimal.NewFromString(qty)
	if err != nil {
		t.Fatalf("qty %q: %v", qty, err)
	}
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tenantID,
		ItemID:      itemID,
		WarehouseID: warehouseID,
		Qty:         q,
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   actorID,
	}); err != nil {
		t.Fatalf("pre-receipt move: %v", err)
	}
}

// TestPhaseN6BOMLifecycle covers BOM CRUD and the
// single-active-row-per-item invariant. Activating a second BOM for
// the same item must auto-demote the first to obsolete so the
// boms_active_per_item_uniq partial unique index never trips on the
// activation path.
func TestPhaseN6BOMLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, mfg, fg, compA, compB, _ := newTenantForManufacturing(t, h)
	actor := uuid.New()

	v1, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID:    fg.ID,
		Version:   "v1",
		OutputQty: decimal.NewFromInt(1),
		UOM:       "each",
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(2), UOM: "each", SortOrder: 0},
			{ComponentItemID: compB.ID, Qty: decimal.NewFromInt(3), UOM: "each", SortOrder: 1},
		},
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if v1.Status != manufacturing.BOMStatusDraft {
		t.Fatalf("expected v1.status=draft, got %s", v1.Status)
	}

	// Activate v1. With one BOM, this should just flip the row.
	if err := mfg.SetBOMStatus(ctx, tn.ID, v1.ID, manufacturing.BOMStatusActive); err != nil {
		t.Fatalf("activate v1: %v", err)
	}

	// Create v2 in draft and activate it. The store must
	// auto-demote v1 to obsolete so the partial unique index
	// stays satisfied.
	v2, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID:    fg.ID,
		Version:   "v2",
		OutputQty: decimal.NewFromInt(1),
		UOM:       "each",
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each", SortOrder: 0},
		},
	})
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if err := mfg.SetBOMStatus(ctx, tn.ID, v2.ID, manufacturing.BOMStatusActive); err != nil {
		t.Fatalf("activate v2: %v", err)
	}

	// v1 must now be obsolete.
	got, err := mfg.GetBOM(ctx, tn.ID, v1.ID)
	if err != nil {
		t.Fatalf("re-fetch v1: %v", err)
	}
	if got.Status != manufacturing.BOMStatusObsolete {
		t.Fatalf("expected v1 auto-demoted to obsolete, got %s", got.Status)
	}

	// Activating an empty BOM must fail with the sentinel.
	empty, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID: fg.ID, Version: "v-empty", OutputQty: decimal.NewFromInt(1), UOM: "each",
		Components: []manufacturing.BOMComponent{{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each"}},
	})
	if err != nil {
		t.Fatalf("create v-empty: %v", err)
	}
	// Drop the component to fake a "components forgotten on
	// upgrade" scenario. RLS requires the tenant_id GUC, so we
	// run inside dbutil.WithTenantTx rather than against the bare
	// pool (which would silently match zero rows).
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM bom_components WHERE tenant_id=$1 AND bom_id=$2`, tn.ID, empty.ID)
		return err
	}); err != nil {
		t.Fatalf("drop components: %v", err)
	}
	if err := mfg.SetBOMStatus(ctx, tn.ID, empty.ID, manufacturing.BOMStatusActive); !errors.Is(err, manufacturing.ErrBOMHasNoComponents) {
		t.Fatalf("expected ErrBOMHasNoComponents, got %v", err)
	}
}

// TestPhaseN6WorkOrderCompletionEmitsMoves is the headline test for
// Phase N6: a completed work order must consume each BOM component
// (scaled by yield + scrap) and receipt the finished good in the
// same transaction. The partial unique index on inventory_moves
// (source_ktype, source_id, item_id, warehouse_id) keeps the
// completion idempotent on retry.
func TestPhaseN6WorkOrderCompletionEmitsMoves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, compB, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	// Pre-stage stock for both components. compA scrap = 10%
	// will need 2 * 5 * 1.10 = 11; compB scrap = 0 will need
	// 3 * 5 = 15. Receipt 50 of each so the guard passes.
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "50")
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compB.ID, wh.ID, "50")

	scrap := decimal.NewFromInt(10)
	bom, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID:    fg.ID,
		Version:   "v1",
		OutputQty: decimal.NewFromInt(1),
		UOM:       "each",
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(2), UOM: "each", ScrapPercent: &scrap, SortOrder: 0},
			{ComponentItemID: compB.ID, Qty: decimal.NewFromInt(3), UOM: "each", SortOrder: 1},
		},
	})
	if err != nil {
		t.Fatalf("create bom: %v", err)
	}
	if err := mfg.SetBOMStatus(ctx, tn.ID, bom.ID, manufacturing.BOMStatusActive); err != nil {
		t.Fatalf("activate bom: %v", err)
	}

	wo, err := mfg.CreateWorkOrder(ctx, tn.ID, actor, manufacturing.CreateWorkOrderInput{
		ItemID:      fg.ID,
		WarehouseID: wh.ID,
		PlannedQty:  decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create work order: %v", err)
	}
	if _, err := mfg.ReleaseWorkOrder(ctx, tn.ID, wo.ID); err != nil {
		t.Fatalf("release work order: %v", err)
	}
	// Complete with planned actual (5 units).
	done, err := mfg.CompleteWorkOrder(ctx, tn.ID, wo.ID, actor, manufacturing.CompleteWorkOrderInput{})
	if err != nil {
		t.Fatalf("complete work order: %v", err)
	}
	if done.Status != manufacturing.WorkOrderStatusCompleted {
		t.Fatalf("expected status=completed, got %s", done.Status)
	}
	if done.CompletedAt == nil {
		t.Fatalf("expected completed_at to be stamped")
	}

	// Verify the three moves landed with the right qty + source
	// labels. RLS on inventory_moves requires the tenant_id GUC,
	// so we run the SUM through dbutil.WithTenantTx rather than
	// the bare pool.
	expectMoveSum := func(t *testing.T, itemID uuid.UUID, sourceKType string, want string) {
		t.Helper()
		var sum decimal.Decimal
		if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT COALESCE(SUM(qty), 0) FROM inventory_moves
				  WHERE tenant_id = $1 AND item_id = $2 AND source_ktype = $3 AND source_id = $4`,
				tn.ID, itemID, sourceKType, wo.ID,
			).Scan(&sum)
		}); err != nil {
			t.Fatalf("sum moves %s/%s: %v", itemID, sourceKType, err)
		}
		w, _ := decimal.NewFromString(want)
		if !sum.Equal(w) {
			t.Fatalf("move sum for %s/%s = %s, want %s", itemID, sourceKType, sum.String(), want)
		}
	}
	expectMoveSum(t, compA.ID, manufacturing.MoveSourceWorkOrderConsume, "-11")
	expectMoveSum(t, compB.ID, manufacturing.MoveSourceWorkOrderConsume, "-15")
	expectMoveSum(t, fg.ID, manufacturing.MoveSourceWorkOrderReceipt, "5")
}

// TestPhaseN6WorkOrderInsufficientStock guards the strict-mode
// stock check: completing a work order whose BOM consumption would
// drive a component negative must fail with the sentinel and leave
// the work order in its prior status so the operator can pre-receipt
// the missing component and retry.
func TestPhaseN6WorkOrderInsufficientStock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, _, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	// Pre-receipt only 1 unit of compA — the BOM consumption
	// for planned_qty=5 will need 10 units, so completion must
	// fail.
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "1")

	bom, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID:    fg.ID,
		Version:   "v1",
		OutputQty: decimal.NewFromInt(1),
		UOM:       "each",
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(2), UOM: "each", SortOrder: 0},
		},
	})
	if err != nil {
		t.Fatalf("create bom: %v", err)
	}
	if err := mfg.SetBOMStatus(ctx, tn.ID, bom.ID, manufacturing.BOMStatusActive); err != nil {
		t.Fatalf("activate bom: %v", err)
	}
	wo, err := mfg.CreateWorkOrder(ctx, tn.ID, actor, manufacturing.CreateWorkOrderInput{
		ItemID:      fg.ID,
		WarehouseID: wh.ID,
		PlannedQty:  decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create work order: %v", err)
	}
	if _, err := mfg.ReleaseWorkOrder(ctx, tn.ID, wo.ID); err != nil {
		t.Fatalf("release work order: %v", err)
	}
	_, err = mfg.CompleteWorkOrder(ctx, tn.ID, wo.ID, actor, manufacturing.CompleteWorkOrderInput{})
	if !errors.Is(err, manufacturing.ErrWorkOrderInsufficientStock) {
		t.Fatalf("expected ErrWorkOrderInsufficientStock, got %v", err)
	}

	// Work order should still be in `released` (or whatever
	// status it was in before the failed completion).
	again, err := mfg.GetWorkOrder(ctx, tn.ID, wo.ID)
	if err != nil {
		t.Fatalf("re-fetch work order: %v", err)
	}
	if again.Status != manufacturing.WorkOrderStatusReleased {
		t.Fatalf("expected work order to stay in released, got %s", again.Status)
	}
}

// TestPhaseN6CompletionIsIdempotent verifies the retry guard: a
// CompleteWorkOrder call that lands the status flip and emits some
// (but not all) of the moves can be safely retried — the second
// call sees status=completed and is a no-op for the status, and the
// inventory_moves_source_uniq partial unique index treats already-
// emitted moves as duplicates.
func TestPhaseN6CompletionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, _, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "100")

	bom, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID:    fg.ID,
		Version:   "v1",
		OutputQty: decimal.NewFromInt(1),
		UOM:       "each",
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each", SortOrder: 0},
		},
	})
	if err != nil {
		t.Fatalf("create bom: %v", err)
	}
	if err := mfg.SetBOMStatus(ctx, tn.ID, bom.ID, manufacturing.BOMStatusActive); err != nil {
		t.Fatalf("activate bom: %v", err)
	}
	wo, err := mfg.CreateWorkOrder(ctx, tn.ID, actor, manufacturing.CreateWorkOrderInput{
		ItemID:      fg.ID,
		WarehouseID: wh.ID,
		PlannedQty:  decimal.NewFromInt(3),
	})
	if err != nil {
		t.Fatalf("create work order: %v", err)
	}
	if _, err := mfg.ReleaseWorkOrder(ctx, tn.ID, wo.ID); err != nil {
		t.Fatalf("release work order: %v", err)
	}
	if _, err := mfg.CompleteWorkOrder(ctx, tn.ID, wo.ID, actor, manufacturing.CompleteWorkOrderInput{}); err != nil {
		t.Fatalf("complete work order: %v", err)
	}

	// Second completion must succeed and leave the move counts
	// unchanged.
	if _, err := mfg.CompleteWorkOrder(ctx, tn.ID, wo.ID, actor, manufacturing.CompleteWorkOrderInput{}); err != nil {
		t.Fatalf("second complete work order: %v", err)
	}
	var n int
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM inventory_moves
			  WHERE tenant_id = $1 AND source_id = $2`,
			tn.ID, wo.ID,
		).Scan(&n)
	}); err != nil {
		t.Fatalf("count moves: %v", err)
	}
	// 1 consumption + 1 receipt = 2 moves total. A retry would
	// have doubled this if idempotency was broken.
	if n != 2 {
		t.Fatalf("expected exactly 2 moves after retry, got %d", n)
	}
}

// TestPhaseN6IllegalTransition verifies the Go-side state-machine
// guard rejects backwards transitions with a typed sentinel.
func TestPhaseN6IllegalTransition(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, _, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "100")

	bom, _ := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID: fg.ID, Version: "v1", OutputQty: decimal.NewFromInt(1), UOM: "each",
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each"},
		},
	})
	if err := mfg.SetBOMStatus(ctx, tn.ID, bom.ID, manufacturing.BOMStatusActive); err != nil {
		t.Fatalf("activate bom: %v", err)
	}
	wo, _ := mfg.CreateWorkOrder(ctx, tn.ID, actor, manufacturing.CreateWorkOrderInput{
		ItemID: fg.ID, WarehouseID: wh.ID, PlannedQty: decimal.NewFromInt(2),
	})

	// draft → in_progress is illegal (must go through released).
	if _, err := mfg.StartWorkOrder(ctx, tn.ID, wo.ID); !errors.Is(err, manufacturing.ErrWorkOrderInvalidTransition) {
		t.Fatalf("expected ErrWorkOrderInvalidTransition for draft→in_progress, got %v", err)
	}
}
