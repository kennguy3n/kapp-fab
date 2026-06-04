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
	// Stream 2 — Manufacturing Depth.
	x.Register(&createWorkCenterTool{store: store})
	x.Register(&createRoutingTool{store: store})
	x.Register(&activateRoutingTool{store: store})
	x.Register(&startJobCardTool{store: store})
	x.Register(&completeJobCardTool{store: store})
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

// ----- manufacturing.create_work_center -----

type createWorkCenterToolInput struct {
	Name                 string          `json:"name"`
	CapacityPerHour      decimal.Decimal `json:"capacity_per_hour,omitempty"`
	OperatingHoursPerDay decimal.Decimal `json:"operating_hours_per_day,omitempty"`
	EfficiencyPercent    decimal.Decimal `json:"efficiency_percent,omitempty"`
	Notes                string          `json:"notes,omitempty"`
}

type createWorkCenterTool struct {
	store *manufacturing.PGStore
}

func (t *createWorkCenterTool) Name() string { return "manufacturing.create_work_center" }

// RequiresConfirmation is false — a work center is reference data and
// changes no inventory or financial state.
func (t *createWorkCenterTool) RequiresConfirmation() bool { return false }

func (t *createWorkCenterTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createWorkCenterToolInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, errors.New("manufacturing.create_work_center: name required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would create work center %q", in.Name),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.create_work_center: manufacturing store not configured")
	}
	wc, err := t.store.CreateWorkCenter(ctx, inv.TenantID, inv.ActorID, manufacturing.CreateWorkCenterInput{
		Name:                 in.Name,
		CapacityPerHour:      in.CapacityPerHour,
		OperatingHoursPerDay: in.OperatingHoursPerDay,
		EfficiencyPercent:    in.EfficiencyPercent,
		Notes:                in.Notes,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(wc)
	return &Result{
		Summary: fmt.Sprintf("Created work center %s (%s)", wc.Name, wc.ID),
		Preview: body,
		Extra:   map[string]any{"work_center_id": wc.ID.String(), "status": wc.Status},
	}, nil
}

// ----- manufacturing.create_routing -----

type createRoutingToolOperation struct {
	OperationName    string          `json:"operation_name"`
	WorkCenterID     uuid.UUID       `json:"work_center_id"`
	SetupTimeMinutes decimal.Decimal `json:"setup_time_minutes,omitempty"`
	CycleTimeMinutes decimal.Decimal `json:"cycle_time_minutes,omitempty"`
	Description      string          `json:"description,omitempty"`
}

type createRoutingToolInput struct {
	ItemID     uuid.UUID                    `json:"item_id"`
	Version    string                       `json:"version"`
	Notes      string                       `json:"notes,omitempty"`
	Operations []createRoutingToolOperation `json:"operations"`
	Activate   bool                         `json:"activate,omitempty"`
}

type createRoutingTool struct {
	store *manufacturing.PGStore
}

func (t *createRoutingTool) Name() string { return "manufacturing.create_routing" }

// RequiresConfirmation is false — authoring a routing (even an active
// one) changes no inventory; it only defines how future work orders
// generate job cards.
func (t *createRoutingTool) RequiresConfirmation() bool { return false }

func (t *createRoutingTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createRoutingToolInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ItemID == uuid.Nil {
		return nil, errors.New("manufacturing.create_routing: item_id required")
	}
	if in.Version == "" {
		return nil, errors.New("manufacturing.create_routing: version required")
	}
	if len(in.Operations) == 0 {
		return nil, errors.New("manufacturing.create_routing: at least one operation required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would create routing %s for item %s (%d operations)", in.Version, in.ItemID, len(in.Operations)),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.create_routing: manufacturing store not configured")
	}
	storeIn := manufacturing.CreateRoutingInput{
		ItemID:   in.ItemID,
		Version:  in.Version,
		Notes:    in.Notes,
		Activate: in.Activate,
	}
	for _, op := range in.Operations {
		storeIn.Operations = append(storeIn.Operations, manufacturing.RoutingOperationInput{
			OperationName:    op.OperationName,
			WorkCenterID:     op.WorkCenterID,
			SetupTimeMinutes: op.SetupTimeMinutes,
			CycleTimeMinutes: op.CycleTimeMinutes,
			Description:      op.Description,
		})
	}
	routing, err := t.store.CreateRouting(ctx, inv.TenantID, inv.ActorID, storeIn)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(routing)
	return &Result{
		Summary: fmt.Sprintf("Created routing %s for item %s (%s)", routing.Version, routing.ItemID, routing.Status),
		Preview: body,
		Extra:   map[string]any{"routing_id": routing.ID.String(), "status": routing.Status},
	}, nil
}

// ----- manufacturing.activate_routing -----

type activateRoutingInput struct {
	RoutingID uuid.UUID `json:"routing_id"`
}

type activateRoutingTool struct {
	store *manufacturing.PGStore
}

func (t *activateRoutingTool) Name() string { return "manufacturing.activate_routing" }

// RequiresConfirmation is false — activation demotes any previously
// active routing for the item but moves no stock. The next work-order
// release picks up the newly active routing for its job cards.
func (t *activateRoutingTool) RequiresConfirmation() bool { return false }

func (t *activateRoutingTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in activateRoutingInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.RoutingID == uuid.Nil {
		return nil, errors.New("manufacturing.activate_routing: routing_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would activate routing %s", in.RoutingID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.activate_routing: manufacturing store not configured")
	}
	if err := t.store.SetRoutingStatus(ctx, inv.TenantID, in.RoutingID, manufacturing.RoutingStatusActive); err != nil {
		return nil, err
	}
	routing, err := t.store.GetRouting(ctx, inv.TenantID, in.RoutingID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(routing)
	return &Result{
		Summary: fmt.Sprintf("Activated routing %s for item %s", routing.Version, routing.ItemID),
		Preview: body,
		Extra:   map[string]any{"routing_id": routing.ID.String(), "status": routing.Status},
	}, nil
}

// ----- manufacturing.start_job_card -----

type startJobCardInput struct {
	JobCardID uuid.UUID `json:"job_card_id"`
}

type startJobCardTool struct {
	store *manufacturing.PGStore
}

func (t *startJobCardTool) Name() string { return "manufacturing.start_job_card" }

// RequiresConfirmation is false — starting a card only stamps
// actual_start and the operator; no inventory moves.
func (t *startJobCardTool) RequiresConfirmation() bool { return false }

func (t *startJobCardTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in startJobCardInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.JobCardID == uuid.Nil {
		return nil, errors.New("manufacturing.start_job_card: job_card_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would start job card %s", in.JobCardID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.start_job_card: manufacturing store not configured")
	}
	jc, err := t.store.StartJobCard(ctx, inv.TenantID, in.JobCardID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(jc)
	return &Result{
		Summary: fmt.Sprintf("Started job card %s (op %d)", jc.ID, jc.RoutingOperationSeq),
		Preview: body,
		Extra:   map[string]any{"job_card_id": jc.ID.String(), "status": jc.Status},
	}, nil
}

// ----- manufacturing.complete_job_card -----
//
// Completing the LAST open card on a work order triggers the work
// order's completion flow, which emits the consumption + finished-goods
// inventory moves — so this tool requires confirmation.

type completeJobCardToolInput struct {
	JobCardID   uuid.UUID       `json:"job_card_id"`
	QtyProduced decimal.Decimal `json:"qty_produced,omitempty"`
	QtyRejected decimal.Decimal `json:"qty_rejected,omitempty"`
	Notes       string          `json:"notes,omitempty"`
}

type completeJobCardTool struct {
	store *manufacturing.PGStore
}

func (t *completeJobCardTool) Name() string { return "manufacturing.complete_job_card" }

// RequiresConfirmation returns true: completing the final job card on a
// work order auto-completes the work order, which debits component
// stock and credits finished goods — the same ledger-moving step that
// makes complete_work_order require confirmation.
func (t *completeJobCardTool) RequiresConfirmation() bool { return true }

func (t *completeJobCardTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in completeJobCardToolInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.JobCardID == uuid.Nil {
		return nil, errors.New("manufacturing.complete_job_card: job_card_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would complete job card %s", in.JobCardID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("manufacturing.complete_job_card: manufacturing store not configured")
	}
	jc, err := t.store.CompleteJobCard(ctx, inv.TenantID, in.JobCardID, manufacturing.CompleteJobCardInput{
		OperatorID:  inv.ActorID,
		QtyProduced: in.QtyProduced,
		QtyRejected: in.QtyRejected,
		Notes:       in.Notes,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(jc)
	return &Result{
		Summary: fmt.Sprintf("Completed job card %s (op %d)", jc.ID, jc.RoutingOperationSeq),
		Preview: body,
		Extra:   map[string]any{"job_card_id": jc.ID.String(), "status": jc.Status},
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
