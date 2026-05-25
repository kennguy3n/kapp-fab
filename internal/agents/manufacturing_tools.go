package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/manufacturing"
)

// RegisterManufacturingTools attaches the Phase N6 manufacturing tools
// to an executor. Mirrors RegisterInventoryTools; wiring runs at
// service startup once the manufacturing store is built.
//
// A nil store is tolerated so tests that don't exercise the
// manufacturing schema still pass — commit-mode calls return a clear
// error in that case rather than panicking.
func RegisterManufacturingTools(x *Executor, store *manufacturing.PGStore) {
	x.Register(&createWorkOrderTool{store: store})
	x.Register(&completeWorkOrderTool{store: store})
	x.Register(&releaseWorkOrderTool{store: store})
}

// ----- manufacturing.create_work_order -----

type createWorkOrderInput struct {
	ItemID         uuid.UUID       `json:"item_id"`
	WarehouseID    uuid.UUID       `json:"warehouse_id"`
	PlannedQty     decimal.Decimal `json:"planned_qty"`
	ScheduledStart *time.Time      `json:"scheduled_start,omitempty"`
	ScheduledEnd   *time.Time      `json:"scheduled_end,omitempty"`
	Notes          string          `json:"notes,omitempty"`
}

type createWorkOrderTool struct {
	store *manufacturing.PGStore
}

// Name is the agent-tool identifier used by the registry and the
// confirmation card.
func (t *createWorkOrderTool) Name() string { return "manufacturing.create_work_order" }

// RequiresConfirmation reports whether the executor should pause
// for an explicit human confirmation card before invoking the tool.
// Creating a draft work order does not change inventory, so no
// confirmation is needed at this step.
func (t *createWorkOrderTool) RequiresConfirmation() bool { return false }

// Invoke creates a draft work order in commit mode and returns a
// preview JSON in dry-run mode.
func (t *createWorkOrderTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createWorkOrderInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ItemID == uuid.Nil || in.WarehouseID == uuid.Nil {
		return nil, errors.New("manufacturing.create_work_order: item_id and warehouse_id required")
	}
	if in.PlannedQty.IsZero() || in.PlannedQty.IsNegative() {
		return nil, errors.New("manufacturing.create_work_order: planned_qty must be > 0")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would create work order for %s x%s @ %s", in.ItemID, in.PlannedQty.String(), in.WarehouseID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.create_work_order: manufacturing store not configured")
	}
	wo, err := t.store.CreateWorkOrder(ctx, inv.TenantID, inv.ActorID, manufacturing.CreateWorkOrderInput{
		ItemID:         in.ItemID,
		WarehouseID:    in.WarehouseID,
		PlannedQty:     in.PlannedQty,
		ScheduledStart: in.ScheduledStart,
		ScheduledEnd:   in.ScheduledEnd,
		Notes:          in.Notes,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(wo)
	return &Result{
		Summary: fmt.Sprintf("Created work order %s for item %s (planned %s)", wo.ID, wo.ItemID, wo.PlannedQty.String()),
		Preview: body,
		Extra:   map[string]any{"work_order_id": wo.ID.String(), "status": wo.Status},
	}, nil
}

// ----- manufacturing.release_work_order -----
//
// Snapshots the currently active BOM onto the work order row and
// flips status to 'released'. Separate from create so an SME can
// build a queue of draft work orders before committing material.

type releaseWorkOrderInput struct {
	WorkOrderID uuid.UUID `json:"work_order_id"`
}

type releaseWorkOrderTool struct {
	store *manufacturing.PGStore
}

// Name is the agent-tool identifier used by the registry and the
// confirmation card.
func (t *releaseWorkOrderTool) Name() string { return "manufacturing.release_work_order" }

// RequiresConfirmation reports whether the executor should pause
// for explicit human confirmation. Release snapshots the active
// BOM but does not yet move stock, so confirmation is not required.
func (t *releaseWorkOrderTool) RequiresConfirmation() bool { return false }

// Invoke transitions the work order to 'released' and snapshots the
// active BOM onto the row.
func (t *releaseWorkOrderTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in releaseWorkOrderInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.WorkOrderID == uuid.Nil {
		return nil, errors.New("manufacturing.release_work_order: work_order_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would release work order %s", in.WorkOrderID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.release_work_order: manufacturing store not configured")
	}
	wo, err := t.store.ReleaseWorkOrder(ctx, inv.TenantID, in.WorkOrderID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(wo)
	return &Result{
		Summary: fmt.Sprintf("Released work order %s (snapshot BOM %s)", wo.ID, derefUUID(wo.BOMID)),
		Preview: body,
		Extra:   map[string]any{"work_order_id": wo.ID.String(), "status": wo.Status},
	}, nil
}

// ----- manufacturing.complete_work_order -----
//
// Stamps actual_qty + completed_at, flips status to 'completed',
// and emits the matching inventory moves (one consumption move per
// BOM component, one finished-goods receipt). RequiresConfirmation
// because the moves debit and credit the inventory ledger.

type completeWorkOrderInput struct {
	WorkOrderID uuid.UUID       `json:"work_order_id"`
	ActualQty   decimal.Decimal `json:"actual_qty,omitempty"`
}

type completeWorkOrderTool struct {
	store *manufacturing.PGStore
}

// Name is the agent-tool identifier used by the registry and the
// confirmation card.
func (t *completeWorkOrderTool) Name() string { return "manufacturing.complete_work_order" }

// RequiresConfirmation returns true because completion emits the
// consumption + receipt inventory moves and cannot be reversed
// without a manual adjustment journal — the operator must approve
// it via the confirmation card.
func (t *completeWorkOrderTool) RequiresConfirmation() bool { return true }

// Invoke completes the work order: stamps actual_qty + completed_at,
// flips status to 'completed', and emits one consumption move per
// BOM component plus one finished-goods receipt move, atomically.
func (t *completeWorkOrderTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in completeWorkOrderInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.WorkOrderID == uuid.Nil {
		return nil, errors.New("manufacturing.complete_work_order: work_order_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		qtyStr := "<planned>"
		if !in.ActualQty.IsZero() {
			qtyStr = in.ActualQty.String()
		}
		return &Result{
			Summary: fmt.Sprintf("Would complete work order %s (actual %s)", in.WorkOrderID, qtyStr),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.complete_work_order: manufacturing store not configured")
	}
	wo, err := t.store.CompleteWorkOrder(ctx, inv.TenantID, in.WorkOrderID, inv.ActorID, manufacturing.CompleteWorkOrderInput{
		ActualQty: in.ActualQty,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(wo)
	actual := ""
	if wo.ActualQty != nil {
		actual = wo.ActualQty.String()
	}
	return &Result{
		Summary: fmt.Sprintf("Completed work order %s (actual %s)", wo.ID, actual),
		Preview: body,
		Extra:   map[string]any{"work_order_id": wo.ID.String(), "status": wo.Status, "actual_qty": actual},
	}, nil
}

// derefUUID renders a *uuid.UUID as its string or the empty string.
// Kept package-private so other tool files can re-use the helper
// without exporting it from the agents API surface.
func derefUUID(u *uuid.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}
