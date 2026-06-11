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
	"github.com/kennguy3n/kapp-fab/internal/manufacturing"
)

// TestWS2LotTrackingDecrement exercises the lot (batch) tracking
// contract end-to-end on the move ledger:
//
//   - a lot-tracked item must reference a batch on every move
//     (ErrLotRequired otherwise);
//   - receipts roll the lot's qty_on_hand up and issues roll it down;
//   - an issue that would drive the lot negative is rejected with
//     ErrInsufficientLotStock and leaves the running total untouched
//     (over-issue is impossible).
func TestWS2LotTrackingDecrement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, inv, _, wh := newTenantForInventory(t, h)
	actor := uuid.New()

	item, err := inv.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "WS2-LOT-ONLY", Name: "Lot Only", UOM: "each",
		Active: true, LotTracked: true,
	})
	if err != nil {
		t.Fatalf("upsert lot item: %v", err)
	}

	// A lot-tracked item rejects a move with no batch.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(1), SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrLotRequired) {
		t.Fatalf("missing batch: want ErrLotRequired, got %v", err)
	}

	batch, err := inv.CreateBatch(ctx, inventory.Batch{
		TenantID: tn.ID, ItemID: item.ID, BatchNo: "LOT-DEC-1", CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}

	// Receive 10.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(10), BatchID: &batch.ID,
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("receive lot: %v", err)
	}
	assertLotQty(t, ctx, inv, tn.ID, batch.ID, "10")

	// Issue 4 → 6 remaining.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(-4), BatchID: &batch.ID,
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("issue lot: %v", err)
	}
	assertLotQty(t, ctx, inv, tn.ID, batch.ID, "6")

	// Over-issue 7 (only 6 on hand) → rejected, balance unchanged.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(-7), BatchID: &batch.ID,
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrInsufficientLotStock) {
		t.Fatalf("over-issue: want ErrInsufficientLotStock, got %v", err)
	}
	assertLotQty(t, ctx, inv, tn.ID, batch.ID, "6")

	// The per-lot stock projection agrees with the running total.
	levels, err := inv.ListStockLevelsByBatch(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list batch stock: %v", err)
	}
	if len(levels) != 1 || !levels[0].Qty.Equal(decimal.NewFromInt(6)) {
		t.Fatalf("batch stock projection = %+v, want single row qty 6", levels)
	}
}

// TestWS2SerialTrackingAndTrace exercises serial tracking and the
// forward/backward traceability queries.
func TestWS2SerialTrackingAndTrace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, inv, plainItem, wh := newTenantForInventory(t, h)
	actor := uuid.New()

	item, err := inv.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "WS2-SN-LOT", Name: "Serial+Lot", UOM: "each",
		Active: true, LotTracked: true, SerialTracked: true,
	})
	if err != nil {
		t.Fatalf("upsert serial item: %v", err)
	}
	batch, err := inv.CreateBatch(ctx, inventory.Batch{
		TenantID: tn.ID, ItemID: item.ID, BatchNo: "LOT-SN-1", CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}

	// Serial count must equal |qty|.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(3), BatchID: &batch.ID,
		SerialNos:   []string{"S1", "S2"}, // only 2 for qty 3
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrSerialQtyMismatch) {
		t.Fatalf("serial qty mismatch: want ErrSerialQtyMismatch, got %v", err)
	}

	// Serials on a non-serial item are rejected.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: plainItem.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(1), SerialNos: []string{"X1"},
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrSerialUnsupported) {
		t.Fatalf("serials on plain item: want ErrSerialUnsupported, got %v", err)
	}

	// Receive 3 serials into the lot.
	recv, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(3), BatchID: &batch.ID,
		SerialNos:   []string{"S1", "S2", "S3"},
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("receive serials: %v", err)
	}
	assertLotQty(t, ctx, inv, tn.ID, batch.ID, "3")

	inStock, err := inv.ListSerials(ctx, tn.ID, inventory.SerialFilter{ItemID: &item.ID, Status: inventory.SerialStatusInStock})
	if err != nil {
		t.Fatalf("list serials: %v", err)
	}
	if len(inStock) != 3 {
		t.Fatalf("in-stock serials = %d, want 3", len(inStock))
	}
	for _, s := range inStock {
		if s.BatchID == nil || *s.BatchID != batch.ID {
			t.Fatalf("serial %s not linked to lot", s.SerialNo)
		}
		if s.WarehouseID == nil || *s.WarehouseID != wh.ID {
			t.Fatalf("serial %s not at warehouse", s.SerialNo)
		}
	}

	// Duplicate intake of an in-stock serial is rejected.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(1), BatchID: &batch.ID,
		SerialNos: []string{"S1"}, SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrSerialAlreadyInStock) {
		t.Fatalf("duplicate intake: want ErrSerialAlreadyInStock, got %v", err)
	}

	// Ship S2 to a customer (delivered).
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(-1), BatchID: &batch.ID,
		SerialNos: []string{"S2"}, SerialOutStatus: inventory.SerialStatusDelivered,
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("ship serial: %v", err)
	}
	assertLotQty(t, ctx, inv, tn.ID, batch.ID, "2")

	s2, err := inv.GetSerial(ctx, tn.ID, item.ID, "S2")
	if err != nil {
		t.Fatalf("get S2: %v", err)
	}
	if s2.Status != inventory.SerialStatusDelivered || s2.WarehouseID != nil {
		t.Fatalf("S2 = %+v, want delivered with nil warehouse", s2)
	}

	// Issuing S2 again is impossible — it already left stock.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(-1), BatchID: &batch.ID,
		SerialNos: []string{"S2"}, SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrSerialNotAvailable) {
		t.Fatalf("re-issue S2: want ErrSerialNotAvailable, got %v", err)
	}

	// Forward/backward trace for S2: receipt then delivery, oldest-first.
	trace, err := inv.TraceSerial(ctx, tn.ID, item.ID, "S2")
	if err != nil {
		t.Fatalf("trace S2: %v", err)
	}
	if len(trace.Events) != 2 {
		t.Fatalf("S2 trace events = %d, want 2", len(trace.Events))
	}
	if !trace.Events[0].Qty.Equal(decimal.NewFromInt(3)) {
		t.Fatalf("S2 origin event qty = %s, want 3 (receipt)", trace.Events[0].Qty)
	}
	if !trace.Events[1].Qty.Equal(decimal.NewFromInt(-1)) {
		t.Fatalf("S2 terminal event qty = %s, want -1 (delivery)", trace.Events[1].Qty)
	}
	if trace.Events[0].MoveID != recv.ID {
		t.Fatalf("S2 origin move id = %d, want receipt %d", trace.Events[0].MoveID, recv.ID)
	}

	// S1 was only received → a single trace event.
	traceS1, err := inv.TraceSerial(ctx, tn.ID, item.ID, "S1")
	if err != nil {
		t.Fatalf("trace S1: %v", err)
	}
	if len(traceS1.Events) != 1 {
		t.Fatalf("S1 trace events = %d, want 1", len(traceS1.Events))
	}

	// Lot trace covers every move that referenced the lot:
	// receipt(+3) and delivery(-1) → 2 events.
	lotTrace, err := inv.TraceLot(ctx, tn.ID, batch.ID)
	if err != nil {
		t.Fatalf("trace lot: %v", err)
	}
	if len(lotTrace.Events) != 2 {
		t.Fatalf("lot trace events = %d, want 2", len(lotTrace.Events))
	}
}

// TestWS2SerialReversal verifies that reversing a serial move restores
// the serial's prior state so the ledger and the serial registry stay
// in lock-step.
func TestWS2SerialReversal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, inv, _, wh := newTenantForInventory(t, h)
	actor := uuid.New()

	item, err := inv.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "WS2-SN-REV", Name: "Serial Rev", UOM: "each",
		Active: true, SerialTracked: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Receive one serial.
	recv, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(1), SerialNos: []string{"R1"},
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	// Issue it.
	issue, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(-1), SerialNos: []string{"R1"},
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Reverse the issue → serial back in stock at the warehouse.
	if _, err := inv.ReverseMove(ctx, tn.ID, issue.ID, actor, "oops"); err != nil {
		t.Fatalf("reverse issue: %v", err)
	}
	s, err := inv.GetSerial(ctx, tn.ID, item.ID, "R1")
	if err != nil {
		t.Fatalf("get R1: %v", err)
	}
	if s.Status != inventory.SerialStatusInStock || s.WarehouseID == nil || *s.WarehouseID != wh.ID {
		t.Fatalf("after reversing issue, R1 = %+v, want in_stock at warehouse", s)
	}

	// Reverse the original receipt → serial leaves stock again.
	if _, err := inv.ReverseMove(ctx, tn.ID, recv.ID, actor, "oops2"); err != nil {
		t.Fatalf("reverse receipt: %v", err)
	}
	s, err = inv.GetSerial(ctx, tn.ID, item.ID, "R1")
	if err != nil {
		t.Fatalf("get R1 again: %v", err)
	}
	if s.Status == inventory.SerialStatusInStock {
		t.Fatalf("after reversing receipt, R1 still in_stock: %+v", s)
	}
}

// TestWS2WorkOrderSerialThreading threads lot/serial tracking through a
// full work-order completion: a serial-tracked component is consumed
// (its serials transition to 'consumed') and the serial-tracked
// finished good is produced (new serials created in stock), with the
// move↔serial junction backing traceability across the production step.
func TestWS2WorkOrderSerialThreading(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, inv, mfg, fg, compA, compB, wh := newTenantForManufacturing(t, h)
	actor := uuid.New()

	// Flip the finished good and component A to serial-tracked.
	if _, err := inv.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: fg.SKU, Name: fg.Name, UOM: fg.UOM, Active: true, SerialTracked: true,
	}); err != nil {
		t.Fatalf("flip fg serial: %v", err)
	}
	if _, err := inv.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: compA.SKU, Name: compA.Name, UOM: compA.UOM, Active: true, SerialTracked: true,
	}); err != nil {
		t.Fatalf("flip compA serial: %v", err)
	}

	// Pre-receipt: 2 serials for component A, plain stock for B.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: compA.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(2), SerialNos: []string{"CA1", "CA2"},
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("receive compA serials: %v", err)
	}
	preReceiptStock(t, ctx, inv, tn.ID, actor, compB.ID, wh.ID, "50")

	// BOM: 1x compA + 3x compB per finished unit.
	bom, err := mfg.CreateBOM(ctx, tn.ID, actor, manufacturing.CreateBOMInput{
		ItemID: fg.ID, Version: "v1", OutputQty: decimal.NewFromInt(1), UOM: "each",
		Components: []manufacturing.BOMComponent{
			{ComponentItemID: compA.ID, Qty: decimal.NewFromInt(1), UOM: "each"},
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
		ItemID: fg.ID, WarehouseID: wh.ID, PlannedQty: decimal.NewFromInt(2),
	})
	if err != nil {
		t.Fatalf("create wo: %v", err)
	}
	if _, err := mfg.ReleaseWorkOrder(ctx, tn.ID, wo.ID); err != nil {
		t.Fatalf("release wo: %v", err)
	}

	// Completing without the required serials must fail up front
	// (before the status flip) so the work order can be retried.
	if _, err := mfg.CompleteWorkOrder(ctx, tn.ID, wo.ID, actor, manufacturing.CompleteWorkOrderInput{
		ActualQty: decimal.NewFromInt(2),
	}); !errors.Is(err, inventory.ErrSerialRequired) {
		t.Fatalf("complete without serials: want ErrSerialRequired, got %v", err)
	}

	// Complete with the consumed component serials and the produced
	// finished-good serials.
	done, err := mfg.CompleteWorkOrder(ctx, tn.ID, wo.ID, actor, manufacturing.CompleteWorkOrderInput{
		ActualQty:        decimal.NewFromInt(2),
		ComponentSerials: map[uuid.UUID][]string{compA.ID: {"CA1", "CA2"}},
		FinishedSerials:  []string{"FG1", "FG2"},
	})
	if err != nil {
		t.Fatalf("complete wo: %v", err)
	}
	if done.Status != manufacturing.WorkOrderStatusCompleted {
		t.Fatalf("wo status = %s, want completed", done.Status)
	}

	// Component A serials are consumed and out of stock.
	for _, sn := range []string{"CA1", "CA2"} {
		s, err := inv.GetSerial(ctx, tn.ID, compA.ID, sn)
		if err != nil {
			t.Fatalf("get %s: %v", sn, err)
		}
		if s.Status != inventory.SerialStatusConsumed || s.WarehouseID != nil {
			t.Fatalf("%s = %+v, want consumed with nil warehouse", sn, s)
		}
	}

	// Finished-good serials are in stock at the work-order warehouse.
	fgSerials, err := inv.ListSerials(ctx, tn.ID, inventory.SerialFilter{ItemID: &fg.ID, Status: inventory.SerialStatusInStock})
	if err != nil {
		t.Fatalf("list fg serials: %v", err)
	}
	if len(fgSerials) != 2 {
		t.Fatalf("fg in-stock serials = %d, want 2", len(fgSerials))
	}

	// Backward trace from a finished-good serial reaches the
	// work-order receipt move.
	trace, err := inv.TraceSerial(ctx, tn.ID, fg.ID, "FG1")
	if err != nil {
		t.Fatalf("trace FG1: %v", err)
	}
	if len(trace.Events) != 1 {
		t.Fatalf("FG1 trace events = %d, want 1", len(trace.Events))
	}
	if trace.Events[0].SourceKType != manufacturing.MoveSourceWorkOrderReceipt {
		t.Fatalf("FG1 origin source = %q, want %q", trace.Events[0].SourceKType, manufacturing.MoveSourceWorkOrderReceipt)
	}
	if trace.Events[0].SourceID == nil || *trace.Events[0].SourceID != wo.ID {
		t.Fatalf("FG1 origin should point at work order %s", wo.ID)
	}

	// Forward trace from a component serial reaches the consumption
	// move tagged with the same work order.
	caTrace, err := inv.TraceSerial(ctx, tn.ID, compA.ID, "CA1")
	if err != nil {
		t.Fatalf("trace CA1: %v", err)
	}
	if len(caTrace.Events) != 2 {
		t.Fatalf("CA1 trace events = %d, want 2 (receipt + consume)", len(caTrace.Events))
	}
	last := caTrace.Events[len(caTrace.Events)-1]
	if last.SourceKType != manufacturing.MoveSourceWorkOrderConsume || last.SourceID == nil || *last.SourceID != wo.ID {
		t.Fatalf("CA1 terminal event = %+v, want work-order consume for %s", last, wo.ID)
	}
}

// TestWS2TransferLotSerialRelocation verifies that a warehouse
// transfer of a lot/serial-tracked item relocates the serials in the
// registry (issue-out of source, re-stock at destination) and keeps the
// per-warehouse ledger and serial location consistent end-to-end.
func TestWS2TransferLotSerialRelocation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, inv, _, wh := newTenantForInventory(t, h)
	actor := uuid.New()

	item, err := inv.UpsertItem(ctx, inventory.Item{
		TenantID: tn.ID, SKU: "WS2-XFER-SN", Name: "Transferable Serial", UOM: "each",
		Active: true, LotTracked: true, SerialTracked: true,
	})
	if err != nil {
		t.Fatalf("upsert item: %v", err)
	}
	wh2, err := inv.UpsertWarehouse(ctx, inventory.Warehouse{
		TenantID: tn.ID, Code: "WH-DEST", Name: "Destination Warehouse",
	})
	if err != nil {
		t.Fatalf("seed dest warehouse: %v", err)
	}
	batch, err := inv.CreateBatch(ctx, inventory.Batch{
		TenantID: tn.ID, ItemID: item.ID, BatchNo: "XFER-LOT-1", CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}

	// Receive 3 serials into the source warehouse.
	if _, err := inv.RecordMove(ctx, inventory.Move{
		TenantID: tn.ID, ItemID: item.ID, WarehouseID: wh.ID,
		Qty: decimal.NewFromInt(3), BatchID: &batch.ID,
		SerialNos:   []string{"T1", "T2", "T3"},
		SourceKType: inventory.MoveSourceAdjustment, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("receive serials: %v", err)
	}

	// A serial-tracked transfer with no serials is rejected up front.
	if _, err := inv.RecordTransfer(ctx, inventory.Transfer{
		TenantID: tn.ID, ItemID: item.ID, FromWarehouse: wh.ID, ToWarehouse: wh2.ID,
		Qty: decimal.NewFromInt(1), BatchID: &batch.ID, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrSerialRequired) {
		t.Fatalf("transfer w/o serials: want ErrSerialRequired, got %v", err)
	}

	// Transferring a serial that isn't at the source warehouse is
	// impossible: issueSerial guards on warehouse_id.
	if _, err := inv.RecordTransfer(ctx, inventory.Transfer{
		TenantID: tn.ID, ItemID: item.ID, FromWarehouse: wh2.ID, ToWarehouse: wh.ID,
		Qty: decimal.NewFromInt(1), BatchID: &batch.ID, SerialNos: []string{"T1"}, CreatedBy: actor,
	}); !errors.Is(err, inventory.ErrSerialNotAvailable) {
		t.Fatalf("transfer from wrong wh: want ErrSerialNotAvailable, got %v", err)
	}

	// Relocate T1 and T2 to the destination warehouse.
	if _, err := inv.RecordTransfer(ctx, inventory.Transfer{
		TenantID: tn.ID, ItemID: item.ID, FromWarehouse: wh.ID, ToWarehouse: wh2.ID,
		Qty: decimal.NewFromInt(2), BatchID: &batch.ID, SerialNos: []string{"T1", "T2"}, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("transfer serials: %v", err)
	}

	// The lot's global qty_on_hand is conserved by the balanced legs.
	assertLotQty(t, ctx, inv, tn.ID, batch.ID, "3")

	// T1/T2 are now in stock at the destination; T3 stays at source.
	for _, sn := range []string{"T1", "T2"} {
		s, err := inv.GetSerial(ctx, tn.ID, item.ID, sn)
		if err != nil {
			t.Fatalf("get %s: %v", sn, err)
		}
		if s.Status != inventory.SerialStatusInStock || s.WarehouseID == nil || *s.WarehouseID != wh2.ID {
			t.Fatalf("%s = %+v, want in_stock at dest warehouse", sn, s)
		}
	}
	t3, err := inv.GetSerial(ctx, tn.ID, item.ID, "T3")
	if err != nil {
		t.Fatalf("get T3: %v", err)
	}
	if t3.WarehouseID == nil || *t3.WarehouseID != wh.ID {
		t.Fatalf("T3 = %+v, want still at source warehouse", t3)
	}

	// The per-warehouse, per-lot projection reflects the relocation:
	// source nets 1 unit, destination holds 2.
	levels, err := inv.ListStockLevelsByBatch(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list batch stock: %v", err)
	}
	byWh := map[uuid.UUID]decimal.Decimal{}
	for _, l := range levels {
		byWh[l.WarehouseID] = l.Qty
	}
	if got := byWh[wh.ID]; !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("source lot qty = %s, want 1", got)
	}
	if got := byWh[wh2.ID]; !got.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("dest lot qty = %s, want 2", got)
	}

	// T1's trace spans receipt → transfer-out → transfer-in.
	trace, err := inv.TraceSerial(ctx, tn.ID, item.ID, "T1")
	if err != nil {
		t.Fatalf("trace T1: %v", err)
	}
	if len(trace.Events) != 3 {
		t.Fatalf("T1 trace events = %d, want 3 (receipt + transfer out + in)", len(trace.Events))
	}
	if trace.Events[1].SourceKType != inventory.MoveSourceTransfer ||
		trace.Events[2].SourceKType != inventory.MoveSourceTransfer {
		t.Fatalf("T1 transfer legs mis-tagged: %+v", trace.Events)
	}
}

func assertLotQty(t *testing.T, ctx context.Context, inv *inventory.PGStore, tenantID, batchID uuid.UUID, want string) {
	t.Helper()
	b, err := inv.GetBatch(ctx, tenantID, batchID)
	if err != nil {
		t.Fatalf("get batch: %v", err)
	}
	w, _ := decimal.NewFromString(want)
	if !b.QtyOnHand.Equal(w) {
		t.Fatalf("lot qty_on_hand = %s, want %s", b.QtyOnHand.String(), want)
	}
}
