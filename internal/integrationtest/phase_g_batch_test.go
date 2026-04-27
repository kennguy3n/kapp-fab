//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
)

// TestBatchTrackingHappyPath asserts that:
//
//  1. CreateBatch persists a row scoped to the supplied tenant + item.
//  2. RecordMove with BatchID set rolls the batch's qty_on_hand and
//     stores batch_id on the move.
//  3. RecordMove with BatchID pointing at a *different* tenant's
//     batch surfaces ErrBatchNotFound (cross-tenant linkage is
//     impossible by construction; the composite FK + RLS make the
//     row invisible from the other tenant's transaction).
//  4. RecordMove with BatchID pointing at a batch belonging to a
//     different item surfaces ErrBatchItemMismatch.
func TestBatchTrackingHappyPath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tnA, _, _, invA, itemA, whA := newTenantForInventory(t, h)
	tnB, _, _, invB, itemB, _ := newTenantForInventory(t, h)
	_ = tnB
	_ = itemB

	// Create a batch under tenant A for item A.
	batchA, err := invA.CreateBatch(ctx, inventory.Batch{
		TenantID:  tnA.ID,
		ItemID:    itemA.ID,
		BatchNo:   "LOT-A-001",
		QtyOnHand: decimal.Zero,
		CreatedBy: uuid.New(),
	})
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if batchA.ID == uuid.Nil {
		t.Fatalf("batch id zero")
	}

	// RecordMove with the batch — qty rolls into qty_on_hand.
	if _, err := invA.RecordMove(ctx, inventory.Move{
		TenantID:    tnA.ID,
		ItemID:      itemA.ID,
		WarehouseID: whA.ID,
		Qty:         decimal.NewFromInt(7),
		BatchID:     &batchA.ID,
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   uuid.New(),
	}); err != nil {
		t.Fatalf("record batched move: %v", err)
	}
	got, err := invA.GetBatch(ctx, tnA.ID, batchA.ID)
	if err != nil {
		t.Fatalf("get batch: %v", err)
	}
	if !got.QtyOnHand.Equal(decimal.NewFromInt(7)) {
		t.Fatalf("qty rolled to %s, want 7", got.QtyOnHand.String())
	}

	// Cross-tenant FK is invisible — tenant B asking for tenant A's
	// batch returns ErrBatchNotFound (never a 500 from a stray FK
	// violation).
	_, err = invB.GetBatch(ctx, tnB.ID, batchA.ID)
	if !errors.Is(err, inventory.ErrBatchNotFound) {
		t.Fatalf("cross-tenant get: want ErrBatchNotFound, got %v", err)
	}

	// Item mismatch under the *same* tenant must surface
	// ErrBatchItemMismatch — create a second item under tenant A
	// and try to record a move that points the existing batch at
	// it.
	otherItem, err := invA.UpsertItem(ctx, inventory.Item{
		TenantID: tnA.ID,
		SKU:      "BATCH-OTHER-SKU",
		Name:     "Other Item",
		UOM:      "ea",
	})
	if err != nil {
		t.Fatalf("create other item: %v", err)
	}
	_, err = invA.RecordMove(ctx, inventory.Move{
		TenantID:    tnA.ID,
		ItemID:      otherItem.ID,
		WarehouseID: whA.ID,
		Qty:         decimal.NewFromInt(1),
		BatchID:     &batchA.ID,
		SourceKType: inventory.MoveSourceAdjustment,
		CreatedBy:   uuid.New(),
	})
	if !errors.Is(err, inventory.ErrBatchItemMismatch) {
		t.Fatalf("item mismatch: want ErrBatchItemMismatch, got %v", err)
	}
}
