package manufacturing

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Subcontracting.
//
// A subcontract order models work performed by an external supplier:
// components are issued out of our stock to the supplier, the supplier
// performs an operation (assembly, plating, machining, …), and the
// finished or sub-assembled item is received back. It optionally ties to
// an existing work order's routing operation so subcontracting plugs
// into the routing model rather than forking a parallel one.
//
// The lifecycle is draft → issued → received → closed, with a cancelled
// terminal state reachable only from draft (once components have been
// issued the order carries stock motion that must be received or
// reversed deliberately, so a blanket cancel is disallowed). Issuing
// emits one negative inventory move per component; receiving emits a
// positive receipt move for the finished item, valued at the supplier's
// service charge per unit. Both go through the existing
// inventory.RecordMove path so they inherit the outbox + audit pipeline
// and idempotency from inventory_moves_source_uniq (see migration
// 000093).

// Subcontract stock-move source labels. Kept here so the manufacturing
// package, the inventory engine, and the integration tests agree on the
// label. Both moves carry source_id = the subcontract order id; the
// distinct ktypes keep the issue and receipt moves separately
// idempotent.
const (
	// MoveSourceSubcontractIssue tags the negative-qty moves issuing
	// components out of our stock to the supplier.
	MoveSourceSubcontractIssue = "manufacturing.subcontract.issue"

	// MoveSourceSubcontractReceipt tags the positive-qty receipt of the
	// finished / sub-assembled item back from the supplier.
	MoveSourceSubcontractReceipt = "manufacturing.subcontract.receipt"
)

// SubcontractStatus enumerates the legal values for
// subcontract_orders.status. The state machine in CanTransitionTo (and
// the explicit guards in the issue/receive/close/cancel store methods)
// enforces the legal transitions.
const (
	SubcontractStatusDraft     = "draft"
	SubcontractStatusIssued    = "issued"
	SubcontractStatusReceived  = "received"
	SubcontractStatusClosed    = "closed"
	SubcontractStatusCancelled = "cancelled"
)

// Sentinel errors for the subcontracting surface. Callers compare with
// errors.Is; the HTTP layer maps each to a status code in
// writeManufacturingError.
var (
	// ErrSubcontractOrderNotFound is returned when the order does not
	// exist for the caller's tenant.
	ErrSubcontractOrderNotFound = errors.New("manufacturing: subcontract order not found")

	// ErrSubcontractNoComponents is returned by CreateSubcontractOrder
	// when no components are supplied — a subcontract order with nothing
	// to issue is always an authoring mistake.
	ErrSubcontractNoComponents = errors.New("manufacturing: subcontract order has no components")

	// ErrSubcontractDuplicateComponent is returned when the input lists
	// the same component item_id twice (the
	// subcontract_components_order_item_uniq index would otherwise fire a
	// raw 23505).
	ErrSubcontractDuplicateComponent = errors.New("manufacturing: subcontract order lists the same component item more than once")

	// ErrSubcontractInvalidTransition is returned for an illegal status
	// transition (e.g. issuing an already-issued order, receiving a
	// draft order, cancelling once components have been issued).
	ErrSubcontractInvalidTransition = errors.New("manufacturing: invalid subcontract order status transition")

	// ErrSubcontractInsufficientStock is returned by IssueSubcontractOrder
	// when a component's on-hand balance cannot cover the quantity being
	// issued to the supplier.
	ErrSubcontractInsufficientStock = errors.New("manufacturing: insufficient stock to issue subcontract components")
)

// SubcontractOrder is the persisted header of a subcontracting job.
type SubcontractOrder struct {
	TenantID uuid.UUID `json:"tenant_id"`
	ID       uuid.UUID `json:"id"`
	// WorkOrderID / RoutingOperationSeq optionally tie the order to an
	// existing work order's routing operation. Both nil for a standalone
	// out-sourced job.
	WorkOrderID         *uuid.UUID `json:"work_order_id,omitempty"`
	RoutingOperationSeq *int       `json:"routing_operation_seq,omitempty"`
	// SupplierID is an opaque reference to the supplier (a crm
	// organization KRecord); not FK-constrained because suppliers are
	// records, not a typed table.
	SupplierID  *uuid.UUID      `json:"supplier_id,omitempty"`
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	ReceivedQty decimal.Decimal `json:"received_qty"`
	Status      string          `json:"status"`
	// ChargeAmount is the supplier's service fee for the whole job,
	// folded into the receipt move's unit cost so the finished stock
	// carries the subcontracting cost. ChargeCurrency is informational.
	ChargeAmount   decimal.Decimal `json:"charge_amount"`
	ChargeCurrency string          `json:"charge_currency,omitempty"`
	IssuedAt       *time.Time      `json:"issued_at,omitempty"`
	ReceivedAt     *time.Time      `json:"received_at,omitempty"`
	Notes          string          `json:"notes,omitempty"`
	CreatedBy      uuid.UUID       `json:"created_by,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`

	// Components is loaded by GetSubcontractOrder and the issue path.
	Components []SubcontractComponent `json:"components,omitempty"`
}

// SubcontractComponent is one component issued to the supplier for an
// order. Qty is the planned issue quantity; IssuedQty is the running
// actual stamped when the order is issued.
type SubcontractComponent struct {
	TenantID           uuid.UUID       `json:"tenant_id"`
	ID                 uuid.UUID       `json:"id"`
	SubcontractOrderID uuid.UUID       `json:"subcontract_order_id"`
	ItemID             uuid.UUID       `json:"item_id"`
	Qty                decimal.Decimal `json:"qty"`
	IssuedQty          decimal.Decimal `json:"issued_qty"`
	CreatedAt          time.Time       `json:"created_at"`
}

// CanTransitionTo reports whether the subcontract order may move to the
// supplied target status. Exposed as a method so the UI can grey out
// illegal status buttons. Legal transitions:
//
//	draft     → issued      (issue components to the supplier)
//	draft     → cancelled   (abandon before any stock moved)
//	issued    → received    (receive the finished item back)
//	received  → closed      (settle the order)
//	X         → X           (idempotent re-assertion)
//
// issued → cancelled is rejected on purpose: once components have left
// our stock the order carries inventory motion that must be received (or
// reversed deliberately), not silently dropped.
func (o SubcontractOrder) CanTransitionTo(target string) bool {
	if o.Status == target {
		return true
	}
	switch o.Status {
	case SubcontractStatusDraft:
		return target == SubcontractStatusIssued || target == SubcontractStatusCancelled
	case SubcontractStatusIssued:
		return target == SubcontractStatusReceived
	case SubcontractStatusReceived:
		return target == SubcontractStatusClosed
	default:
		return false
	}
}
