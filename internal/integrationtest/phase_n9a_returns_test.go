//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/sales"
)

// TestSalesReturnLifecycle walks a sales.return through every
// transition end-to-end against a live tenant + the real
// ReturnPoster, asserting the full posting footprint at each step:
//
//  1. Seed a posted finance.ar_invoice with an inventory-backed
//     line (the return needs an `original_invoice_id` whose
//     `ar_account_code` + `revenue_account_code` the credit-note
//     posting reads).
//  2. Create a draft sales.return.
//  3. Approve → assert status=approved, no posting side-effects.
//  4. Receive → assert status=received, exactly one positive-qty
//     inventory_moves row with source_ktype=sales.return,
//     source_id=<return id>.
//  5. Refund → assert status=refunded, finance.credit_note KRecord
//     was created (with `created_from_return_id` back-pointer),
//     credit_note status=posted, journal_entry stamped on both the
//     credit note and the return. Verifies the JE reverses
//     AR / Revenue with the same code pair the original invoice used.
//  6. Replay each transition idempotently — Receive twice does not
//     double-stock, Refund twice does not create a second credit
//     note or a second JE.
func TestSalesReturnLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, ledgerStore, invoicePoster, invStore, item, warehouse := newTenantForInventory(t, h)
	for _, kt := range sales.ReturnKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register sales.return ktype: %v", err)
		}
	}
	actor := uuid.New()
	customerID := uuid.New()

	// Seed stock so the invoice posting + receipt move both have
	// a non-zero base level to swing against.
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: warehouse.ID,
		Qty:         decimal.NewFromInt(50),
		UnitCost:    decimal.NewFromInt(8),
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   actor,
	}); err != nil {
		t.Fatalf("seed opening stock: %v", err)
	}

	// 1. Create + post the source AR invoice (3 widgets at $20).
	qtySold := decimal.NewFromInt(3)
	unitPrice := decimal.NewFromInt(20)
	arID := createARInvoiceWithInventoryLine(t, h, tn.ID, actor, "AR-RET-001", customerID.String(), item.ID, warehouse.ID, qtySold, unitPrice)
	if _, err := invoicePoster.PostSalesInvoice(ctx, tn.ID, arID, actor); err != nil {
		t.Fatalf("post source invoice: %v", err)
	}

	// 2. Author the return for 2 of the 3 units.
	qtyReturned := decimal.NewFromInt(2)
	retTotal := qtyReturned.Mul(unitPrice)
	retF, _ := retTotal.Float64()
	priceF, _ := unitPrice.Float64()
	qtyF, _ := qtyReturned.Float64()
	retBody, _ := json.Marshal(map[string]any{
		"return_number":       "RET-001",
		"original_invoice_id": arID.String(),
		"customer_id":         customerID.String(),
		"warehouse_id":        warehouse.ID.String(),
		"return_date":         "2026-04-15",
		"reason":              "customer changed mind",
		"lines": []map[string]any{{
			"item_id":    item.ID.String(),
			"qty":        qtyF,
			"unit_price": priceF,
			"unit_cost":  8,
		}},
		"subtotal": retF,
		"total":    retF,
		"currency": "USD",
		"status":   sales.ReturnStatusRequested,
	})
	retRec, err := h.records.Create(ctx, record.KRecord{
		ID:        uuid.New(),
		TenantID:  tn.ID,
		KType:     sales.KTypeSalesReturn,
		Data:      retBody,
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create return record: %v", err)
	}

	poster := sales.NewReturnPoster(h.records, invoicePoster, invStore, ledgerStore)

	// 3. Approve — pure status flip.
	approved, err := poster.Approve(ctx, tn.ID, retRec.ID, actor)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := readStatus(t, approved); got != sales.ReturnStatusApproved {
		t.Fatalf("approve: status=%q, want %q", got, sales.ReturnStatusApproved)
	}

	// 4. Receive — posts one positive-qty inventory move keyed on
	//    the return's UUID via source_ktype=sales.return.
	received, err := poster.Receive(ctx, tn.ID, retRec.ID, actor)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got := readStatus(t, received); got != sales.ReturnStatusReceived {
		t.Fatalf("receive: status=%q, want %q", got, sales.ReturnStatusReceived)
	}
	allMoves, err := invStore.ListMoves(ctx, tn.ID, inventory.MoveFilter{
		ItemID: &item.ID, WarehouseID: &warehouse.ID,
	})
	if err != nil {
		t.Fatalf("list moves: %v", err)
	}
	receiptCount := 0
	var receiptQty decimal.Decimal
	for _, m := range allMoves {
		if m.SourceKType == inventory.MoveSourceSalesReturn && m.SourceID != nil && *m.SourceID == retRec.ID {
			receiptCount++
			receiptQty = receiptQty.Add(m.Qty)
		}
	}
	if receiptCount != 1 {
		t.Fatalf("got %d sales.return moves; want 1", receiptCount)
	}
	if receiptQty.Cmp(qtyReturned) != 0 {
		t.Fatalf("return receipt qty=%s; want %s (positive == stock IN)", receiptQty, qtyReturned)
	}

	// 5. Refund — creates credit_note + JE, stamps both onto the return.
	refunded, err := poster.Refund(ctx, tn.ID, retRec.ID, actor)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	refundedData := readBody(t, refunded)
	if got, _ := refundedData["status"].(string); got != sales.ReturnStatusRefunded {
		t.Fatalf("refund: status=%q, want %q", got, sales.ReturnStatusRefunded)
	}
	creditNoteID, _ := refundedData["credit_note_id"].(string)
	if creditNoteID == "" {
		t.Fatal("refund: credit_note_id not stamped on return")
	}
	jeID, _ := refundedData["journal_entry_id"].(string)
	if jeID == "" {
		t.Fatal("refund: journal_entry_id not stamped on return")
	}
	cnRec, err := h.records.Get(ctx, tn.ID, uuid.MustParse(creditNoteID))
	if err != nil || cnRec == nil {
		t.Fatalf("load credit note: %v (rec=%v)", err, cnRec)
	}
	cnData := readBody(t, cnRec)
	if got, _ := cnData["status"].(string); got != "posted" {
		t.Fatalf("credit_note status=%q; want posted", got)
	}
	if got, _ := cnData["original_invoice_id"].(string); got != arID.String() {
		t.Fatalf("credit_note original_invoice_id=%q; want %s", got, arID)
	}

	// Verify the JE matches the AR/revenue accounts of the source
	// invoice — Dr Revenue, Cr AR — and the totals net to zero.
	je, err := ledgerStore.GetJournalEntry(ctx, tn.ID, uuid.MustParse(jeID))
	if err != nil || je == nil {
		t.Fatalf("load credit-note JE: %v", err)
	}
	if je.SourceKType != finance.KTypeCreditNote {
		t.Fatalf("JE source_ktype=%q; want %q", je.SourceKType, finance.KTypeCreditNote)
	}
	var drRevenue, crAR decimal.Decimal
	for _, ln := range je.Lines {
		switch ln.AccountCode {
		case "4000":
			drRevenue = drRevenue.Add(ln.Debit).Sub(ln.Credit)
		case "1100":
			crAR = crAR.Add(ln.Credit).Sub(ln.Debit)
		}
	}
	if drRevenue.Cmp(retTotal) != 0 {
		t.Fatalf("Revenue debit=%s; want %s", drRevenue, retTotal)
	}
	if crAR.Cmp(retTotal) != 0 {
		t.Fatalf("AR credit=%s; want %s", crAR, retTotal)
	}

	// 6. Idempotency — Receive + Refund replay against the
	//    already-advanced record short-circuit instead of
	//    double-posting. We assert by re-counting moves and
	//    looking up credit_note rows.
	if _, err := poster.Receive(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("receive replay: %v", err)
	}
	if _, err := poster.Refund(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("refund replay: %v", err)
	}
	allMoves2, _ := invStore.ListMoves(ctx, tn.ID, inventory.MoveFilter{
		ItemID: &item.ID, WarehouseID: &warehouse.ID,
	})
	count := 0
	for _, m := range allMoves2 {
		if m.SourceKType == inventory.MoveSourceSalesReturn && m.SourceID != nil && *m.SourceID == retRec.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("receive replay produced %d return moves; want 1", count)
	}
	// Credit-note list must still have exactly one row keyed on
	// this return (resumable allocation reused the stamped id).
	cnRecs, err := h.records.List(ctx, tn.ID, record.ListFilter{KType: finance.KTypeCreditNote, Limit: 50})
	if err != nil {
		t.Fatalf("list credit notes: %v", err)
	}
	cnCount := 0
	for _, r := range cnRecs {
		var d map[string]any
		if err := json.Unmarshal(r.Data, &d); err != nil {
			continue
		}
		if v, _ := d["created_from_return_id"].(string); v == retRec.ID.String() {
			cnCount++
		}
	}
	if cnCount != 1 {
		t.Fatalf("refund replay produced %d credit notes; want 1", cnCount)
	}
}

// TestSalesReturnCannotCancelRefunded verifies that the state
// machine refuses to cancel a refunded return — once the credit
// note has posted, reversing the side-effects requires an explicit
// fresh sales invoice, not a status flip. Mirrors ERPNext's
// "cancel from posted" guard.
func TestSalesReturnCannotCancelRefunded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, ledgerStore, invoicePoster, invStore, item, warehouse := newTenantForInventory(t, h)
	for _, kt := range sales.ReturnKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register sales.return ktype: %v", err)
		}
	}
	actor := uuid.New()
	customerID := uuid.New()

	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: warehouse.ID,
		Qty:         decimal.NewFromInt(50),
		UnitCost:    decimal.NewFromInt(8),
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   actor,
	}); err != nil {
		t.Fatalf("seed opening stock: %v", err)
	}

	arID := createARInvoiceWithInventoryLine(t, h, tn.ID, actor, "AR-RET-002", customerID.String(), item.ID, warehouse.ID, decimal.NewFromInt(2), decimal.NewFromInt(10))
	if _, err := invoicePoster.PostSalesInvoice(ctx, tn.ID, arID, actor); err != nil {
		t.Fatalf("post source invoice: %v", err)
	}

	retBody, _ := json.Marshal(map[string]any{
		"return_number":       "RET-002",
		"original_invoice_id": arID.String(),
		"customer_id":         customerID.String(),
		"warehouse_id":        warehouse.ID.String(),
		"return_date":         "2026-04-16",
		"lines": []map[string]any{{
			"item_id":    item.ID.String(),
			"qty":        1,
			"unit_price": 10,
			"unit_cost":  8,
		}},
		"total":    10.0,
		"currency": "USD",
		"status":   sales.ReturnStatusRequested,
	})
	retRec, err := h.records.Create(ctx, record.KRecord{
		ID: uuid.New(), TenantID: tn.ID, KType: sales.KTypeSalesReturn, Data: retBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create return: %v", err)
	}

	poster := sales.NewReturnPoster(h.records, invoicePoster, invStore, ledgerStore)
	if _, err := poster.Approve(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := poster.Receive(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if _, err := poster.Refund(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if _, err := poster.Cancel(ctx, tn.ID, retRec.ID, actor); err == nil {
		t.Fatal("cancel of refunded return should be rejected")
	}
}

// TestSalesReturnCancelFromReceivedReverses pins the auto-reversal
// behaviour Cancel exhibits when called on a "received" return.
// Receive posts a +qty inventory move; Cancel must post a balancing
// -qty contra row so the warehouse's stock_levels view returns to
// the pre-receive level. The test also re-runs Cancel to confirm
// the operation is idempotent via the
// inventory_moves_reversal_of_uniq partial unique index.
func TestSalesReturnCancelFromReceivedReverses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, ledgerStore, invoicePoster, invStore, item, warehouse := newTenantForInventory(t, h)
	for _, kt := range sales.ReturnKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register sales.return ktype: %v", err)
		}
	}
	actor := uuid.New()
	customerID := uuid.New()

	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: warehouse.ID,
		Qty:         decimal.NewFromInt(50),
		UnitCost:    decimal.NewFromInt(8),
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   actor,
	}); err != nil {
		t.Fatalf("seed opening stock: %v", err)
	}
	arID := createARInvoiceWithInventoryLine(t, h, tn.ID, actor, "AR-RET-003", customerID.String(), item.ID, warehouse.ID, decimal.NewFromInt(3), decimal.NewFromInt(20))
	if _, err := invoicePoster.PostSalesInvoice(ctx, tn.ID, arID, actor); err != nil {
		t.Fatalf("post source invoice: %v", err)
	}

	// Snapshot pre-return stock so we can compare back to baseline.
	preLevels, err := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("snapshot pre-receive stock: %v", err)
	}
	preQty := decimal.Zero
	for _, lvl := range preLevels {
		if lvl.WarehouseID == warehouse.ID {
			preQty = lvl.Qty
		}
	}

	retBody, _ := json.Marshal(map[string]any{
		"return_number":       "RET-003",
		"original_invoice_id": arID.String(),
		"customer_id":         customerID.String(),
		"warehouse_id":        warehouse.ID.String(),
		"return_date":         "2026-04-17",
		"lines": []map[string]any{{
			"item_id":    item.ID.String(),
			"qty":        2,
			"unit_price": 20,
			"unit_cost":  8,
		}},
		"total":    40.0,
		"currency": "USD",
		"status":   sales.ReturnStatusRequested,
	})
	retRec, err := h.records.Create(ctx, record.KRecord{
		ID: uuid.New(), TenantID: tn.ID, KType: sales.KTypeSalesReturn, Data: retBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create return: %v", err)
	}

	poster := sales.NewReturnPoster(h.records, invoicePoster, invStore, ledgerStore)
	if _, err := poster.Approve(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := poster.Receive(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("receive: %v", err)
	}

	// After receive, the warehouse should hold preQty + 2.
	postReceive, _ := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	var postReceiveQty decimal.Decimal
	for _, lvl := range postReceive {
		if lvl.WarehouseID == warehouse.ID {
			postReceiveQty = lvl.Qty
		}
	}
	if postReceiveQty.Cmp(preQty.Add(decimal.NewFromInt(2))) != 0 {
		t.Fatalf("post-receive stock=%s; want %s", postReceiveQty, preQty.Add(decimal.NewFromInt(2)))
	}

	cancelled, err := poster.Cancel(ctx, tn.ID, retRec.ID, actor)
	if err != nil {
		t.Fatalf("cancel from received: %v", err)
	}
	if got := readStatus(t, cancelled); got != sales.ReturnStatusCancelled {
		t.Fatalf("cancel: status=%q, want %q", got, sales.ReturnStatusCancelled)
	}

	// Stock must drain back to the pre-receive baseline. The
	// reversal posts a -qty contra row, not a -qty source-tagged
	// move, so the source-filtered ListMoves still returns exactly
	// one row (the original receipt).
	postCancel, _ := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	var postCancelQty decimal.Decimal
	for _, lvl := range postCancel {
		if lvl.WarehouseID == warehouse.ID {
			postCancelQty = lvl.Qty
		}
	}
	if postCancelQty.Cmp(preQty) != 0 {
		t.Fatalf("post-cancel stock=%s; want %s (reversal must drain receipt)", postCancelQty, preQty)
	}

	// Idempotency — replaying Cancel after the first reversal must
	// not mint a second contra row (the partial unique index
	// inventory_moves_reversal_of_uniq enforces this and Cancel
	// treats ErrAlreadyReversed as success).
	if _, err := poster.Cancel(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("cancel replay: %v", err)
	}
	allMoves, _ := invStore.ListMoves(ctx, tn.ID, inventory.MoveFilter{
		ItemID: &item.ID, WarehouseID: &warehouse.ID,
	})
	contraCount := 0
	for _, m := range allMoves {
		if m.ReversalOf != nil {
			contraCount++
		}
	}
	if contraCount != 1 {
		t.Fatalf("cancel replay produced %d contra rows; want 1", contraCount)
	}
}

// TestSalesReturnCancelDrainsOrphanedReceiptMoves pins the defense
// against a partially-failed Receive: if Receive successfully posted
// inventory moves but the subsequent status `persist` failed (e.g.
// version conflict, transient DB error), the return is left in
// "approved" with orphaned positive-qty moves tagged with the
// return's source_id. A bare status-only Cancel would silently
// leave the warehouse stock inflated by the orphaned receipt.
//
// The test simulates that scenario directly by posting a source-keyed
// inventory move BEFORE calling Cancel from "approved" (without
// running Receive), then asserts that Cancel reversed the orphan
// and drained the stock back to the pre-receipt level.
func TestSalesReturnCancelDrainsOrphanedReceiptMoves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, ledgerStore, invoicePoster, invStore, item, warehouse := newTenantForInventory(t, h)
	for _, kt := range sales.ReturnKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register sales.return ktype: %v", err)
		}
	}
	actor := uuid.New()
	customerID := uuid.New()

	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: warehouse.ID,
		Qty:         decimal.NewFromInt(50),
		UnitCost:    decimal.NewFromInt(8),
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   actor,
	}); err != nil {
		t.Fatalf("seed opening stock: %v", err)
	}
	arID := createARInvoiceWithInventoryLine(t, h, tn.ID, actor, "AR-RET-004", customerID.String(), item.ID, warehouse.ID, decimal.NewFromInt(3), decimal.NewFromInt(20))
	if _, err := invoicePoster.PostSalesInvoice(ctx, tn.ID, arID, actor); err != nil {
		t.Fatalf("post source invoice: %v", err)
	}

	preLevels, _ := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	preQty := decimal.Zero
	for _, lvl := range preLevels {
		if lvl.WarehouseID == warehouse.ID {
			preQty = lvl.Qty
		}
	}

	retBody, _ := json.Marshal(map[string]any{
		"return_number":       "RET-004",
		"original_invoice_id": arID.String(),
		"customer_id":         customerID.String(),
		"warehouse_id":        warehouse.ID.String(),
		"return_date":         "2026-04-18",
		"lines": []map[string]any{{
			"item_id":    item.ID.String(),
			"qty":        2,
			"unit_price": 20,
			"unit_cost":  8,
		}},
		"total":    40.0,
		"currency": "USD",
		"status":   sales.ReturnStatusRequested,
	})
	retRec, err := h.records.Create(ctx, record.KRecord{
		ID: uuid.New(), TenantID: tn.ID, KType: sales.KTypeSalesReturn, Data: retBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create return: %v", err)
	}

	poster := sales.NewReturnPoster(h.records, invoicePoster, invStore, ledgerStore)
	if _, err := poster.Approve(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Simulate the partially-failed Receive: inventory move is
	// committed against the return's source_id, but the status
	// `persist` step never ran so the record stays in "approved".
	// This is the state Cancel must defend against.
	sourceID := retRec.ID
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: warehouse.ID,
		Qty:         decimal.NewFromInt(2),
		UnitCost:    decimal.NewFromInt(8),
		SourceKType: inventory.MoveSourceSalesReturn,
		SourceID:    &sourceID,
		CreatedBy:   actor,
	}); err != nil {
		t.Fatalf("simulate orphaned receipt move: %v", err)
	}

	// Status must still be "approved" (we never ran the persist
	// half of Receive), and the orphaned move must be visible in
	// stock_levels so the test's assertion has something to drain.
	cur, err := h.records.Get(ctx, tn.ID, retRec.ID)
	if err != nil {
		t.Fatalf("reload return: %v", err)
	}
	if got := readStatus(t, cur); got != sales.ReturnStatusApproved {
		t.Fatalf("pre-cancel: status=%q, want %q", got, sales.ReturnStatusApproved)
	}
	inflated, _ := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	var inflatedQty decimal.Decimal
	for _, lvl := range inflated {
		if lvl.WarehouseID == warehouse.ID {
			inflatedQty = lvl.Qty
		}
	}
	if inflatedQty.Cmp(preQty.Add(decimal.NewFromInt(2))) != 0 {
		t.Fatalf("orphan-injection stock=%s; want %s (test setup is wrong)", inflatedQty, preQty.Add(decimal.NewFromInt(2)))
	}

	// Cancel from "approved" must reverse the orphan even though
	// the status branch (received) was never entered.
	cancelled, err := poster.Cancel(ctx, tn.ID, retRec.ID, actor)
	if err != nil {
		t.Fatalf("cancel from approved with orphaned move: %v", err)
	}
	if got := readStatus(t, cancelled); got != sales.ReturnStatusCancelled {
		t.Fatalf("cancel: status=%q, want %q", got, sales.ReturnStatusCancelled)
	}

	postCancel, _ := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	var postCancelQty decimal.Decimal
	for _, lvl := range postCancel {
		if lvl.WarehouseID == warehouse.ID {
			postCancelQty = lvl.Qty
		}
	}
	if postCancelQty.Cmp(preQty) != 0 {
		t.Fatalf("post-cancel stock=%s; want %s (Cancel must drain orphaned receipt regardless of status)", postCancelQty, preQty)
	}
}

// TestSalesReturnRefundAdoptsOrphanedCreditNote pins the recovery
// path for Devin Review finding 3303293709: Refund's intermediate
// Update stamp can fail (version conflict, transient DB error) after
// the credit-note KRecord has already been committed. Without
// adoption, a retry would mint a SECOND credit note and leave the
// first as a draft orphan.
//
// We simulate the partially-failed Refund directly by manually
// committing a finance.credit_note KRecord with
// created_from_return_id set to the return's ID, then leaving the
// return in "received" with no credit_note_id stamp. The retry must:
//
//  1. Find the orphan via ListByField (created_from_return_id key).
//  2. Adopt it (re-use the same KRecord ID).
//  3. Post the credit-note JE against the adopted ID.
//  4. Leave exactly ONE finance.credit_note row keyed to this return
//     (no duplicate from a fresh Create), and stamp credit_note_id +
//     journal_entry_id on the return as the happy path would.
func TestSalesReturnRefundAdoptsOrphanedCreditNote(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, ledgerStore, invoicePoster, invStore, item, warehouse := newTenantForInventory(t, h)
	for _, kt := range sales.ReturnKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register sales.return ktype: %v", err)
		}
	}
	actor := uuid.New()
	customerID := uuid.New()

	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: warehouse.ID,
		Qty:         decimal.NewFromInt(50),
		UnitCost:    decimal.NewFromInt(8),
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   actor,
	}); err != nil {
		t.Fatalf("seed opening stock: %v", err)
	}
	arID := createARInvoiceWithInventoryLine(t, h, tn.ID, actor, "AR-RET-005", customerID.String(), item.ID, warehouse.ID, decimal.NewFromInt(3), decimal.NewFromInt(20))
	if _, err := invoicePoster.PostSalesInvoice(ctx, tn.ID, arID, actor); err != nil {
		t.Fatalf("post source invoice: %v", err)
	}

	retBody, _ := json.Marshal(map[string]any{
		"return_number":       "RET-005",
		"original_invoice_id": arID.String(),
		"customer_id":         customerID.String(),
		"warehouse_id":        warehouse.ID.String(),
		"return_date":         "2026-04-19",
		"lines": []map[string]any{{
			"item_id":    item.ID.String(),
			"qty":        2,
			"unit_price": 20,
			"unit_cost":  8,
		}},
		"total":    40.0,
		"currency": "USD",
		"status":   sales.ReturnStatusRequested,
	})
	retRec, err := h.records.Create(ctx, record.KRecord{
		ID: uuid.New(), TenantID: tn.ID, KType: sales.KTypeSalesReturn, Data: retBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create return: %v", err)
	}

	poster := sales.NewReturnPoster(h.records, invoicePoster, invStore, ledgerStore)
	if _, err := poster.Approve(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := poster.Receive(ctx, tn.ID, retRec.ID, actor); err != nil {
		t.Fatalf("receive: %v", err)
	}

	// Simulate the partially-failed Refund: a finance.credit_note
	// is committed with created_from_return_id pointing back at
	// the return, but the intermediate Update that stamps
	// credit_note_id on the return never ran. This is the exact
	// failure mode the bot's finding calls out (Create succeeded,
	// Update failed) — the orphan is committed but unreferenced.
	orphanID := uuid.New()
	orphanBody, _ := json.Marshal(map[string]any{
		"original_invoice_id":    arID.String(),
		"credit_note_number":     "RET-005",
		"issue_date":             "2026-04-19",
		"reason":                 "Orphaned by partial Refund",
		"amount":                 40.0,
		"currency":               "USD",
		"status":                 "draft",
		"created_from_return_id": retRec.ID.String(),
	})
	if _, err := h.records.Create(ctx, record.KRecord{
		ID: orphanID, TenantID: tn.ID, KType: finance.KTypeCreditNote, Data: orphanBody, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("simulate orphaned credit_note: %v", err)
	}

	// Confirm the orphan is in the database and that the return
	// has no credit_note_id pointer (the test setup mirrors the
	// real partial-failure shape).
	pre, err := h.records.Get(ctx, tn.ID, retRec.ID)
	if err != nil {
		t.Fatalf("reload return pre-refund: %v", err)
	}
	if got, _ := readBody(t, pre)["credit_note_id"].(string); got != "" {
		t.Fatalf("pre-refund: credit_note_id=%q; want empty (test setup wrong)", got)
	}

	// Retry Refund. Must adopt the orphan, NOT mint a second CN.
	refunded, err := poster.Refund(ctx, tn.ID, retRec.ID, actor)
	if err != nil {
		t.Fatalf("refund retry with orphaned credit_note: %v", err)
	}
	refundedBody := readBody(t, refunded)
	if got, _ := refundedBody["status"].(string); got != sales.ReturnStatusRefunded {
		t.Fatalf("post-refund status=%q; want %q", got, sales.ReturnStatusRefunded)
	}
	stampedCNID, _ := refundedBody["credit_note_id"].(string)
	if stampedCNID != orphanID.String() {
		t.Fatalf("refund: credit_note_id=%q; want orphan %s (adoption failed — second CN was minted)", stampedCNID, orphanID)
	}
	if got, _ := refundedBody["journal_entry_id"].(string); got == "" {
		t.Fatal("refund: journal_entry_id not stamped on return after orphan adoption")
	}

	// Exactly one finance.credit_note must exist for this return.
	allCNs, err := h.records.ListByField(ctx, tn.ID,
		record.ListFilter{KType: finance.KTypeCreditNote},
		"created_from_return_id", retRec.ID.String(),
	)
	if err != nil {
		t.Fatalf("list credit_notes by return: %v", err)
	}
	if len(allCNs) != 1 {
		t.Fatalf("post-refund credit_note count=%d; want 1 (a second CN was leaked — orphan adoption did not fire)", len(allCNs))
	}
	if allCNs[0].ID != orphanID {
		t.Fatalf("post-refund credit_note ID=%s; want orphan %s", allCNs[0].ID, orphanID)
	}
}

// readStatus extracts the status field from a sales.return KRecord
// for one-line assertions. Returns "" on parse failure so the
// caller's t.Fatalf line stays readable.
func readStatus(t *testing.T, rec *record.KRecord) string {
	t.Helper()
	body := readBody(t, rec)
	v, _ := body["status"].(string)
	return v
}

// readBody decodes the KRecord's JSONB payload. Reusable so each
// test step can assert on whichever stamped fields are relevant.
func readBody(t *testing.T, rec *record.KRecord) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Data, &body); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	return body
}
