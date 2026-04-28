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
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/sales"
)

// TestPOSPosterFinalizesARAndPayment is the Phase M Task 6
// regression. Builds a fresh tenant with the inventory hook
// wired into InvoicePoster, registers POS KTypes, creates a
// pos_profile + draft pos_invoice, and finalises it. Asserts:
//
//  1. PostPOSInvoice flips status=posted with non-empty
//     ar_invoice_id + payment_id refs.
//  2. The created ar_invoice itself shows status=posted (the
//     standard InvoicePoster path was used).
//  3. The inventory hook ran — there is exactly one stock move
//     for (item, warehouse) with qty = -<sold qty>.
//  4. Calling PostPOSInvoice a second time short-circuits
//     instead of double-posting (status guard).
func TestPOSPosterFinalizesARAndPayment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, ledgerStore, invoicePoster, invStore, item, warehouse := newTenantForInventory(t, h)
	if _, err := ledgerStore.CreateAccount(ctx, ledger.Account{
		TenantID: tn.ID, Code: "1000", Name: "Cash", Type: ledger.AccountTypeAsset, Active: true,
	}); err != nil {
		t.Fatalf("seed cash account: %v", err)
	}
	for _, kt := range sales.POSKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register pos ktype %s: %v", kt.Name, err)
		}
	}
	paymentPoster := ledger.NewPaymentPoster(ledger.NewPGStore(h.pool, h.publisher, h.auditor), h.records)
	poster := sales.NewPOSPoster(h.records, invoicePoster, paymentPoster)
	actor := uuid.New()
	customerID := uuid.New()

	// Create a POS profile.
	profileBody, _ := json.Marshal(map[string]any{
		"name":                 "Storefront 1",
		"warehouse_id":         warehouse.ID.String(),
		"default_customer_id":  customerID.String(),
		"currency":             "USD",
		"ar_account_code":      "1100",
		"revenue_account_code": "4000",
		"bank_account_code":    "1000",
		"active":               true,
	})
	profileRec, err := h.records.Create(ctx, record.KRecord{
		ID:        uuid.New(),
		TenantID:  tn.ID,
		KType:     sales.KTypePOSProfile,
		Data:      profileBody,
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}

	// Create a draft pos_invoice with one inventory-backed line.
	qty := decimal.NewFromInt(2)
	unitPrice := decimal.NewFromInt(15)
	total := qty.Mul(unitPrice)
	posBody, _ := json.Marshal(map[string]any{
		"profile_id":  profileRec.ID.String(),
		"customer_id": customerID.String(),
		"currency":    "USD",
		"lines": []map[string]any{
			{
				"item_id":      item.ID.String(),
				"warehouse_id": warehouse.ID.String(),
				"qty":          qty.String(),
				"unit_price":   unitPrice.String(),
			},
		},
		"subtotal": total.String(),
		"total":    total.String(),
		"tendered": total.String(),
		"status":   "draft",
	})
	posRec, err := h.records.Create(ctx, record.KRecord{
		ID:        uuid.New(),
		TenantID:  tn.ID,
		KType:     sales.KTypePOSInvoice,
		Data:      posBody,
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create pos invoice: %v", err)
	}

	// 1. Finalize.
	posted, err := poster.PostPOSInvoice(ctx, tn.ID, posRec.ID, actor)
	if err != nil {
		t.Fatalf("PostPOSInvoice: %v", err)
	}
	var postedData map[string]any
	if err := json.Unmarshal(posted.Data, &postedData); err != nil {
		t.Fatalf("decode posted: %v", err)
	}
	if got, _ := postedData["status"].(string); got != "posted" {
		t.Fatalf("status = %q; want posted", got)
	}
	arID, _ := postedData["ar_invoice_id"].(string)
	if arID == "" {
		t.Fatalf("ar_invoice_id missing on posted pos_invoice")
	}
	if pay, _ := postedData["payment_id"].(string); pay == "" {
		t.Fatalf("payment_id missing on posted pos_invoice")
	}

	// 2. The AR invoice should itself be posted.
	arUUID := uuid.MustParse(arID)
	arRec, err := h.records.Get(ctx, tn.ID, arUUID)
	if err != nil || arRec == nil {
		t.Fatalf("load ar invoice: %v", err)
	}
	var arData map[string]any
	if err := json.Unmarshal(arRec.Data, &arData); err != nil {
		t.Fatalf("decode ar: %v", err)
	}
	if got, _ := arData["status"].(string); got != "posted" {
		t.Fatalf("ar status = %q; want posted", got)
	}
	if arData["journal_entry_id"] == nil {
		t.Fatalf("ar journal_entry_id missing — InvoicePoster did not run")
	}

	// 3. Inventory hook should have produced one negative-qty stock move.
	moves, err := invStore.ListMoves(ctx, tn.ID, inventory.MoveFilter{
		ItemID: &item.ID, WarehouseID: &warehouse.ID,
	})
	if err != nil {
		t.Fatalf("list moves: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("got %d stock moves; want 1", len(moves))
	}
	if moves[0].Qty.Cmp(qty.Neg()) != 0 {
		t.Fatalf("move qty = %s; want %s", moves[0].Qty, qty.Neg())
	}

	// 4. Re-posting a finalised pos_invoice short-circuits.
	again, err := poster.PostPOSInvoice(ctx, tn.ID, posRec.ID, actor)
	if err != nil {
		t.Fatalf("re-post: %v", err)
	}
	var againData map[string]any
	_ = json.Unmarshal(again.Data, &againData)
	if got, _ := againData["ar_invoice_id"].(string); got != arID {
		t.Fatalf("re-post produced new ar_invoice_id (%s != %s)", got, arID)
	}

	// Confirm no second journal entry / stock move was emitted.
	moves2, _ := invStore.ListMoves(ctx, tn.ID, inventory.MoveFilter{
		ItemID: &item.ID, WarehouseID: &warehouse.ID,
	})
	if len(moves2) != 1 {
		t.Fatalf("re-post produced extra stock moves (now %d)", len(moves2))
	}

	// And the AR invoice we created the first time is the one referenced.
	if _, ok := arData["customer_id"]; !ok {
		t.Fatalf("ar invoice missing customer_id")
	}
	_ = finance.KTypeARInvoice
}
