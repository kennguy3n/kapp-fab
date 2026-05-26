// Package manufacturing implements Phase N6 — Light Manufacturing:
// Bills of Material + Work Orders. The deliberately narrow scope
// closes the biggest module gap vs ERPNext per the audit
// (Recommendation #3) without committing to routings, capacity
// planning, shop-floor control, or quality inspection.
//
// The model:
//
//   - A BOM (Bill of Materials) is a versioned recipe for producing
//     a finished-good Item. Only one BOM per item can be marked
//     `active` at a time; that row drives the consumption math for
//     work orders against the item.
//   - A WorkOrder is a single production run against an active BOM.
//     The order walks draft → released → in_progress → completed →
//     closed (with a cancelled terminal state available from any
//     pre-completed status). On completion the engine emits a
//     receipt move for the finished good and one consumption move
//     per BOM component, all in the same DB transaction so a
//     partial completion is impossible.
//
// Posting hooks intentionally piggy-back on the existing
// inventory.RecordMove path so the moves participate in the same
// outbox + audit pipeline as every other stock motion. The
// idempotency guarantee comes for free from the existing
// inventory_moves_source_uniq partial unique index — a retry of
// CompleteWorkOrder after a partial failure replays as a no-op for
// the moves that already landed.
package manufacturing

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// KType identifiers. Kept as constants so the API, agent tools, and
// tests all reference the same strings.
const (
	KTypeBOM       = "manufacturing.bom"
	KTypeWorkOrder = "manufacturing.work_order"
)

// Stock-move source labels emitted by the work-order completion
// engine. Kept here so the manufacturing package, the inventory
// engine, and the integration tests all agree on the label.
const (
	// MoveSourceWorkOrderConsume tags the consumption moves
	// (negative qty against component items) emitted on work-order
	// completion.
	MoveSourceWorkOrderConsume = "manufacturing.work_order.consume"

	// MoveSourceWorkOrderReceipt tags the finished-goods receipt
	// move (positive qty against the work order's output item)
	// emitted on work-order completion.
	MoveSourceWorkOrderReceipt = "manufacturing.work_order.receipt"
)

// BOMStatus enumerates the legal values for boms.status.
const (
	BOMStatusDraft    = "draft"
	BOMStatusActive   = "active"
	BOMStatusObsolete = "obsolete"
)

// WorkOrderStatus enumerates the legal values for work_orders.status.
// The state machine in store.go enforces the legal transitions.
const (
	WorkOrderStatusDraft      = "draft"
	WorkOrderStatusReleased   = "released"
	WorkOrderStatusInProgress = "in_progress"
	WorkOrderStatusCompleted  = "completed"
	WorkOrderStatusClosed     = "closed"
	WorkOrderStatusCancelled  = "cancelled"
)

// Sentinel errors. Callers compare with errors.Is so the wrapping
// chain in the store can attach context without breaking the
// equality.
var (
	// ErrBOMNotFound is returned by GetBOM and ListBOMs when the
	// requested BOM does not exist for the caller's tenant.
	ErrBOMNotFound = errors.New("manufacturing: bom not found")

	// ErrBOMNotActive is returned by ReleaseWorkOrder when the
	// referenced item has no BOM in `active` status.
	ErrBOMNotActive = errors.New("manufacturing: item has no active bom")

	// ErrBOMHasComponents is returned by SetBOMStatus when the
	// caller tries to activate a BOM with zero components — a
	// safety net against accidentally posting a work order that
	// consumes nothing.
	ErrBOMHasNoComponents = errors.New("manufacturing: bom has no components")

	// ErrBOMItemMismatch is returned when a BOM component's
	// component_item_id duplicates the parent BOM's item_id (a
	// recipe that consumes itself is always a typo).
	ErrBOMSelfReference = errors.New("manufacturing: bom cannot reference its own output item as a component")

	// ErrBOMDuplicateComponent is returned by CreateBOM when the
	// input slice lists the same component_item_id twice. The
	// `bom_components` table has PK (tenant_id, bom_id,
	// component_item_id) so the database would reject the second
	// insert with a 23505 — surfacing that as a clear typed error
	// at validation time avoids returning a cryptic 500 to the
	// HTTP caller.
	ErrBOMDuplicateComponent = errors.New("manufacturing: bom lists the same component_item_id more than once")

	// ErrWorkOrderNotFound mirrors ErrBOMNotFound for work_orders.
	ErrWorkOrderNotFound = errors.New("manufacturing: work order not found")

	// ErrWorkOrderInvalidTransition is returned when callers
	// attempt an illegal status transition (e.g. completed →
	// in_progress). The state machine is enforced in Go rather
	// than in SQL so the error surface is a typed sentinel and
	// the message can name both the current and the requested
	// status.
	ErrWorkOrderInvalidTransition = errors.New("manufacturing: invalid work order status transition")

	// ErrWorkOrderInsufficientStock is returned by
	// CompleteWorkOrder when one or more BOM components don't
	// have enough on-hand stock at the work order's warehouse to
	// cover the consumption math. The error message lists the
	// offending items so the UI / KChat surface can render a
	// useful diagnostic. Strict enforcement is the safer default
	// for SME use; tenants that want negative-stock tolerance can
	// pre-receipt components into a "WIP" warehouse first.
	ErrWorkOrderInsufficientStock = errors.New("manufacturing: insufficient component stock to complete work order")

	// ErrInvalidInput is the umbrella sentinel for client-input
	// validation failures (empty / zero / out-of-range fields on
	// CreateBOM / CreateWorkOrder / SetBOMStatus / CompleteWorkOrder).
	// Each call site wraps it with %w plus a human-readable message
	// naming the offending field, so the HTTP layer can map every
	// such error to 422 Unprocessable Entity in one switch arm via
	// errors.Is(err, ErrInvalidInput) rather than needing a dedicated
	// sentinel per field. Programmer-error paths (e.g. tenant id
	// required, which is set by middleware and never by the client)
	// deliberately do NOT wrap this sentinel so they still surface as
	// a 500 — they indicate a server-side bug, not bad user input.
	ErrInvalidInput = errors.New("manufacturing: invalid input")
)

// BOM is a Bill of Materials master record. One row per
// (tenant_id, item_id, version). Status drives the draft / active /
// obsolete lifecycle; only one row per item_id may have
// status='active' at any time (enforced by the boms_active_per_item_uniq
// partial unique index).
type BOM struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	ID        uuid.UUID `json:"id"`
	ItemID    uuid.UUID `json:"item_id"`
	Version   string    `json:"version"`
	Status    string    `json:"status"`
	OutputQty decimal.Decimal `json:"output_qty"`
	UOM       string    `json:"uom"`
	Notes     string    `json:"notes,omitempty"`
	CreatedBy uuid.UUID `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Components is loaded by GetBOM and the work-order
	// completion engine. Empty for partial fetches (e.g.
	// ListBOMs without component expansion) so the slice's
	// length is not a reliable existence check on its own.
	Components []BOMComponent `json:"components,omitempty"`
}

// BOMComponent is one consumed item in a BOM. Quantities are
// per-output-batch — the engine multiplies by
// (work_order.planned_qty / bom.output_qty) when computing actual
// consumption.
type BOMComponent struct {
	BOMID           uuid.UUID       `json:"bom_id"`
	ComponentItemID uuid.UUID       `json:"component_item_id"`
	Qty             decimal.Decimal `json:"qty"`
	UOM             string          `json:"uom"`
	// ScrapPercent reserves additional material for spoilage on
	// work-order completion. NULL ≡ 0.
	ScrapPercent *decimal.Decimal `json:"scrap_percent,omitempty"`
	SortOrder    int              `json:"sort_order"`
}

// EffectiveQty is the per-output-batch quantity scaled for scrap.
// Returned as a pure value so the store and the agent tools share
// the same math.
func (c BOMComponent) EffectiveQty() decimal.Decimal {
	if c.ScrapPercent == nil || c.ScrapPercent.IsZero() {
		return c.Qty
	}
	// qty * (1 + scrap/100)
	factor := decimal.NewFromInt(1).Add(c.ScrapPercent.Div(decimal.NewFromInt(100)))
	return c.Qty.Mul(factor)
}

// WorkOrder is a single production run against a BOM. The state
// machine in store.go enforces the legal transitions.
type WorkOrder struct {
	TenantID        uuid.UUID        `json:"tenant_id"`
	ID              uuid.UUID        `json:"id"`
	ItemID          uuid.UUID        `json:"item_id"`
	BOMID           *uuid.UUID       `json:"bom_id,omitempty"`
	WarehouseID     uuid.UUID        `json:"warehouse_id"`
	PlannedQty      decimal.Decimal  `json:"planned_qty"`
	ActualQty       *decimal.Decimal `json:"actual_qty,omitempty"`
	Status          string           `json:"status"`
	ScheduledStart  *time.Time       `json:"scheduled_start,omitempty"`
	ScheduledEnd    *time.Time       `json:"scheduled_end,omitempty"`
	StartedAt       *time.Time       `json:"started_at,omitempty"`
	CompletedAt     *time.Time       `json:"completed_at,omitempty"`
	Notes           string           `json:"notes,omitempty"`
	CreatedBy       uuid.UUID        `json:"created_by,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// CanTransitionTo reports whether the receiver may move to the
// supplied target status. The matrix is duplicated in store.go's
// SetWorkOrderStatus method for the SQL-side guard; this method is
// the source of truth for callers that want to render legal
// transitions in a UI (e.g. greying out illegal status buttons).
func (w WorkOrder) CanTransitionTo(target string) bool {
	if w.Status == target {
		// Idempotent re-assertion of the current status is
		// allowed so the API doesn't have to special-case
		// retries.
		return true
	}
	switch w.Status {
	case WorkOrderStatusDraft:
		return target == WorkOrderStatusReleased || target == WorkOrderStatusCancelled
	case WorkOrderStatusReleased:
		// Direct release→completed is allowed so a small shop
		// that doesn't bother with an explicit "in_progress"
		// phase can complete a work order in one HTTP call. The
		// state machine still rejects released→closed (must go
		// through completed first) and any backwards transition.
		return target == WorkOrderStatusInProgress ||
			target == WorkOrderStatusCompleted ||
			target == WorkOrderStatusCancelled
	case WorkOrderStatusInProgress:
		return target == WorkOrderStatusCompleted || target == WorkOrderStatusCancelled
	case WorkOrderStatusCompleted:
		return target == WorkOrderStatusClosed
	case WorkOrderStatusClosed, WorkOrderStatusCancelled:
		// Terminal — no further transitions.
		return false
	default:
		return false
	}
}
