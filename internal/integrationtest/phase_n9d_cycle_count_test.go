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

	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestPhaseN9dCycleCount exercises the full lifecycle of a cycle
// count against a live Postgres:
//
//  1. Seed an item + warehouse + +10 baseline stock move.
//  2. Create draft session, seed expected_qty from stock_levels.
//  3. Update line counted_qty to 8 (a -2 variance).
//  4. Transition draft → counting → reconciled → posted.
//  5. Verify a single inventory_move with source_ktype =
//     "inventory.cycle_count" and qty = -2 was written.
//  6. Verify stock_levels reports the corrected qty = 8.
//  7. Post again (idempotent retry) — no duplicate move written.
//  8. Verify a cycle-count session with no variance produces zero
//     additional moves (the all-zero-variance branch must skip
//     the per-line poster but still mark the session posted).
func TestPhaseN9dCycleCount(t *testing.T) {
	h := newHarness(t)
	tn, actorID := newTenantForCycleCount(t, h)
	ctx := context.Background()

	invStore := inventory.NewPGStore(h.pool, h.publisher, h.auditor)
	ccStore := inventory.NewCycleCountStore(h.pool, invStore)

	item, wh := seedCycleCountFixture(t, h, tn.ID, actorID, "WIDGET-N9D", "MAIN-N9D")

	// Baseline: +10 on hand.
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: wh.ID,
		Qty:         decimal.NewFromInt(10),
		SourceKType: "test.seed",
		MovedAt:     time.Now().UTC(),
		CreatedBy:   actorID,
	}); err != nil {
		t.Fatalf("seed stock move: %v", err)
	}

	// Step 2: create session, seed from stock_levels.
	session, err := ccStore.CreateSession(ctx, inventory.CycleCountSession{
		TenantID:    tn.ID,
		Code:        "CC-2026-001",
		Description: "Integration test cycle count",
		WarehouseID: wh.ID,
		CreatedBy:   actorID,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.Status != inventory.CycleCountStatusDraft {
		t.Fatalf("new session status = %q, want draft", session.Status)
	}
	if err := ccStore.SeedExpectedFromStock(ctx, tn.ID, session.ID); err != nil {
		t.Fatalf("seed expected: %v", err)
	}
	lines, err := ccStore.ListLines(ctx, tn.ID, session.ID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 seeded line, got %d", len(lines))
	}
	if got, want := lines[0].ExpectedQty.String(), "10"; got != want {
		t.Fatalf("expected_qty = %q, want %q", got, want)
	}

	// Step 2b: SeedExpectedFromStock is idempotent. Re-seeding must
	// not create a duplicate row per item, and must refresh
	// expected_qty to reflect any inventory moves that landed
	// between session creation and the re-seed. We bump on-hand to
	// 12 (10 + 2 extra) and re-seed.
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: wh.ID,
		Qty:         decimal.NewFromInt(2),
		SourceKType: "test.topup",
		MovedAt:     time.Now().UTC(),
		CreatedBy:   actorID,
	}); err != nil {
		t.Fatalf("topup move: %v", err)
	}
	if err := ccStore.SeedExpectedFromStock(ctx, tn.ID, session.ID); err != nil {
		t.Fatalf("re-seed expected: %v", err)
	}
	reseeded, err := ccStore.ListLines(ctx, tn.ID, session.ID)
	if err != nil {
		t.Fatalf("list lines after re-seed: %v", err)
	}
	if len(reseeded) != 1 {
		t.Fatalf("re-seed created duplicate lines: got %d, want 1", len(reseeded))
	}
	if got, want := reseeded[0].ExpectedQty.String(), "12"; got != want {
		t.Fatalf("re-seed expected_qty = %q, want %q", got, want)
	}
	// Back out the topup so the rest of the test sees the original
	// 10 baseline — operator's eventual count of 8 should still
	// produce a -2 variance against the post-seed expected of 12,
	// which means we expect -4 final variance after this point.
	// Simpler: post a -2 reversal here.
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: wh.ID,
		Qty:         decimal.NewFromInt(-2),
		SourceKType: "test.topup.reverse",
		MovedAt:     time.Now().UTC(),
		CreatedBy:   actorID,
	}); err != nil {
		t.Fatalf("reverse topup move: %v", err)
	}
	// Re-seed once more to bring expected_qty back to 10 for the
	// downstream assertions.
	if err := ccStore.SeedExpectedFromStock(ctx, tn.ID, session.ID); err != nil {
		t.Fatalf("final re-seed: %v", err)
	}
	final, err := ccStore.ListLines(ctx, tn.ID, session.ID)
	if err != nil {
		t.Fatalf("list lines after final re-seed: %v", err)
	}
	if len(final) != 1 {
		t.Fatalf("final re-seed produced %d lines, want 1", len(final))
	}
	if got, want := final[0].ExpectedQty.String(), "10"; got != want {
		t.Fatalf("final expected_qty = %q, want %q", got, want)
	}
	lines = final

	// Step 3: operator counts 8 (a -2 variance).
	updated, err := ccStore.UpsertLine(ctx, inventory.CycleCountLine{
		TenantID:    tn.ID,
		ID:          lines[0].ID,
		SessionID:   session.ID,
		ItemID:      item.ID,
		ExpectedQty: lines[0].ExpectedQty,
		CountedQty:  decimal.NewFromInt(8),
		Notes:       "missing two units in bin A1",
	})
	if err != nil {
		t.Fatalf("upsert line: %v", err)
	}
	if got, want := updated.Variance.String(), "-2"; got != want {
		t.Fatalf("variance = %q, want %q", got, want)
	}

	// Step 4: walk the state machine.
	if _, err := ccStore.UpdateSession(ctx, inventory.CycleCountSession{
		TenantID: tn.ID, ID: session.ID, Code: session.Code,
		Description: session.Description, WarehouseID: session.WarehouseID,
		Status: inventory.CycleCountStatusCounting,
	}); err != nil {
		t.Fatalf("transition to counting: %v", err)
	}
	if _, err := ccStore.UpdateSession(ctx, inventory.CycleCountSession{
		TenantID: tn.ID, ID: session.ID, Code: session.Code,
		Description: session.Description, WarehouseID: session.WarehouseID,
		Status: inventory.CycleCountStatusReconciled,
	}); err != nil {
		t.Fatalf("transition to reconciled: %v", err)
	}
	posted, err := ccStore.PostSession(ctx, tn.ID, session.ID, actorID)
	if err != nil {
		t.Fatalf("post session: %v", err)
	}
	if posted.Status != inventory.CycleCountStatusPosted {
		t.Fatalf("posted status = %q, want posted", posted.Status)
	}
	if posted.PostedAt == nil {
		t.Fatalf("posted_at unset")
	}

	// Step 5: verify a single variance move was written.
	moves := listCycleCountMoves(t, h, tn.ID, updated.ID)
	if len(moves) != 1 {
		t.Fatalf("expected 1 variance move, got %d", len(moves))
	}
	if got, want := moves[0].Qty.String(), "-2"; got != want {
		t.Fatalf("variance move qty = %q, want %q", got, want)
	}
	if moves[0].SourceKType != inventory.MoveSourceCycleCount {
		t.Fatalf("source_ktype = %q, want %q", moves[0].SourceKType, inventory.MoveSourceCycleCount)
	}
	if moves[0].SourceID == nil || *moves[0].SourceID != updated.ID {
		t.Fatalf("source_id = %v, want %v", moves[0].SourceID, updated.ID)
	}

	// Step 6: stock_levels should now read 8 (10 baseline + (-2) variance).
	levels, err := invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list stock levels: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("expected 1 stock level row, got %d", len(levels))
	}
	if got, want := levels[0].Qty.String(), "8"; got != want {
		t.Fatalf("on-hand qty = %q, want %q", got, want)
	}

	// Step 7: idempotent retry — posting again must not write a
	// duplicate move and must return the same session.
	posted2, err := ccStore.PostSession(ctx, tn.ID, session.ID, actorID)
	if err != nil {
		t.Fatalf("re-post session: %v", err)
	}
	if posted2.ID != posted.ID {
		t.Fatalf("re-post returned different session id: %v vs %v", posted2.ID, posted.ID)
	}
	moves2 := listCycleCountMoves(t, h, tn.ID, updated.ID)
	if len(moves2) != 1 {
		t.Fatalf("re-post wrote duplicate moves: %d", len(moves2))
	}

	// Step 8: zero-variance session still posts cleanly with no moves.
	zeroSession, err := ccStore.CreateSession(ctx, inventory.CycleCountSession{
		TenantID:    tn.ID,
		Code:        "CC-2026-002",
		Description: "no variance",
		WarehouseID: wh.ID,
		CreatedBy:   actorID,
	})
	if err != nil {
		t.Fatalf("create zero-variance session: %v", err)
	}
	if err := ccStore.SeedExpectedFromStock(ctx, tn.ID, zeroSession.ID); err != nil {
		t.Fatalf("seed expected on zero session: %v", err)
	}
	zeroLines, _ := ccStore.ListLines(ctx, tn.ID, zeroSession.ID)
	if len(zeroLines) != 1 {
		t.Fatalf("zero session expected 1 line, got %d", len(zeroLines))
	}
	if _, err := ccStore.UpsertLine(ctx, inventory.CycleCountLine{
		TenantID:    tn.ID,
		ID:          zeroLines[0].ID,
		SessionID:   zeroSession.ID,
		ItemID:      item.ID,
		ExpectedQty: zeroLines[0].ExpectedQty,
		CountedQty:  zeroLines[0].ExpectedQty, // counted == expected → zero variance
	}); err != nil {
		t.Fatalf("upsert zero variance line: %v", err)
	}
	if _, err := ccStore.UpdateSession(ctx, inventory.CycleCountSession{
		TenantID: tn.ID, ID: zeroSession.ID, Code: zeroSession.Code,
		Description: zeroSession.Description, WarehouseID: zeroSession.WarehouseID,
		Status: inventory.CycleCountStatusCounting,
	}); err != nil {
		t.Fatalf("zero counting: %v", err)
	}
	if _, err := ccStore.UpdateSession(ctx, inventory.CycleCountSession{
		TenantID: tn.ID, ID: zeroSession.ID, Code: zeroSession.Code,
		Description: zeroSession.Description, WarehouseID: zeroSession.WarehouseID,
		Status: inventory.CycleCountStatusReconciled,
	}); err != nil {
		t.Fatalf("zero reconciled: %v", err)
	}
	if _, err := ccStore.PostSession(ctx, tn.ID, zeroSession.ID, actorID); err != nil {
		t.Fatalf("zero post: %v", err)
	}
	// On-hand qty after the zero-variance post must still be 8.
	levels, err = invStore.ListStockLevels(ctx, tn.ID, &item.ID)
	if err != nil {
		t.Fatalf("list stock levels after zero post: %v", err)
	}
	if got, want := levels[0].Qty.String(), "8"; got != want {
		t.Fatalf("on-hand qty after zero post = %q, want %q", got, want)
	}
}

// newTenantForCycleCount registers the inventory KTypes and returns
// the tenant + an actor uuid suitable for CreatedBy.
func newTenantForCycleCount(t *testing.T, h *harness) (*tenantRow, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := inventory.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register inventory ktypes: %v", err)
	}
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("cycle-count"),
		Name: "Cycle Count Co",
		Cell: "test",
		Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return &tenantRow{ID: tn.ID, Slug: tn.Slug}, uuid.New()
}

// seedCycleCountFixture creates one item + one warehouse on the
// supplied tenant. Returns the krecords (so the caller can use
// item.ID / wh.ID downstream).
func seedCycleCountFixture(
	t *testing.T,
	h *harness,
	tenantID, actorID uuid.UUID,
	sku, whCode string,
) (*record.KRecord, *record.KRecord) {
	t.Helper()
	ctx := context.Background()
	itemBody, _ := json.Marshal(map[string]any{
		"sku": sku, "name": sku, "uom": "ea", "active": true,
	})
	item, err := h.records.Create(ctx, record.KRecord{
		TenantID: tenantID, KType: inventory.KTypeItem,
		Status: "active", Data: itemBody, CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	whBody, _ := json.Marshal(map[string]any{
		"code": whCode, "name": whCode, "active": true,
	})
	wh, err := h.records.Create(ctx, record.KRecord{
		TenantID: tenantID, KType: inventory.KTypeWarehouse,
		Status: "active", Data: whBody, CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("seed warehouse: %v", err)
	}
	return item, wh
}

// listCycleCountMoves returns every inventory_move keyed on
// (MoveSourceCycleCount, line.id). Used by the assertions to count
// variance moves produced by a single line.
func listCycleCountMoves(t *testing.T, h *harness, tenantID, lineID uuid.UUID) []inventory.Move {
	t.Helper()
	invStore := inventory.NewPGStore(h.pool, h.publisher, h.auditor)
	all, err := invStore.ListMoves(context.Background(), tenantID, inventory.MoveFilter{
		SourceKType: inventory.MoveSourceCycleCount,
	})
	if err != nil {
		t.Fatalf("list moves: %v", err)
	}
	var out []inventory.Move
	for i := range all {
		if all[i].SourceID != nil && *all[i].SourceID == lineID {
			out = append(out, all[i])
		}
	}
	return out
}
