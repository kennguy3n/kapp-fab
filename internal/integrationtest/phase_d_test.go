//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// newTenantForInventory builds a fresh tenant wired for Phase D:
// finance + inventory KTypes registered, chart of accounts seeded,
// a sample item+warehouse pair, and an InvoicePoster with the
// inventory hook attached so posted invoices/bills emit stock moves.
func newTenantForInventory(t *testing.T, h *harness) (
	*tenant.Tenant,
	*ledger.PGStore,
	*ledger.InvoicePoster,
	*inventory.PGStore,
	inventory.Item,
	inventory.Warehouse,
) {
	t.Helper()
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phased"), Name: "Phase D Co", Cell: "test", Plan: "free",
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

	ledgerStore := ledger.NewPGStore(h.pool, h.publisher, h.auditor)
	invStore := inventory.NewPGStore(h.pool, h.publisher, h.auditor)
	hook := inventory.NewPosterHook(invStore)
	poster := ledger.NewInvoicePoster(ledgerStore, h.records).
		WithSalesInvoiceHook(hook.OnSalesInvoicePosted).
		WithPurchaseBillHook(hook.OnPurchaseBillPosted)

	seed := []ledger.Account{
		{TenantID: tn.ID, Code: "1100", Name: "Accounts Receivable", Type: ledger.AccountTypeAsset, Active: true},
		{TenantID: tn.ID, Code: "2100", Name: "Accounts Payable", Type: ledger.AccountTypeLiability, Active: true},
		{TenantID: tn.ID, Code: "2200", Name: "Tax Payable", Type: ledger.AccountTypeLiability, Active: true},
		{TenantID: tn.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
		{TenantID: tn.ID, Code: "5000", Name: "Cost of Goods Sold", Type: ledger.AccountTypeExpense, Active: true},
		{TenantID: tn.ID, Code: "6000", Name: "Operating Expense", Type: ledger.AccountTypeExpense, Active: true},
	}
	for _, a := range seed {
		if _, err := ledgerStore.CreateAccount(ctx, a); err != nil {
			t.Fatalf("seed account %s: %v", a.Code, err)
		}
	}

	item, err := invStore.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "SKU-WIDGET-1", Name: "Widget", UOM: "each", Active: true,
	})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	wh, err := invStore.UpsertWarehouse(ctx, inventory.Warehouse{
		TenantID: tn.ID, Code: "WH-MAIN", Name: "Main Warehouse",
	})
	if err != nil {
		t.Fatalf("seed warehouse: %v", err)
	}
	return tn, ledgerStore, poster, invStore, *item, *wh
}

// createARInvoiceWithInventoryLine inserts a draft finance.ar_invoice
// carrying a single inventory-backed line so the inventory hook has
// something to act on when the invoice posts.
func createARInvoiceWithInventoryLine(
	t *testing.T, h *harness, tenantID, actorID uuid.UUID,
	number, customer string, itemID, warehouseID uuid.UUID,
	qty decimal.Decimal, unitPrice decimal.Decimal,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	subtotal := qty.Mul(unitPrice)
	tax := decimal.Zero
	total := subtotal.Add(tax)
	subF, _ := subtotal.Float64()
	totF, _ := total.Float64()
	qtyF, _ := qty.Float64()
	priceF, _ := unitPrice.Float64()
	data := map[string]any{
		"customer_id":          customer,
		"invoice_number":       number,
		"issue_date":           "2026-01-15",
		"due_date":             "2026-02-14",
		"subtotal":             subF,
		"tax_amount":           0,
		"total":                totF,
		"currency":             "USD",
		"status":               "draft",
		"ar_account_code":      "1100",
		"revenue_account_code": "4000",
		"lines": []map[string]any{{
			"item_id":      itemID.String(),
			"warehouse_id": warehouseID.String(),
			"qty":          qtyF,
			"unit_price":   priceF,
		}},
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal invoice: %v", err)
	}
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     finance.KTypeARInvoice,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create invoice record: %v", err)
	}
	return rec.ID
}

func createAPBillWithInventoryLine(
	t *testing.T, h *harness, tenantID, actorID uuid.UUID,
	number, supplier string, itemID, warehouseID uuid.UUID,
	qty decimal.Decimal, unitCost decimal.Decimal,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	subtotal := qty.Mul(unitCost)
	subF, _ := subtotal.Float64()
	totF, _ := subtotal.Float64()
	qtyF, _ := qty.Float64()
	costF, _ := unitCost.Float64()
	data := map[string]any{
		"supplier_id":          supplier,
		"bill_number":          number,
		"issue_date":           "2026-01-20",
		"due_date":             "2026-02-19",
		"subtotal":             subF,
		"tax_amount":           0,
		"total":                totF,
		"currency":             "USD",
		"status":               "draft",
		"ap_account_code":      "2100",
		"expense_account_code": "6000",
		"lines": []map[string]any{{
			"item_id":      itemID.String(),
			"warehouse_id": warehouseID.String(),
			"qty":          qtyF,
			"unit_cost":    costF,
		}},
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal bill: %v", err)
	}
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     finance.KTypeAPBill,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create bill record: %v", err)
	}
	return rec.ID
}

// TestSalesInvoicePostsDeliveryMove posts an AR invoice carrying one
// inventory line and checks that the inventory hook emitted a single
// negative-qty delivery move linked to that invoice.
func TestSalesInvoicePostsDeliveryMove(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, poster, invStore, item, wh := newTenantForInventory(t, h)

	// Seed 10 widgets on-hand so the delivery has stock to consume and
	// we can assert the level decreased (not just turned negative).
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(10), UnitCost: decimal.NewFromInt(5),
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: uuid.New(),
	}); err != nil {
		t.Fatalf("seed opening stock: %v", err)
	}

	actor := uuid.New()
	invoiceID := createARInvoiceWithInventoryLine(
		t, h, tn.ID, actor, "INV-D-1", uuid.NewString(),
		item.ID, wh.ID, decimal.NewFromInt(3), decimal.NewFromInt(25),
	)
	if _, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor); err != nil {
		t.Fatalf("post sales invoice: %v", err)
	}

	mv, err := invStore.GetMoveBySource(ctx, tn.ID, inventory.MoveSourceSalesInvoice, invoiceID, item.ID, wh.ID)
	if err != nil {
		t.Fatalf("load source move: %v", err)
	}
	if mv == nil {
		t.Fatalf("expected a move for invoice %s; got nil", invoiceID)
	}
	if !mv.Qty.Equal(decimal.NewFromInt(-3)) {
		t.Fatalf("delivery qty = %s; want -3", mv.Qty)
	}

	levels, err := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list stock levels: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("stock levels = %d; want 1 row", len(levels))
	}
	if !levels[0].Qty.Equal(decimal.NewFromInt(7)) {
		t.Fatalf("stock qty = %s; want 7 (10 opening - 3 delivery)", levels[0].Qty)
	}
}

// TestPurchaseBillPostsReceiptMove is the AP analog: posted bill emits
// a positive-qty receipt move and stock level rises by that amount.
func TestPurchaseBillPostsReceiptMove(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, poster, invStore, item, wh := newTenantForInventory(t, h)

	actor := uuid.New()
	billID := createAPBillWithInventoryLine(
		t, h, tn.ID, actor, "BILL-D-1", uuid.NewString(),
		item.ID, wh.ID, decimal.NewFromInt(25), decimal.NewFromInt(4),
	)
	if _, err := poster.PostPurchaseBill(ctx, tn.ID, billID, actor); err != nil {
		t.Fatalf("post purchase bill: %v", err)
	}

	mv, err := invStore.GetMoveBySource(ctx, tn.ID, inventory.MoveSourcePurchaseBill, billID, item.ID, wh.ID)
	if err != nil {
		t.Fatalf("load source move: %v", err)
	}
	if mv == nil || !mv.Qty.Equal(decimal.NewFromInt(25)) {
		t.Fatalf("receipt move = %+v; want qty 25", mv)
	}

	levels, err := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list stock levels: %v", err)
	}
	if len(levels) != 1 || !levels[0].Qty.Equal(decimal.NewFromInt(25)) {
		t.Fatalf("stock levels = %+v; want one row with qty 25", levels)
	}
}

// TestStockLevelsMatchSumOfMoves asserts that every row in
// `stock_levels` equals SUM(qty) from `inventory_moves` for that
// (item, warehouse) regardless of how the moves were composed —
// receipts, deliveries, adjustments, and transfer legs are all
// represented so the projection is exercised across every
// MoveSourceKType the store emits.
func TestStockLevelsMatchSumOfMoves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, invStore, item, whSrc := newTenantForInventory(t, h)
	actor := uuid.New()

	whDst, err := invStore.UpsertWarehouse(ctx, inventory.Warehouse{
		TenantID: tn.ID, Code: "WH-SECONDARY", Name: "Secondary",
	})
	if err != nil {
		t.Fatalf("seed destination warehouse: %v", err)
	}

	moves := []struct {
		qty         decimal.Decimal
		sourceKType string
		warehouseID uuid.UUID
	}{
		{decimal.NewFromInt(100), inventory.MoveSourcePurchaseBill, whSrc.ID},
		{decimal.NewFromInt(-12), inventory.MoveSourceSalesInvoice, whSrc.ID},
		{decimal.NewFromInt(3), inventory.MoveSourceAdjustment, whSrc.ID},
		{decimal.NewFromInt(-50), inventory.MoveSourceSalesInvoice, whSrc.ID},
	}
	for i, m := range moves {
		src := uuid.New()
		if _, err := invStore.RecordMove(ctx, inventory.Move{
			TenantID: tn.ID, ItemID: item.ID, WarehouseID: m.warehouseID,
			Qty: m.qty, UnitCost: decimal.NewFromInt(1),
			SourceKType: m.sourceKType, SourceID: &src,
			MovedAt:   time.Now().UTC().Add(time.Duration(i) * time.Second),
			CreatedBy: actor,
		}); err != nil {
			t.Fatalf("record move %d: %v", i, err)
		}
	}
	if _, err := invStore.RecordTransfer(ctx, inventory.Transfer{
		TenantID: tn.ID, ItemID: item.ID,
		FromWarehouse: whSrc.ID, ToWarehouse: whDst.ID,
		Qty: decimal.NewFromInt(10), UnitCost: decimal.NewFromInt(1),
		CreatedBy: actor,
	}); err != nil {
		t.Fatalf("record transfer: %v", err)
	}

	levels, err := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list stock levels: %v", err)
	}
	byWH := map[uuid.UUID]decimal.Decimal{}
	for _, l := range levels {
		byWH[l.WarehouseID] = l.Qty
	}

	sums, err := sumMovesByWarehouse(ctx, h.pool, tn.ID, item.ID)
	if err != nil {
		t.Fatalf("sum moves: %v", err)
	}
	if len(sums) != len(byWH) {
		t.Fatalf("warehouse count mismatch: levels=%d sums=%d", len(byWH), len(sums))
	}
	for wid, want := range sums {
		got, ok := byWH[wid]
		if !ok {
			t.Fatalf("stock_levels missing warehouse %s", wid)
		}
		if !got.Equal(want) {
			t.Fatalf("stock_levels qty for %s = %s; want %s (SUM of moves)", wid, got, want)
		}
	}
}

// TestWarehouseTransfersAreBalanced exercises RecordTransfer and
// confirms the two emitted moves sum to zero at the item level while
// redistributing stock from source to destination warehouse.
func TestWarehouseTransfersAreBalanced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, invStore, item, whSrc := newTenantForInventory(t, h)
	actor := uuid.New()

	whDst, err := invStore.UpsertWarehouse(ctx, inventory.Warehouse{
		TenantID: tn.ID, Code: "WH-AUX", Name: "Auxiliary",
	})
	if err != nil {
		t.Fatalf("seed destination warehouse: %v", err)
	}

	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: whSrc.ID,
		Qty: decimal.NewFromInt(40), UnitCost: decimal.NewFromInt(2),
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("seed opening stock: %v", err)
	}

	moves, err := invStore.RecordTransfer(ctx, inventory.Transfer{
		TenantID: tn.ID, ItemID: item.ID,
		FromWarehouse: whSrc.ID, ToWarehouse: whDst.ID,
		Qty: decimal.NewFromInt(15), UnitCost: decimal.NewFromInt(2),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("record transfer: %v", err)
	}
	if len(moves) != 2 {
		t.Fatalf("transfer emitted %d moves; want 2", len(moves))
	}
	if !moves[0].Qty.Add(moves[1].Qty).IsZero() {
		t.Fatalf("transfer legs unbalanced: %s + %s", moves[0].Qty, moves[1].Qty)
	}

	levels, err := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list stock levels: %v", err)
	}
	byWH := map[uuid.UUID]decimal.Decimal{}
	for _, l := range levels {
		byWH[l.WarehouseID] = l.Qty
	}
	if got := byWH[whSrc.ID]; !got.Equal(decimal.NewFromInt(25)) {
		t.Fatalf("source warehouse qty = %s; want 25 (40 - 15)", got)
	}
	if got := byWH[whDst.ID]; !got.Equal(decimal.NewFromInt(15)) {
		t.Fatalf("destination warehouse qty = %s; want 15", got)
	}
}

// sumMovesByWarehouse computes SUM(qty) grouped by warehouse_id for an
// item, reading directly from the inventory_moves table. Used as an
// independent cross-check against the stock_levels projection.
func sumMovesByWarehouse(ctx context.Context, pool *pgxpool.Pool, tenantID, itemID uuid.UUID) (map[uuid.UUID]decimal.Decimal, error) {
	out := map[uuid.UUID]decimal.Decimal{}
	err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT warehouse_id, SUM(qty)
			   FROM inventory_moves
			  WHERE tenant_id = $1 AND item_id = $2
			  GROUP BY warehouse_id`,
			tenantID, itemID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var wid uuid.UUID
			var sum decimal.Decimal
			if err := rows.Scan(&wid, &sum); err != nil {
				return err
			}
			out[wid] = sum
		}
		return rows.Err()
	})
	return out, err
}
