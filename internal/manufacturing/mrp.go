package manufacturing

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Material Requirements Planning (MRP).
//
// An MRP run takes a snapshot of independent demand (sales orders, work
// orders, or min-stock top-ups), nets it against supply (on-hand stock
// plus the open work orders already scheduled to receive the item),
// explodes the active BOM of every make item to derive the dependent
// demand for its components, and emits planned orders (make vs buy) with
// a suggested release date computed by backward scheduling from the
// demand due date over the item's lead time. The run and its outputs are
// persisted (see migration 000092) so a planner can audit which inputs
// produced which planned orders.
//
// The planning math (low-level coding, netting, BOM explosion, backward
// scheduling) lives in the pure planMRP function so it is unit-testable
// without a database; the PGStore.RunMRP method (store_mrp.go) builds the
// in-memory snapshot from the tenant's tables and persists the result.

// MRP demand sources. source records where an independent demand line
// originated so the run stays auditable.
const (
	MRPDemandSourceSalesOrder = "sales_order"
	MRPDemandSourceWorkOrder  = "work_order"
	MRPDemandSourceMinStock   = "min_stock"
	MRPDemandSourceManual     = "manual"
)

// MRP planned-order types. An item with an active BOM is made in-house
// (and exploded into component demand); anything else is bought.
const (
	MRPOrderTypeMake = "make"
	MRPOrderTypeBuy  = "buy"
)

// MRP run statuses.
const (
	MRPRunStatusCompleted = "completed"
	MRPRunStatusFailed    = "failed"
)

// defaultBuyLeadTimeDays is the purchasing lead time used to backward
// schedule buy planned orders when the caller does not supply one.
const defaultBuyLeadTimeDays = 7

// defaultMakeLeadTimeDays is the lead time assigned to a make planned
// order whose item has no active routing — a make order still consumes
// roughly a day of shop time, so the suggested start lands one day
// before the due date rather than on it.
const defaultMakeLeadTimeDays = 1

// mrpMaxExplosionLevels caps the BOM explosion depth so a pathological
// (cyclic) BOM graph cannot loop forever. SME bills of material are a
// handful of levels deep; 50 is comfortably beyond any real recipe.
const mrpMaxExplosionLevels = 50

// Sentinel errors for the MRP surface. Callers compare with errors.Is;
// the HTTP layer maps each to a status code in writeManufacturingError.
var (
	// ErrMRPRunNotFound is returned by GetMRPRun when the run does not
	// exist for the caller's tenant.
	ErrMRPRunNotFound = errors.New("manufacturing: mrp run not found")

	// ErrMRPNoDemand is returned by RunMRP when the run would consider
	// zero demand — no explicit demand lines and min-stock topping-up
	// disabled. A run with nothing to plan is always a caller mistake.
	ErrMRPNoDemand = errors.New("manufacturing: mrp run has no demand to plan")

	// ErrMRPCyclicBOM is returned when the active BOM graph contains a
	// cycle (item A's BOM consumes B whose BOM consumes A). Explosion
	// cannot terminate, so the run is rejected rather than looping.
	ErrMRPCyclicBOM = errors.New("manufacturing: cyclic bom detected during mrp explosion")

	// ErrMRPInvalidBOMOutputQty is returned when an active BOM reaches
	// the explosion step with a non-positive output_qty. CreateBOM
	// rejects such values up front, so this only fires on corrupt or
	// legacy data; the run is rejected rather than dividing by zero.
	ErrMRPInvalidBOMOutputQty = errors.New("manufacturing: bom output_qty must be positive for mrp explosion")
)

// MRPRunInput is the canonical input for RunMRP. Demand carries the
// independent demand lines; IncludeMinStock additionally tops up items
// below their inventory reorder_level. The horizon bounds which demand
// due dates the run plans (lines due after HorizonEnd are ignored).
type MRPRunInput struct {
	HorizonStart    time.Time
	HorizonEnd      time.Time
	IncludeMinStock bool
	// BuyLeadTimeDays is the fixed purchasing lead time used to backward
	// schedule buy planned orders. Zero falls back to
	// defaultBuyLeadTimeDays.
	BuyLeadTimeDays int
	Demand          []MRPDemandInput
	Notes           string
}

// MRPDemandInput is one independent demand line supplied to a run. Qty
// must be positive; DueDate is when the quantity is required.
type MRPDemandInput struct {
	ItemID    uuid.UUID
	Qty       decimal.Decimal
	DueDate   time.Time
	Source    string
	SourceRef string
}

// MRPRun is the persisted header of an executed run plus (when loaded by
// GetMRPRun) its demand snapshot and planned-order output.
type MRPRun struct {
	TenantID          uuid.UUID `json:"tenant_id"`
	ID                uuid.UUID `json:"id"`
	Status            string    `json:"status"`
	HorizonStart      time.Time `json:"horizon_start"`
	HorizonEnd        time.Time `json:"horizon_end"`
	IncludeMinStock   bool      `json:"include_min_stock"`
	BuyLeadTimeDays   int       `json:"buy_lead_time_days"`
	DemandLineCount   int       `json:"demand_line_count"`
	PlannedOrderCount int       `json:"planned_order_count"`
	MakeOrderCount    int       `json:"make_order_count"`
	BuyOrderCount     int       `json:"buy_order_count"`
	Notes             string    `json:"notes,omitempty"`
	CreatedBy         uuid.UUID `json:"created_by,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	DemandLines   []MRPDemandLine   `json:"demand_lines,omitempty"`
	PlannedOrders []MRPPlannedOrder `json:"planned_orders,omitempty"`
}

// MRPDemandLine is one persisted independent demand line a run was
// computed against.
type MRPDemandLine struct {
	TenantID  uuid.UUID       `json:"tenant_id"`
	ID        uuid.UUID       `json:"id"`
	RunID     uuid.UUID       `json:"run_id"`
	ItemID    uuid.UUID       `json:"item_id"`
	Qty       decimal.Decimal `json:"qty"`
	DueDate   time.Time       `json:"due_date"`
	Source    string          `json:"source"`
	SourceRef string          `json:"source_ref,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// MRPPlannedOrder is one netted planned order produced by a run.
type MRPPlannedOrder struct {
	TenantID           uuid.UUID       `json:"tenant_id"`
	ID                 uuid.UUID       `json:"id"`
	RunID              uuid.UUID       `json:"run_id"`
	ItemID             uuid.UUID       `json:"item_id"`
	OrderType          string          `json:"order_type"`
	Qty                decimal.Decimal `json:"qty"`
	DueDate            time.Time       `json:"due_date"`
	SuggestedStartDate time.Time       `json:"suggested_start_date"`
	ExplosionLevel     int             `json:"explosion_level"`
	BOMID              *uuid.UUID      `json:"bom_id,omitempty"`
	RoutingID          *uuid.UUID      `json:"routing_id,omitempty"`
	LeadTimeDays       int             `json:"lead_time_days"`
	CreatedAt          time.Time       `json:"created_at"`
}

// mrpItemData is the per-item supply snapshot the planner nets demand
// against. ActiveBOM is nil for purchased (buy) items; ActiveRouting is
// nil for make items with no routing.
type mrpItemData struct {
	OnHand        decimal.Decimal
	Scheduled     decimal.Decimal
	ActiveBOM     *BOM
	ActiveRouting *Routing
}

// mrpDemand is the accumulated gross demand for one item: a quantity and
// the earliest (most constraining) date it is required by.
type mrpDemand struct {
	qty decimal.Decimal
	due time.Time
}

// planMRP is the pure planning kernel. Given the independent demand, the
// per-item supply snapshot, the work-center available-minutes map (for
// routing-derived make lead times) and the purchasing lead time, it
// returns the netted planned orders ordered by BOM explosion level then
// item id. It is database-free so the netting / explosion / scheduling
// math is unit-testable in isolation.
//
// Algorithm (textbook low-level-coded MRP):
//  1. Assign every reachable item a low-level code = the deepest level it
//     appears at across all active-BOM paths from a demand item. Cycles
//     are rejected (ErrMRPCyclicBOM).
//  2. Seed gross demand and the constraining due date from the
//     independent demand.
//  3. Process items in ascending low-level order (so an item is netted
//     only after every parent that contributes demand to it has been
//     processed). For each item: net = gross - on-hand - scheduled
//     receipts. If net > 0 emit a planned order; make items (those with
//     an active BOM) explode their net into component gross demand due by
//     the make order's suggested start date.
func planMRP(
	independent map[uuid.UUID]mrpDemand,
	data map[uuid.UUID]mrpItemData,
	wcAvail map[uuid.UUID]decimal.Decimal,
	buyLeadDays int,
) ([]MRPPlannedOrder, error) {
	if buyLeadDays <= 0 {
		buyLeadDays = defaultBuyLeadTimeDays
	}

	level, err := computeLowLevelCodes(independent, data)
	if err != nil {
		return nil, err
	}

	gross := make(map[uuid.UUID]mrpDemand, len(independent))
	for item, d := range independent {
		gross[item] = d
	}

	var planned []MRPPlannedOrder
	// Explosion may add new items (components) to gross; iterate until no
	// unprocessed item remains, always picking the lowest-level item next.
	processed := make(map[uuid.UUID]struct{}, len(gross))
	for {
		// Pick the unprocessed item with the smallest level (ties broken
		// by item id) so parents are always netted before children.
		var next uuid.UUID
		found := false
		for item := range gross {
			if _, done := processed[item]; done {
				continue
			}
			if !found || lessByLevel(level, item, next) {
				next = item
				found = true
			}
		}
		if !found {
			break
		}
		processed[next] = struct{}{}

		d := gross[next]
		info := data[next]
		net := d.qty.Sub(info.OnHand).Sub(info.Scheduled)
		if net.LessThanOrEqual(decimal.Zero) {
			continue
		}

		isMake := info.ActiveBOM != nil
		leadDays := buyLeadDays
		var bomID, routingID *uuid.UUID
		if isMake {
			bomID = &info.ActiveBOM.ID
			leadDays = makeLeadTimeDays(info.ActiveRouting, net, wcAvail)
			if info.ActiveRouting != nil {
				routingID = &info.ActiveRouting.ID
			}
		}
		start := backwardSchedule(d.due, leadDays)

		orderType := MRPOrderTypeBuy
		if isMake {
			orderType = MRPOrderTypeMake
		}
		planned = append(planned, MRPPlannedOrder{
			ItemID:             next,
			OrderType:          orderType,
			Qty:                net,
			DueDate:            d.due,
			SuggestedStartDate: start,
			ExplosionLevel:     level[next],
			BOMID:              bomID,
			RoutingID:          routingID,
			LeadTimeDays:       leadDays,
		})

		if !isMake {
			continue
		}
		// Explode net into component gross demand, due by this make
		// order's suggested start date.
		bom := info.ActiveBOM
		// OutputQty is the per-batch yield and the explosion divisor.
		// CreateBOM enforces a positive value, but guard here too so a
		// corrupt/legacy row can't panic this otherwise-pure planner.
		if !bom.OutputQty.IsPositive() {
			return nil, fmt.Errorf("%w: item %s bom %s", ErrMRPInvalidBOMOutputQty, next, bom.ID)
		}
		for _, c := range bom.Components {
			compQty := c.EffectiveQty().Mul(net).Div(bom.OutputQty)
			cur, ok := gross[c.ComponentItemID]
			if !ok {
				gross[c.ComponentItemID] = mrpDemand{qty: compQty, due: start}
			} else {
				cur.qty = cur.qty.Add(compQty)
				if start.Before(cur.due) {
					cur.due = start
				}
				gross[c.ComponentItemID] = cur
			}
		}
	}

	sort.SliceStable(planned, func(i, j int) bool {
		if planned[i].ExplosionLevel != planned[j].ExplosionLevel {
			return planned[i].ExplosionLevel < planned[j].ExplosionLevel
		}
		return planned[i].ItemID.String() < planned[j].ItemID.String()
	})
	return planned, nil
}

// lessByLevel orders items by ascending low-level code, breaking ties by
// item id so the selection in planMRP is deterministic.
func lessByLevel(level map[uuid.UUID]int, a, b uuid.UUID) bool {
	if level[a] != level[b] {
		return level[a] < level[b]
	}
	return a.String() < b.String()
}

// computeLowLevelCodes assigns each reachable item the deepest level it
// appears at across all active-BOM explosion paths rooted at the
// independent-demand items. A child always gets a strictly greater level
// than its parent, so processing items in ascending level guarantees an
// item is netted only after every contributing parent. Cycles are
// rejected with ErrMRPCyclicBOM.
func computeLowLevelCodes(
	independent map[uuid.UUID]mrpDemand,
	data map[uuid.UUID]mrpItemData,
) (map[uuid.UUID]int, error) {
	level := make(map[uuid.UUID]int)
	onPath := make(map[uuid.UUID]struct{})

	var visit func(item uuid.UUID, lvl int) error
	visit = func(item uuid.UUID, lvl int) error {
		if lvl > mrpMaxExplosionLevels {
			return ErrMRPCyclicBOM
		}
		if _, cycle := onPath[item]; cycle {
			return ErrMRPCyclicBOM
		}
		if cur, ok := level[item]; ok && cur >= lvl {
			// Already recorded at an equal or deeper level; no deeper
			// path can be discovered by re-descending from here.
			return nil
		}
		level[item] = lvl
		info, ok := data[item]
		if !ok || info.ActiveBOM == nil {
			return nil
		}
		onPath[item] = struct{}{}
		for _, c := range info.ActiveBOM.Components {
			if err := visit(c.ComponentItemID, lvl+1); err != nil {
				return err
			}
		}
		delete(onPath, item)
		return nil
	}

	for item := range independent {
		if err := visit(item, 0); err != nil {
			return nil, err
		}
	}
	return level, nil
}

// makeLeadTimeDays computes the lead time (in whole days) for a make
// planned order. With an active routing the lead time is the sum over
// operations of ceil(operation load minutes / work-center available
// minutes per day) — the same serial-per-order, spread-each-operation
// model the capacity planner uses (capacity.go). Without a routing it
// falls back to defaultMakeLeadTimeDays.
func makeLeadTimeDays(routing *Routing, qty decimal.Decimal, wcAvail map[uuid.UUID]decimal.Decimal) int {
	if routing == nil || len(routing.Operations) == 0 {
		return defaultMakeLeadTimeDays
	}
	total := 0
	for _, op := range routing.Operations {
		load := op.LoadMinutes(qty)
		if load.LessThanOrEqual(decimal.Zero) {
			continue
		}
		avail, ok := wcAvail[op.WorkCenterID]
		if !ok || avail.LessThanOrEqual(decimal.Zero) {
			// Work center has no schedulable capacity (inactive, zero
			// hours). The operation still consumes time; charge a single
			// day rather than dividing by zero.
			total++
			continue
		}
		days := load.Div(avail).Ceil()
		d := int(days.IntPart())
		if d < 1 {
			d = 1
		}
		total += d
	}
	if total < 1 {
		// Every operation had zero load (e.g. qty rounding); a make order
		// still takes at least the default day.
		total = defaultMakeLeadTimeDays
	}
	return total
}

// backwardSchedule subtracts leadDays calendar days from due to get the
// suggested start date. Negative lead is clamped to zero so the start
// never lands after the due date (the migration enforces start <= due).
func backwardSchedule(due time.Time, leadDays int) time.Time {
	if leadDays < 0 {
		leadDays = 0
	}
	return due.AddDate(0, 0, -leadDays)
}

// truncateToDate strips the clock component so demand/horizon dates
// compare cleanly against the DATE columns.
func truncateToDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
