//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"fmt"
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
		// sort_order is derived server-side from the slice index;
		// any explicit value the caller sets here is ignored.
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(2), UOM: "each"},
			{ComponentItemID: compB.ID, Qty: decimal.NewFromInt(3), UOM: "each"},
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
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each"},
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

// TestPhaseN6BOMComponentSortOrderIsArrayIndex pins the invariant
// that bom_components.sort_order is derived from the caller's slice
// index (1-based) regardless of what the caller passed in
// BOMComponent.SortOrder. Earlier code treated SortOrder == 0 as
// "unset" and rewrote it to i+1, which silently collided the first
// component (received 0 → rewrote to 1) with the second (kept its
// explicit 1) and produced non-deterministic ordering after the
// `ORDER BY sort_order, component_item_id` tiebreaker. The frontend
// (BOMPage.tsx) sends `sort_order: i` (0-indexed) and triggered the
// collision on every BOM with two or more components.
func TestPhaseN6BOMComponentSortOrderIsArrayIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, mfg, fg, compA, compB, _ := newTenantForManufacturing(t, h)
	actor := uuid.New()

	// All three combinations exercise the collision: 0+1
	// (what the frontend actually sends), 5+9 (out-of-range
	// values the caller might supply by mistake), and the
	// zero-value default. Whatever the caller passes must be
	// ignored — only the slice position is authoritative.
	cases := []struct {
		name    string
		sortIn0 int
		sortIn1 int
	}{
		{"frontend_zero_indexed", 0, 1},
		{"caller_supplied_high", 5, 9},
		{"both_zero", 0, 0},
	}
	for i, tc := range cases {
		bom, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
			ItemID:    fg.ID,
			Version:   fmt.Sprintf("sort-%d", i),
			OutputQty: decimal.NewFromInt(1),
			UOM:       "each",
			Components: []manufacturing.BOMComponent{
				{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each", SortOrder: tc.sortIn0},
				{ComponentItemID: compB.ID, Qty: decimal.NewFromInt(1), UOM: "each", SortOrder: tc.sortIn1},
			},
		})
		if err != nil {
			t.Fatalf("%s: create bom: %v", tc.name, err)
		}
		got, err := mfg.GetBOM(ctx, tn.ID, bom.ID)
		if err != nil {
			t.Fatalf("%s: get bom: %v", tc.name, err)
		}
		if len(got.Components) != 2 {
			t.Fatalf("%s: expected 2 components, got %d", tc.name, len(got.Components))
		}
		// First component must land at sort_order=1, second at
		// sort_order=2 — and they must NOT collide.
		if got.Components[0].SortOrder == got.Components[1].SortOrder {
			t.Fatalf("%s: sort_order collision: both = %d", tc.name, got.Components[0].SortOrder)
		}
		if got.Components[0].SortOrder != 1 || got.Components[1].SortOrder != 2 {
			t.Fatalf("%s: expected sort_order 1,2; got %d,%d",
				tc.name, got.Components[0].SortOrder, got.Components[1].SortOrder)
		}
		// And the result must come back in the caller's slice
		// order (compA first, compB second).
		if got.Components[0].ComponentItemID != compA.ID || got.Components[1].ComponentItemID != compB.ID {
			t.Fatalf("%s: expected compA then compB; got %s then %s",
				tc.name, got.Components[0].ComponentItemID, got.Components[1].ComponentItemID)
		}
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
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(2), UOM: "each", ScrapPercent: &scrap},
			{ComponentItemID: compB.ID, Qty: decimal.NewFromInt(3), UOM: "each"},
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

	// Replay scenario: Phase 2 commits one inventory move per
	// row OUTSIDE the Phase 1 transaction, so a crash partway
	// through (e.g. between move 2 of 3 and move 3 of 3) leaves
	// the work order stamped `completed` while one or more
	// inventory_moves rows never land. Calling CompleteWorkOrder
	// again must replay just the missing moves — without re-
	// emitting the ones that did land (idempotent via the
	// inventory_moves_source_uniq partial unique index).
	//
	// Simulate the crash by hand-deleting one of the moves and
	// re-running CompleteWorkOrder.
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM inventory_moves
			  WHERE tenant_id=$1 AND item_id=$2 AND source_ktype=$3 AND source_id=$4`,
			tn.ID, compB.ID, manufacturing.MoveSourceWorkOrderConsume, wo.ID)
		return err
	}); err != nil {
		t.Fatalf("simulate partial phase-2 failure: %v", err)
	}
	expectMoveSum(t, compB.ID, manufacturing.MoveSourceWorkOrderConsume, "0")

	// Re-completion of a status=completed work order used to
	// be an early-return no-op, silently leaving the missing
	// move on the floor. The fix recomputes the moves on the
	// replay path and lets Phase 2 re-emit only the ones
	// missing from inventory_moves.
	if _, err := mfg.CompleteWorkOrder(ctx, tn.ID, wo.ID, actor, manufacturing.CompleteWorkOrderInput{}); err != nil {
		t.Fatalf("replay completion: %v", err)
	}
	// The missing component move is back.
	expectMoveSum(t, compB.ID, manufacturing.MoveSourceWorkOrderConsume, "-15")
	// And the moves that originally landed are NOT double-
	// counted — Phase 2 hit ErrDuplicateSourceMove for them
	// and skipped.
	expectMoveSum(t, compA.ID, manufacturing.MoveSourceWorkOrderConsume, "-11")
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
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(2), UOM: "each"},
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
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each"},
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
