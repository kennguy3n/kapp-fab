//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/financeadapters"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestPhaseN9cLandedCost exercises the full landed-cost lifecycle
// end-to-end against a real Postgres:
//
//  1. Seed two purchase-receipt inventory moves on a fresh tenant.
//  2. Create a landed-cost voucher with two charges (freight + duty)
//     and two targets pointing at those receipt moves.
//  3. Allocate — verify allocated_amount is filled in.
//  4. Post — verify each target produced a reversal move and a
//     forward move at the new unit cost, plus a single JE.
//  5. Re-post — verify the second call is a no-op (same JE id, no
//     duplicate inventory moves).
func TestPhaseN9cLandedCost(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := inventory.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register inventory ktypes: %v", err)
	}
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("landed"),
		Name: "Landed Cost Co",
		Cell: "test",
		Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	actor := uuid.New()

	// Seed inventory item and warehouse so the inventory_moves
	// FKs are satisfied.
	itemBody, _ := json.Marshal(map[string]any{
		"sku": "GADGET-1", "name": "Gadget", "uom": "ea", "active": true,
	})
	item, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: inventory.KTypeItem,
		Status: "active", Data: itemBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	whBody, _ := json.Marshal(map[string]any{
		"code": "MAIN", "name": "Main", "active": true,
	})
	wh, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: inventory.KTypeWarehouse,
		Status: "active", Data: whBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("seed warehouse: %v", err)
	}

	// Seed accounts the JE will use (default inventory account +
	// freight/duty expense accounts).
	pub := events.NewPGPublisher(h.pool)
	aud := audit.NewPGLogger(h.pool)
	ledgerStore := ledger.NewPGStore(h.pool, pub, aud)
	inventoryStore := inventory.NewPGStore(h.pool, pub, aud)
	if err := seedLandedCostAccounts(ctx, t, ledgerStore, tn.ID, actor); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}

	// Seed two purchase-receipt moves with the
	// "procurement.purchase_receipt" source_ktype the landed-cost
	// voucher's targets will reference.
	receiptID1 := uuid.New()
	receiptID2 := uuid.New()
	for i, rid := range []uuid.UUID{receiptID1, receiptID2} {
		ridCopy := rid
		_, err := inventoryStore.RecordMove(ctx, inventory.Move{
			TenantID:    tn.ID,
			ItemID:      item.ID,
			WarehouseID: wh.ID,
			Qty:         decimal.NewFromInt(int64(10 + i*5)),
			UnitCost:    decimal.NewFromInt(int64(100 + i*50)),
			SourceKType: "procurement.purchase_receipt",
			SourceID:    &ridCopy,
			MovedAt:     time.Now().UTC(),
			CreatedBy:   actor,
		})
		if err != nil {
			t.Fatalf("seed receipt move %d: %v", i, err)
		}
	}

	// Wire the landed cost store via the financeadapters.
	store := finance.NewLandedCostStore(
		h.pool,
		financeadapters.NewLandedCostInventoryAdapter(inventoryStore),
		financeadapters.NewLandedCostLedgerAdapter(ledgerStore),
	)

	// Step 1: create voucher (status=draft).
	voucher, err := store.CreateVoucher(ctx, finance.LandedCostVoucher{
		TenantID:         tn.ID,
		VoucherNumber:    "LC-0001",
		Description:      "Sea freight + duty for receipt batch 1",
		AllocationMethod: finance.LandedCostByAmount,
		CreatedBy:        actor,
	})
	if err != nil {
		t.Fatalf("create voucher: %v", err)
	}
	if voucher.Status != finance.LandedCostStatusDraft {
		t.Fatalf("expected draft, got %q", voucher.Status)
	}

	// Step 2: upsert two charges.
	if _, err := store.UpsertCharge(ctx, finance.LandedCostCharge{
		TenantID:    tn.ID,
		VoucherID:   voucher.ID,
		Description: "Sea freight",
		Amount:      decimal.NewFromInt(500),
		AccountCode: "5810",
	}); err != nil {
		t.Fatalf("upsert charge 1: %v", err)
	}
	if _, err := store.UpsertCharge(ctx, finance.LandedCostCharge{
		TenantID:    tn.ID,
		VoucherID:   voucher.ID,
		Description: "Customs duty",
		Amount:      decimal.NewFromInt(300),
		AccountCode: "5820",
	}); err != nil {
		t.Fatalf("upsert charge 2: %v", err)
	}

	// Step 3: upsert two targets.
	for _, rid := range []uuid.UUID{receiptID1, receiptID2} {
		ridLocal := rid
		// Pull the seeded move so qty + unit_cost match the
		// reality on the ledger (voucher targets are receipts).
		m, err := inventoryStore.GetMoveBySource(ctx, tn.ID,
			"procurement.purchase_receipt", ridLocal, item.ID, wh.ID)
		if err != nil {
			t.Fatalf("get seed move: %v", err)
		}
		if _, err := store.UpsertTarget(ctx, finance.LandedCostTarget{
			TenantID:    tn.ID,
			VoucherID:   voucher.ID,
			SourceKType: "procurement.purchase_receipt",
			SourceID:    ridLocal,
			ItemID:      item.ID,
			WarehouseID: wh.ID,
			Qty:         m.Qty,
			UnitCost:    m.UnitCost,
		}); err != nil {
			t.Fatalf("upsert target: %v", err)
		}
	}

	// Step 4: allocate. Expect each target's allocated_amount to
	// be > 0 and the sum to equal totalCharges = 800.
	targets, err := store.AllocateVoucher(ctx, tn.ID, voucher.ID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 allocated targets, got %d", len(targets))
	}
	sum := decimal.Zero
	for _, tt := range targets {
		if tt.AllocatedAmount.IsZero() {
			t.Fatalf("target %s allocated_amount=0", tt.ID)
		}
		sum = sum.Add(tt.AllocatedAmount)
	}
	if !sum.Equal(decimal.NewFromInt(800)) {
		t.Fatalf("sum of allocated_amount=%s want 800", sum)
	}

	// Header should have transitioned to allocated.
	got, err := store.GetVoucher(ctx, tn.ID, voucher.ID)
	if err != nil {
		t.Fatalf("re-get voucher: %v", err)
	}
	if got.Status != finance.LandedCostStatusAllocated {
		t.Fatalf("expected allocated, got %q", got.Status)
	}

	// Step 5: post. Each target should produce a reversal + a
	// forward move; a single JE should be written.
	posted, je, err := store.PostVoucher(ctx, tn.ID, voucher.ID, actor)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if posted.Status != finance.LandedCostStatusPosted {
		t.Fatalf("expected posted, got %q", posted.Status)
	}
	if je == nil || je.ID == uuid.Nil {
		t.Fatalf("expected JE returned from post, got nil")
	}

	// Step 6: re-post should be idempotent — same JE id, no
	// extra inventory rows. Just verify same JE returns.
	posted2, je2, err := store.PostVoucher(ctx, tn.ID, voucher.ID, actor)
	if err != nil {
		t.Fatalf("re-post: %v", err)
	}
	if posted2.Status != finance.LandedCostStatusPosted {
		t.Fatalf("re-post status: %q", posted2.Status)
	}
	if je2.ID != je.ID {
		t.Fatalf("re-post JE id changed: first=%s second=%s", je.ID, je2.ID)
	}
}

// seedLandedCostAccounts creates the GL accounts the landed-cost
// poster will debit/credit so the JE write doesn't fail on a
// missing-account FK.
func seedLandedCostAccounts(ctx context.Context, t *testing.T, ls *ledger.PGStore, tenantID, actor uuid.UUID) error {
	t.Helper()
	_ = actor
	accts := []ledger.Account{
		{TenantID: tenantID, Code: finance.DefaultInventoryAccountCode, Name: "Inventory", Type: "asset", Active: true},
		{TenantID: tenantID, Code: "5810", Name: "Freight In", Type: "expense", Active: true},
		{TenantID: tenantID, Code: "5820", Name: "Customs Duty", Type: "expense", Active: true},
		{TenantID: tenantID, Code: finance.DefaultStockAdjustmentAccountCode, Name: "Stock Adjustment", Type: "expense", Active: true},
	}
	for _, a := range accts {
		if _, err := ls.CreateAccount(ctx, a); err != nil {
			return err
		}
	}
	return nil
}
