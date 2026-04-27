package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// parseDateOrTime accepts ISO 8601 dates or full RFC 3339 timestamps
// from a tool input and normalises them to UTC midnight (date-only)
// or the supplied instant. Used by inventory.assign_batch for
// manufactured_at / expires_at.
func parseDateOrTime(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// RegisterInventoryTools attaches the Phase D inventory tools to an
// executor. Mirrors RegisterFinanceTools; wiring runs at service startup
// once the inventory store is built.
//
// A nil store is tolerated so Phase B tests that never spin up the
// inventory schema still pass — commit-mode calls return a clear error
// in that case rather than panicking.
func RegisterInventoryTools(x *Executor, store *inventory.PGStore) {
	x.Register(&recordMoveTool{store: store})
	x.Register(&checkStockTool{store: store})
	x.Register(&reverseMoveTool{store: store})
	x.Register(&assignBatchTool{store: store})
}

// RegisterInventoryReorderTool wires the trigger_reorder tool against
// the live scheduled ReorderHandler. Kept separate so callers that
// don't care about the reorder path don't need to construct a
// handler just to register the other two tools.
func RegisterInventoryReorderTool(x *Executor, handler *inventory.ReorderHandler) {
	x.Register(&triggerReorderTool{handler: handler})
}

// ----- inventory.trigger_reorder -----

type triggerReorderTool struct {
	handler *inventory.ReorderHandler
}

func (t *triggerReorderTool) Name() string               { return "inventory.trigger_reorder" }
func (t *triggerReorderTool) RequiresConfirmation() bool { return true }
func (t *triggerReorderTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	if inv.Mode == ModeDryRun {
		return &Result{
			Summary: "Would run the inventory reorder sweep for this tenant",
			Preview: json.RawMessage(`{"action":"inventory.trigger_reorder","mode":"dry_run"}`),
		}, nil
	}
	if t.handler == nil {
		return nil, errors.New("inventory.trigger_reorder: reorder handler not configured")
	}
	if err := t.handler.Handle(ctx, inv.TenantID, scheduler.ScheduledAction{TenantID: inv.TenantID, ActionType: inventory.ActionTypeReorder}); err != nil {
		return nil, fmt.Errorf("inventory.trigger_reorder: %w", err)
	}
	return &Result{
		Summary: "Inventory reorder sweep completed",
		Preview: json.RawMessage(`{"action":"inventory.trigger_reorder","mode":"commit"}`),
	}, nil
}

// ----- inventory.record_move -----

type recordMoveInput struct {
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	UnitCost    decimal.Decimal `json:"unit_cost,omitempty"`
	SourceKType string          `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID      `json:"source_id,omitempty"`
	BatchID     *uuid.UUID      `json:"batch_id,omitempty"`
	Memo        string          `json:"memo,omitempty"`
}

type recordMoveTool struct {
	store *inventory.PGStore
}

func (t *recordMoveTool) Name() string               { return "inventory.record_move" }
func (t *recordMoveTool) RequiresConfirmation() bool { return true }
func (t *recordMoveTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in recordMoveInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ItemID == uuid.Nil || in.WarehouseID == uuid.Nil {
		return nil, errors.New("inventory.record_move: item_id and warehouse_id required")
	}
	if in.Qty.IsZero() {
		return nil, errors.New("inventory.record_move: qty must be non-zero")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would record move %s of item %s @ %s", in.Qty, in.ItemID, in.WarehouseID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("inventory.record_move: inventory store not configured")
	}
	srcKType := in.SourceKType
	if srcKType == "" {
		srcKType = inventory.MoveSourceAdjustment
	}
	move, err := t.store.RecordMove(ctx, inventory.Move{
		TenantID:    inv.TenantID,
		ItemID:      in.ItemID,
		WarehouseID: in.WarehouseID,
		Qty:         in.Qty,
		UnitCost:    in.UnitCost,
		SourceKType: srcKType,
		SourceID:    in.SourceID,
		BatchID:     in.BatchID,
		CreatedBy:   inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(move)
	return &Result{
		Summary: fmt.Sprintf("Recorded move %d (%s @ %s qty=%s)", move.ID, move.ItemID, move.WarehouseID, move.Qty),
		Preview: body,
		Extra:   map[string]any{"move_id": move.ID},
	}, nil
}

// ----- inventory.reverse_move -----
//
// Cancels a previously-recorded move by posting a contra-entry.
// Confirmation is required because reversal is a destructive
// stock-adjusting action; reversing a contra row directly is
// rejected at the store layer with ErrCannotReverseContra.

type reverseMoveInput struct {
	MoveID int64  `json:"move_id"`
	Memo   string `json:"memo,omitempty"`
}

type reverseMoveTool struct {
	store *inventory.PGStore
}

func (t *reverseMoveTool) Name() string               { return "inventory.reverse_move" }
func (t *reverseMoveTool) RequiresConfirmation() bool { return true }
func (t *reverseMoveTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in reverseMoveInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.MoveID == 0 {
		return nil, errors.New("inventory.reverse_move: move_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would reverse stock move %d", in.MoveID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("inventory.reverse_move: inventory store not configured")
	}
	move, err := t.store.ReverseMove(ctx, inv.TenantID, in.MoveID, inv.ActorID, in.Memo)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(move)
	return &Result{
		Summary: fmt.Sprintf("Reversed move %d (new contra-entry id=%d, qty=%s)", in.MoveID, move.ID, move.Qty),
		Preview: body,
		Extra:   map[string]any{"contra_move_id": move.ID, "reversed_move_id": in.MoveID},
	}, nil
}

// ----- inventory.check_stock -----

type checkStockInput struct {
	ItemID *uuid.UUID `json:"item_id,omitempty"`
}

type checkStockTool struct {
	store *inventory.PGStore
}

func (t *checkStockTool) Name() string               { return "inventory.check_stock" }
func (t *checkStockTool) RequiresConfirmation() bool { return false }
func (t *checkStockTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in checkStockInput
	if len(inv.Inputs) > 0 {
		if err := json.Unmarshal(inv.Inputs, &in); err != nil {
			return nil, fmt.Errorf("inventory.check_stock: decode inputs: %w", err)
		}
	}
	if t.store == nil {
		return nil, errors.New("inventory.check_stock: inventory store not configured")
	}
	levels, err := t.store.ListStockLevels(ctx, inv.TenantID, in.ItemID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(levels)
	summary := fmt.Sprintf("%d stock-level rows", len(levels))
	if in.ItemID != nil {
		summary = fmt.Sprintf("%d warehouses for item %s", len(levels), *in.ItemID)
	}
	return &Result{
		Summary: summary,
		Preview: body,
	}, nil
}

// ----- inventory.assign_batch -----
//
// Creates an inventory_batches row for the supplied item. Idempotent
// in-tenant on (item_id, batch_no) — re-issuing the same batch_no
// returns ErrDuplicate from the underlying unique constraint.
type assignBatchInput struct {
	ItemID         uuid.UUID `json:"item_id"`
	BatchNo        string    `json:"batch_no"`
	ManufacturedAt *string   `json:"manufactured_at,omitempty"`
	ExpiresAt      *string   `json:"expires_at,omitempty"`
}

type assignBatchTool struct {
	store *inventory.PGStore
}

func (t *assignBatchTool) Name() string               { return "inventory.assign_batch" }
func (t *assignBatchTool) RequiresConfirmation() bool { return true }
func (t *assignBatchTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in assignBatchInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ItemID == uuid.Nil || in.BatchNo == "" {
		return nil, errors.New("inventory.assign_batch: item_id and batch_no required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would create batch %q for item %s", in.BatchNo, in.ItemID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("inventory.assign_batch: inventory store not configured")
	}
	b := inventory.Batch{
		TenantID:  inv.TenantID,
		ItemID:    in.ItemID,
		BatchNo:   in.BatchNo,
		CreatedBy: inv.ActorID,
	}
	if in.ManufacturedAt != nil && *in.ManufacturedAt != "" {
		ts, err := parseDateOrTime(*in.ManufacturedAt)
		if err != nil {
			return nil, fmt.Errorf("inventory.assign_batch: invalid manufactured_at %q: %w", *in.ManufacturedAt, err)
		}
		b.ManufacturedAt = &ts
	}
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		ts, err := parseDateOrTime(*in.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("inventory.assign_batch: invalid expires_at %q: %w", *in.ExpiresAt, err)
		}
		b.ExpiresAt = &ts
	}
	out, err := t.store.CreateBatch(ctx, b)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(out)
	return &Result{
		Summary: fmt.Sprintf("Created batch %s (%q) for item %s", out.ID, out.BatchNo, out.ItemID),
		Preview: body,
		Extra:   map[string]any{"batch_id": out.ID},
	}, nil
}
