package manufacturing

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// KType identifiers for the Stream 2 manufacturing-depth surface.
// Kept alongside the Phase N6 constants in manufacturing.go so the
// API, agent tools, and tests reference the same strings.
const (
	KTypeRouting    = "manufacturing.routing"
	KTypeWorkCenter = "manufacturing.work_center"
	KTypeJobCard    = "manufacturing.job_card"
)

// RoutingStatus enumerates the legal values for routings.status.
// Mirrors the BOM lifecycle: a routing is authored as draft, published
// to active (auto-demoting any previously-active routing for the same
// item), then retired to obsolete.
const (
	RoutingStatusDraft    = "draft"
	RoutingStatusActive   = "active"
	RoutingStatusObsolete = "obsolete"
)

// WorkCenterStatus enumerates the legal values for work_centers.status.
// Capacity planning only counts active work centers as available;
// maintenance / retired centers still appear in the grid (so their
// scheduled load surfaces as a conflict) but contribute zero available
// minutes.
const (
	WorkCenterStatusActive      = "active"
	WorkCenterStatusMaintenance = "maintenance"
	WorkCenterStatusRetired     = "retired"
)

// Sentinel errors for the routing / work-center surface. Callers
// compare with errors.Is so the store can wrap them with context
// without breaking equality. The HTTP layer maps each to a status
// code in writeManufacturingError.
var (
	// ErrRoutingNotFound is returned by GetRouting / SetRoutingStatus
	// when the routing does not exist for the caller's tenant.
	ErrRoutingNotFound = errors.New("manufacturing: routing not found")

	// ErrRoutingNotActive is returned when an item has no routing in
	// `active` status (e.g. when a caller explicitly requests the
	// active routing). Work-order release tolerates this — a missing
	// routing simply means no job cards are generated.
	ErrRoutingNotActive = errors.New("manufacturing: item has no active routing")

	// ErrRoutingHasNoOperations is returned when activating a routing
	// with zero operations — an empty routing produces no job cards
	// and no capacity load, so activating it is always a mistake.
	ErrRoutingHasNoOperations = errors.New("manufacturing: routing has no operations")

	// ErrRoutingInvalidTransition is returned for an illegal routing
	// status transition (e.g. obsolete → active). See
	// Routing.CanTransitionTo for the matrix.
	ErrRoutingInvalidTransition = errors.New("manufacturing: invalid routing status transition")

	// ErrRoutingDuplicateSequence is returned by CreateRouting when the
	// input lists the same operation `sequence` twice. The
	// (tenant_id, routing_id, sequence) primary key would otherwise
	// surface a raw 23505.
	ErrRoutingDuplicateSequence = errors.New("manufacturing: routing lists the same operation sequence more than once")

	// ErrWorkCenterNotFound is returned by GetWorkCenter /
	// SetWorkCenterStatus when the work center does not exist for the
	// caller's tenant.
	ErrWorkCenterNotFound = errors.New("manufacturing: work center not found")

	// ErrWorkCenterDuplicateName is returned by CreateWorkCenter when a
	// work center with the same name already exists for the tenant
	// (the (tenant_id, name) unique constraint). Names are the handle
	// the capacity grid renders, so a duplicate is always an authoring
	// mistake.
	ErrWorkCenterDuplicateName = errors.New("manufacturing: work center name already exists")
)

// WorkCenter is a machine or workstation with a finite hourly
// capacity. Available minutes per day derate the nominal throughput by
// the configured efficiency (see AvailableMinutesPerDay).
type WorkCenter struct {
	TenantID uuid.UUID `json:"tenant_id"`
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	// CapacityPerHour is the throughput in output units per hour at
	// 100% efficiency. Informational for now (the v1 capacity engine
	// schedules on operation minutes, not unit throughput) but stored
	// so a later finite-scheduler can convert between the two.
	CapacityPerHour      decimal.Decimal `json:"capacity_per_hour"`
	OperatingHoursPerDay decimal.Decimal `json:"operating_hours_per_day"`
	EfficiencyPercent    decimal.Decimal `json:"efficiency_percent"`
	Status               string          `json:"status"`
	Notes                string          `json:"notes,omitempty"`
	CreatedBy            uuid.UUID       `json:"created_by,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// AvailableMinutesPerDay is the schedulable capacity of the work center
// for a single day: operating_hours_per_day * 60, derated by the
// efficiency factor. A non-active work center contributes zero
// available minutes so any load scheduled against it is flagged as a
// conflict rather than silently absorbed.
//
// Exposed as a method (rather than inlined in the planner) so the
// capacity math is unit-testable without a database.
func (wc WorkCenter) AvailableMinutesPerDay() decimal.Decimal {
	if wc.Status != WorkCenterStatusActive {
		return decimal.Zero
	}
	minutesPerDay := wc.OperatingHoursPerDay.Mul(decimal.NewFromInt(60))
	factor := wc.EfficiencyPercent.Div(decimal.NewFromInt(100))
	return minutesPerDay.Mul(factor)
}

// Routing is a versioned, ordered sequence of operations for producing
// an item. Only one routing per item may be active at a time (enforced
// by the routings_active_per_item_uniq partial unique index). A work
// order snapshots the active routing onto work_orders.routing_id at
// release time so the generated job cards stay reproducible.
type Routing struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	ID        uuid.UUID `json:"id"`
	ItemID    uuid.UUID `json:"item_id"`
	Version   string    `json:"version"`
	Status    string    `json:"status"`
	Notes     string    `json:"notes,omitempty"`
	CreatedBy uuid.UUID `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Operations is loaded by GetRouting and the work-order release
	// path. Empty for partial fetches (e.g. ListRoutings) so the
	// slice's length is not a reliable existence check on its own.
	Operations []RoutingOperation `json:"operations,omitempty"`
}

// CanTransitionTo reports whether the routing may move to the supplied
// target status. The matrix is identical to BOM.CanTransitionTo and is
// enforced by SetRoutingStatus; exposing it as a method lets the UI
// grey out illegal status buttons. Legal transitions:
//
//	draft    → active           (publish the routing)
//	draft    → obsolete         (abandon a draft)
//	active   → obsolete         (retire — replaced by a new version)
//	obsolete → (terminal)       (resurrect by authoring a new version)
//	X        → X                (idempotent re-assertion)
func (r Routing) CanTransitionTo(target string) bool {
	if r.Status == target {
		return true
	}
	switch r.Status {
	case RoutingStatusDraft:
		return target == RoutingStatusActive || target == RoutingStatusObsolete
	case RoutingStatusActive:
		return target == RoutingStatusObsolete
	case RoutingStatusObsolete:
		return false
	default:
		return false
	}
}

// RoutingOperation is one step on a routing. setup_time_minutes is a
// fixed per-run cost (incurred once regardless of batch size);
// cycle_time_minutes is per produced unit. The capacity engine computes
// the minutes a work order places on the operation's work center as
// setup + cycle * planned_qty (see LoadMinutes).
type RoutingOperation struct {
	RoutingID        uuid.UUID       `json:"routing_id"`
	Sequence         int             `json:"sequence"`
	OperationName    string          `json:"operation_name"`
	WorkCenterID     uuid.UUID       `json:"work_center_id"`
	SetupTimeMinutes decimal.Decimal `json:"setup_time_minutes"`
	CycleTimeMinutes decimal.Decimal `json:"cycle_time_minutes"`
	Description      string          `json:"description,omitempty"`
}

// LoadMinutes is the scheduling load (in minutes) this operation places
// on its work center to produce `qty` units: the fixed setup cost plus
// the per-unit cycle time scaled by quantity. A negative quantity is
// clamped to zero so a bad input can never credit capacity back.
func (op RoutingOperation) LoadMinutes(qty decimal.Decimal) decimal.Decimal {
	if qty.IsNegative() {
		qty = decimal.Zero
	}
	return op.SetupTimeMinutes.Add(op.CycleTimeMinutes.Mul(qty))
}
