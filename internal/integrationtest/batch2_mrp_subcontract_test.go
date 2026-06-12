//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/manufacturing"
)

// onHandForItem sums the signed inventory_moves ledger for an item in a
// warehouse, through the tenant GUC (RLS).
func onHandForItem(t *testing.T, ctx context.Context, h *harness, tenantID, itemID, warehouseID uuid.UUID) decimal.Decimal {
	t.Helper()
	var sum decimal.Decimal
	if err := dbutil.WithTenantTx(ctx, h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(qty), 0) FROM inventory_moves
			  WHERE tenant_id = $1 AND item_id = $2 AND warehouse_id = $3`,
			tenantID, itemID, warehouseID,
		).Scan(&sum)
	}); err != nil {
		t.Fatalf("on-hand %s: %v", itemID, err)
	}
	return sum
}

// activeBOMFor authors and activates a single-level BOM that produces fg
// from the supplied components (qty per output unit).
func activeBOMFor(t *testing.T, ctx context.Context, mfg *manufacturing.PGStore, tenantID, actor, fg uuid.UUID, comps []manufacturing.BOMComponent) {
	t.Helper()
	if _, err := mfg.CreateBOM(ctx, tenantID, actor, manufacturing.CreateBOMInput{
		ItemID:     fg,
		Version:    "v1",
		OutputQty:  decimal.NewFromInt(1),
		UOM:        "each",
		Components: comps,
		Activate:   true,
	}); err != nil {
		t.Fatalf("create+activate bom: %v", err)
	}
}

// TestBatch2MRPMakeAndBuy drives the core MRP loop: a finished make item
// explodes its active BOM into component demand, each item is netted
// against on-hand, and the run + planned orders persist and read back.
func TestBatch2MRPMakeAndBuy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, compB, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	activeBOMFor(t, ctx, mfg, tn.ID, actor, fg.ID, []manufacturing.BOMComponent{
		{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(2), UOM: "each"},
		{ComponentItemID: compB.ID, Qty: decimal.NewFromInt(3), UOM: "each"},
	})
	// compA already partly in stock; compB and fg empty.
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "5")

	due := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	run, err := mfg.RunMRP(ctx, tn.ID, actor, manufacturing.MRPRunInput{
		HorizonStart:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		HorizonEnd:      time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		BuyLeadTimeDays: 5,
		Demand: []manufacturing.MRPDemandInput{
			{ItemID: fg.ID, Qty: decimal.NewFromInt(10), DueDate: due, Source: manufacturing.MRPDemandSourceSalesOrder, SourceRef: "SO-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunMRP: %v", err)
	}
	if run.Status != manufacturing.MRPRunStatusCompleted {
		t.Fatalf("run status = %s, want completed", run.Status)
	}
	if run.PlannedOrderCount != 3 || run.MakeOrderCount != 1 || run.BuyOrderCount != 2 {
		t.Fatalf("counts planned=%d make=%d buy=%d, want 3/1/2", run.PlannedOrderCount, run.MakeOrderCount, run.BuyOrderCount)
	}

	byItem := map[uuid.UUID]manufacturing.MRPPlannedOrder{}
	for _, po := range run.PlannedOrders {
		byItem[po.ItemID] = po
	}
	mk := byItem[fg.ID]
	if mk.OrderType != manufacturing.MRPOrderTypeMake || !mk.Qty.Equal(decimal.NewFromInt(10)) || mk.ExplosionLevel != 0 {
		t.Fatalf("fg planned order = %+v", mk)
	}
	if mk.BOMID == nil {
		t.Fatalf("fg make order should snapshot bom_id")
	}
	a := byItem[compA.ID]
	if a.OrderType != manufacturing.MRPOrderTypeBuy || !a.Qty.Equal(decimal.NewFromInt(15)) || a.ExplosionLevel != 1 {
		t.Fatalf("compA planned order = %+v, want buy qty 15 level 1", a)
	}
	b := byItem[compB.ID]
	if b.OrderType != manufacturing.MRPOrderTypeBuy || !b.Qty.Equal(decimal.NewFromInt(30)) || b.ExplosionLevel != 1 {
		t.Fatalf("compB planned order = %+v, want buy qty 30 level 1", b)
	}
	// No routing on fg => default make lead time of 1 day.
	if mk.LeadTimeDays != 1 {
		t.Fatalf("make lead = %d, want 1 (no routing default)", mk.LeadTimeDays)
	}
	wantMakeStart := due.AddDate(0, 0, -1)
	if !mk.SuggestedStartDate.Equal(wantMakeStart) {
		t.Fatalf("make start = %s, want %s", mk.SuggestedStartDate.Format("2006-01-02"), wantMakeStart.Format("2006-01-02"))
	}
	// Components are due by the make order's start and backward scheduled
	// by the 5-day buy lead.
	if !a.DueDate.Equal(mk.SuggestedStartDate) {
		t.Fatalf("compA due = %s, want %s", a.DueDate.Format("2006-01-02"), mk.SuggestedStartDate.Format("2006-01-02"))
	}
	if !a.SuggestedStartDate.Equal(a.DueDate.AddDate(0, 0, -5)) {
		t.Fatalf("compA start = %s, want due-5", a.SuggestedStartDate.Format("2006-01-02"))
	}

	// Read the run back: header + demand snapshot + planned orders.
	got, err := mfg.GetMRPRun(ctx, tn.ID, run.ID)
	if err != nil {
		t.Fatalf("GetMRPRun: %v", err)
	}
	if len(got.DemandLines) != 1 || got.DemandLines[0].Source != manufacturing.MRPDemandSourceSalesOrder {
		t.Fatalf("demand lines = %+v", got.DemandLines)
	}
	if len(got.PlannedOrders) != 3 {
		t.Fatalf("planned orders read back = %d, want 3", len(got.PlannedOrders))
	}

	// Listing surfaces the run header.
	runs, err := mfg.ListMRPRuns(ctx, tn.ID)
	if err != nil {
		t.Fatalf("ListMRPRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("ListMRPRuns = %+v", runs)
	}
}

// TestBatch2MRPNoDemand rejects a run with neither explicit demand nor
// min-stock enabled.
func TestBatch2MRPNoDemand(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, mfg, _, _, _, _ := newTenantForManufacturing(t, h)
	_, err := mfg.RunMRP(ctx, tn.ID, uuid.New(), manufacturing.MRPRunInput{
		HorizonStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		HorizonEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, manufacturing.ErrMRPNoDemand) {
		t.Fatalf("RunMRP no-demand err = %v, want ErrMRPNoDemand", err)
	}
}

// TestBatch2MRPMinStock synthesises demand from items below their
// reorder level.
func TestBatch2MRPMinStock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, _, compA, _, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	// compA wants 12 on hand; only 4 are stocked => shortfall 8.
	if _, err := invStore.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: compA.SKU, Name: compA.Name, UOM: compA.UOM, Active: true,
		ReorderLevel: decimal.NewFromInt(12),
	}); err != nil {
		t.Fatalf("set reorder level: %v", err)
	}
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "4")

	run, err := mfg.RunMRP(ctx, tn.ID, actor, manufacturing.MRPRunInput{
		HorizonStart:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		HorizonEnd:      time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		IncludeMinStock: true,
	})
	if err != nil {
		t.Fatalf("RunMRP min-stock: %v", err)
	}
	var found bool
	for _, po := range run.PlannedOrders {
		if po.ItemID == compA.ID {
			found = true
			if !po.Qty.Equal(decimal.NewFromInt(8)) {
				t.Fatalf("min-stock compA qty = %s, want 8", po.Qty.String())
			}
		}
	}
	if !found {
		t.Fatalf("expected a min-stock planned order for compA")
	}
	for _, dl := range run.DemandLines {
		if dl.ItemID == compA.ID && dl.Source != manufacturing.MRPDemandSourceMinStock {
			t.Fatalf("compA demand source = %s, want min_stock", dl.Source)
		}
	}
}

// TestBatch2SubcontractLifecycle drives the full subcontracting flow:
// create → issue (components leave stock) → receive (finished item lands,
// valued at the service charge) → close, plus idempotent issue replay.
func TestBatch2SubcontractLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, compB, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "10")
	preReceiptStock(t, ctx, invStore, tn.ID, actor, compB.ID, wh.ID, "10")

	order, err := mfg.CreateSubcontractOrder(ctx, tn.ID, actor, manufacturing.CreateSubcontractOrderInput{
		ItemID:       fg.ID,
		WarehouseID:  wh.ID,
		Qty:          decimal.NewFromInt(10),
		ChargeAmount: decimal.NewFromInt(100),
		Components: []manufacturing.SubcontractComponentInput{
			{ItemID: compA.ID, Qty: decimal.NewFromInt(4)},
			{ItemID: compB.ID, Qty: decimal.NewFromInt(6)},
		},
	})
	if err != nil {
		t.Fatalf("CreateSubcontractOrder: %v", err)
	}
	if order.Status != manufacturing.SubcontractStatusDraft {
		t.Fatalf("status = %s, want draft", order.Status)
	}

	issued, err := mfg.IssueSubcontractOrder(ctx, tn.ID, actor, order.ID, manufacturing.IssueSubcontractInput{})
	if err != nil {
		t.Fatalf("IssueSubcontractOrder: %v", err)
	}
	if issued.Status != manufacturing.SubcontractStatusIssued || issued.IssuedAt == nil {
		t.Fatalf("issued order = %+v", issued)
	}
	if got := onHandForItem(t, ctx, h, tn.ID, compA.ID, wh.ID); !got.Equal(decimal.NewFromInt(6)) {
		t.Fatalf("compA on-hand after issue = %s, want 6", got.String())
	}
	if got := onHandForItem(t, ctx, h, tn.ID, compB.ID, wh.ID); !got.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("compB on-hand after issue = %s, want 4", got.String())
	}

	// Idempotent re-issue must not double-deduct.
	if _, err := mfg.IssueSubcontractOrder(ctx, tn.ID, actor, order.ID, manufacturing.IssueSubcontractInput{}); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if got := onHandForItem(t, ctx, h, tn.ID, compA.ID, wh.ID); !got.Equal(decimal.NewFromInt(6)) {
		t.Fatalf("compA on-hand after re-issue = %s, want 6", got.String())
	}

	received, err := mfg.ReceiveSubcontractOrder(ctx, tn.ID, actor, order.ID, manufacturing.ReceiveSubcontractInput{})
	if err != nil {
		t.Fatalf("ReceiveSubcontractOrder: %v", err)
	}
	if received.Status != manufacturing.SubcontractStatusReceived || !received.ReceivedQty.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("received order = %+v", received)
	}
	if got := onHandForItem(t, ctx, h, tn.ID, fg.ID, wh.ID); !got.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("fg on-hand after receive = %s, want 10", got.String())
	}
	// Receipt move carries the per-unit service charge (100 / 10 = 10).
	var unitCost decimal.Decimal
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT unit_cost FROM inventory_moves
			  WHERE tenant_id = $1 AND item_id = $2 AND source_ktype = $3 AND source_id = $4`,
			tn.ID, fg.ID, manufacturing.MoveSourceSubcontractReceipt, order.ID,
		).Scan(&unitCost)
	}); err != nil {
		t.Fatalf("read receipt unit cost: %v", err)
	}
	if !unitCost.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("receipt unit cost = %s, want 10", unitCost.String())
	}

	closed, err := mfg.CloseSubcontractOrder(ctx, tn.ID, order.ID)
	if err != nil {
		t.Fatalf("CloseSubcontractOrder: %v", err)
	}
	if closed.Status != manufacturing.SubcontractStatusClosed {
		t.Fatalf("status = %s, want closed", closed.Status)
	}
}

// TestBatch2SubcontractInsufficientStock rejects an issue that would
// drive a component negative.
func TestBatch2SubcontractInsufficientStock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, _, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "1")
	order, err := mfg.CreateSubcontractOrder(ctx, tn.ID, actor, manufacturing.CreateSubcontractOrderInput{
		ItemID:      fg.ID,
		WarehouseID: wh.ID,
		Qty:         decimal.NewFromInt(1),
		Components: []manufacturing.SubcontractComponentInput{
			{ItemID: compA.ID, Qty: decimal.NewFromInt(5)},
		},
	})
	if err != nil {
		t.Fatalf("CreateSubcontractOrder: %v", err)
	}
	if _, err := mfg.IssueSubcontractOrder(ctx, tn.ID, actor, order.ID, manufacturing.IssueSubcontractInput{}); !errors.Is(err, manufacturing.ErrSubcontractInsufficientStock) {
		t.Fatalf("issue err = %v, want ErrSubcontractInsufficientStock", err)
	}
	// Order stays in draft; no stock moved.
	if got := onHandForItem(t, ctx, h, tn.ID, compA.ID, wh.ID); !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("compA on-hand after failed issue = %s, want 1", got.String())
	}
}

// TestBatch2SubcontractCancelDraft cancels a draft order and rejects a
// cancel once components have been issued.
func TestBatch2SubcontractCancelDraft(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, invStore, mfg, fg, compA, _, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	preReceiptStock(t, ctx, invStore, tn.ID, actor, compA.ID, wh.ID, "10")
	order, err := mfg.CreateSubcontractOrder(ctx, tn.ID, actor, manufacturing.CreateSubcontractOrderInput{
		ItemID:      fg.ID,
		WarehouseID: wh.ID,
		Qty:         decimal.NewFromInt(1),
		Components: []manufacturing.SubcontractComponentInput{
			{ItemID: compA.ID, Qty: decimal.NewFromInt(2)},
		},
	})
	if err != nil {
		t.Fatalf("CreateSubcontractOrder: %v", err)
	}
	cancelled, err := mfg.CancelSubcontractOrder(ctx, tn.ID, order.ID)
	if err != nil {
		t.Fatalf("CancelSubcontractOrder: %v", err)
	}
	if cancelled.Status != manufacturing.SubcontractStatusCancelled {
		t.Fatalf("status = %s, want cancelled", cancelled.Status)
	}

	// A fresh issued order can no longer be cancelled.
	order2, err := mfg.CreateSubcontractOrder(ctx, tn.ID, actor, manufacturing.CreateSubcontractOrderInput{
		ItemID:      fg.ID,
		WarehouseID: wh.ID,
		Qty:         decimal.NewFromInt(1),
		Components: []manufacturing.SubcontractComponentInput{
			{ItemID: compA.ID, Qty: decimal.NewFromInt(2)},
		},
	})
	if err != nil {
		t.Fatalf("CreateSubcontractOrder 2: %v", err)
	}
	if _, err := mfg.IssueSubcontractOrder(ctx, tn.ID, actor, order2.ID, manufacturing.IssueSubcontractInput{}); err != nil {
		t.Fatalf("issue order2: %v", err)
	}
	if _, err := mfg.CancelSubcontractOrder(ctx, tn.ID, order2.ID); !errors.Is(err, manufacturing.ErrSubcontractInvalidTransition) {
		t.Fatalf("cancel issued err = %v, want ErrSubcontractInvalidTransition", err)
	}
}
