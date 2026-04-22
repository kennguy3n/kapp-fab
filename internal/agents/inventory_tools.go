package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
)

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
}

// ----- inventory.record_move -----

type recordMoveInput struct {
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	UnitCost    decimal.Decimal `json:"unit_cost,omitempty"`
	SourceKType string          `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID      `json:"source_id,omitempty"`
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
