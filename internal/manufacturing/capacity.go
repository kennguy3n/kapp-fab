package manufacturing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// maxCapacityWindowDays caps the size of a capacity plan's date range.
// The grid is O(work_centers * days), and the planner walks every day
// in the window, so an unbounded range (or a typo'd far-future end
// date) could allocate an enormous result set. A year is far more than
// any realistic shop-floor planning horizon.
const maxCapacityWindowDays = 366

// ErrCapacityRangeInvalid is returned by CapacityPlanner.Plan when the
// requested window is empty (end before start) or wider than
// maxCapacityWindowDays.
var ErrCapacityRangeInvalid = errors.New("manufacturing: invalid capacity date range")

// DateRange is an inclusive [Start, End] window of calendar days. Only
// the date component is significant; the planner truncates both bounds
// to midnight UTC so the day buckets line up regardless of the clock
// time the caller passes.
type DateRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// CapacityPlanner computes a finite-capacity utilisation grid from the
// released / in-progress work orders' snapshotted routings. It is a
// thin read-only view over the store; it holds the *PGStore rather than
// embedding it so the capacity surface can't accidentally mutate state.
type CapacityPlanner struct {
	store *PGStore
}

// NewCapacityPlanner returns a planner backed by the supplied store.
func NewCapacityPlanner(store *PGStore) *CapacityPlanner {
	return &CapacityPlanner{store: store}
}

// DayLoad is one cell of the capacity grid: the scheduled vs available
// minutes for a single work center on a single calendar day.
type DayLoad struct {
	// Date is the calendar day in RFC-3339 date form (YYYY-MM-DD).
	Date string `json:"date"`
	// ScheduledMinutes is the total operation load placed on the work
	// center that day across all released / in-progress work orders.
	ScheduledMinutes decimal.Decimal `json:"scheduled_minutes"`
	// AvailableMinutes is the work center's schedulable capacity for
	// the day (zero for non-active centers).
	AvailableMinutes decimal.Decimal `json:"available_minutes"`
	// UtilizationPercent is scheduled / available * 100, rounded to
	// two decimals. Zero when the center has no available minutes and
	// no scheduled load; a center with load but zero capacity reports
	// the sentinel 100*  (see Overloaded) rather than dividing by zero.
	UtilizationPercent decimal.Decimal `json:"utilization_percent"`
	// Overloaded is true when scheduled minutes exceed available
	// minutes — the v1 finite-scheduling signal. The planner flags the
	// conflict but does not reschedule.
	Overloaded bool `json:"overloaded"`
}

// WorkCenterSchedule is the per-work-center row of the capacity grid.
type WorkCenterSchedule struct {
	WorkCenterID   uuid.UUID `json:"work_center_id"`
	WorkCenterName string    `json:"work_center_name"`
	Status         string    `json:"status"`
	Days           []DayLoad `json:"days"`
}

// CapacityPlan is the result of CapacityPlanner.Plan: a grid of
// per-work-center, per-day utilisation over the requested window.
type CapacityPlan struct {
	Start time.Time            `json:"start"`
	End   time.Time            `json:"end"`
	Rows  []WorkCenterSchedule `json:"rows"`
}

// operationLoad is an internal row from the work-order × routing join:
// the scheduling load one operation places on a work center on a given
// day.
type operationLoad struct {
	workCenterID uuid.UUID
	day          string
	minutes      decimal.Decimal
}

// Plan computes the capacity-utilisation grid for the supplied date
// range. It joins every released / in-progress work order to its
// snapshotted routing's operations, computes the load minutes each
// operation places on its work center (setup + cycle * planned_qty),
// and forward-schedules those operations across the window.
//
// Scheduling model (v1 finite forward-scheduling): within a work order
// the operations are serial — operation N+1 cannot begin until
// operation N has finished. Starting from the work order's anchor day
// (its scheduled_start, or the window's first day when scheduled_start
// is NULL), each operation's load is laid down on consecutive days,
// with each day absorbing at most that operation's work center's
// available minutes. The next operation resumes on the day the previous
// one finished. This spreads a multi-step or long-running routing across
// the days it actually occupies instead of piling the whole work
// order's load onto its start day. Load that would land beyond the
// window's end is outside this grid and dropped; a work order whose
// scheduled_start itself falls outside the window is ignored entirely.
//
// Every work center in the tenant appears in the grid (even with zero
// load) so the UI can render available capacity, and a center that is
// in maintenance / retired but still carries scheduled load surfaces as
// overloaded (its available minutes are zero, so its load cannot be
// spread and lands on a single day).
func (p *CapacityPlanner) Plan(ctx context.Context, tenantID uuid.UUID, r DateRange) (*CapacityPlan, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	start := r.Start.UTC().Truncate(24 * time.Hour)
	end := r.End.UTC().Truncate(24 * time.Hour)
	if end.Before(start) {
		return nil, fmt.Errorf("%w: end %s is before start %s", ErrCapacityRangeInvalid, end.Format("2006-01-02"), start.Format("2006-01-02"))
	}
	days := int(end.Sub(start).Hours()/24) + 1
	if days > maxCapacityWindowDays {
		return nil, fmt.Errorf("%w: window of %d days exceeds the %d-day maximum", ErrCapacityRangeInvalid, days, maxCapacityWindowDays)
	}

	// Ordered list of day keys spanning the window, plus a key→index
	// map for O(1) "is this day in range, and where?" lookups while
	// forward-scheduling load.
	dayKeys := make([]string, 0, days)
	dayIndex := make(map[string]int, days)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		dayIndex[key] = len(dayKeys)
		dayKeys = append(dayKeys, key)
	}

	workCenters, err := p.store.ListWorkCenters(ctx, tenantID, "")
	if err != nil {
		return nil, err
	}

	// Per-work-center daily capacity, used as the per-day chunk size
	// when spreading an operation's load across consecutive days.
	capacity := make(map[uuid.UUID]decimal.Decimal, len(workCenters))
	for i := range workCenters {
		capacity[workCenters[i].ID] = workCenters[i].AvailableMinutesPerDay()
	}

	loads, err := p.scheduledLoads(ctx, tenantID, dayKeys, dayIndex, capacity)
	if err != nil {
		return nil, err
	}

	// Index scheduled minutes by (work center, day).
	byWC := make(map[uuid.UUID]map[string]decimal.Decimal, len(workCenters))
	for _, l := range loads {
		m, ok := byWC[l.workCenterID]
		if !ok {
			m = make(map[string]decimal.Decimal)
			byWC[l.workCenterID] = m
		}
		m[l.day] = m[l.day].Add(l.minutes)
	}

	rows := make([]WorkCenterSchedule, 0, len(workCenters))
	for i := range workCenters {
		wc := &workCenters[i]
		avail := wc.AvailableMinutesPerDay()
		dayLoads := make([]DayLoad, 0, len(dayKeys))
		for _, key := range dayKeys {
			scheduled := byWC[wc.ID][key]
			dayLoads = append(dayLoads, buildDayLoad(key, scheduled, avail))
		}
		rows = append(rows, WorkCenterSchedule{
			WorkCenterID:   wc.ID,
			WorkCenterName: wc.Name,
			Status:         wc.Status,
			Days:           dayLoads,
		})
	}

	return &CapacityPlan{Start: start, End: end, Rows: rows}, nil
}

// buildDayLoad assembles one grid cell from the scheduled and available
// minutes, computing utilisation and the overloaded flag. Centralised
// so the divide-by-zero guard and rounding live in exactly one place.
func buildDayLoad(day string, scheduled, available decimal.Decimal) DayLoad {
	overloaded := scheduled.GreaterThan(available)
	var util decimal.Decimal
	switch {
	case available.IsPositive():
		util = scheduled.Div(available).Mul(decimal.NewFromInt(100)).Round(2)
	case scheduled.IsPositive():
		// Load scheduled against a center with zero capacity: there
		// is no finite percentage, so report it as fully overloaded
		// rather than dividing by zero. Overloaded already carries
		// the real signal.
		util = decimal.NewFromInt(100)
	default:
		util = decimal.Zero
	}
	return DayLoad{
		Date:               day,
		ScheduledMinutes:   scheduled,
		AvailableMinutes:   available,
		UtilizationPercent: util,
		Overloaded:         overloaded,
	}
}

// woOperations holds one released / in-progress work order's scheduling
// inputs plus its routing operations in sequence order, ready for
// forward-scheduling.
type woOperations struct {
	plannedQty     decimal.Decimal
	scheduledStart *time.Time
	ops            []RoutingOperation
}

// scheduledLoads joins released / in-progress work orders to their
// snapshotted routing operations and forward-schedules each work order's
// operations across the window, returning per-(work center, day) load
// chunks. See Plan for the scheduling model. Anchor rule: the work
// order's scheduled_start day when set and inside the window; the
// window's first day when scheduled_start is NULL; the work order is
// skipped entirely when scheduled_start falls outside the window.
func (p *CapacityPlanner) scheduledLoads(
	ctx context.Context,
	tenantID uuid.UUID,
	dayKeys []string,
	dayIndex map[string]int,
	capacity map[uuid.UUID]decimal.Decimal,
) ([]operationLoad, error) {
	out := make([]operationLoad, 0)
	err := dbutil.WithTenantTx(ctx, p.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Ordered by work order then operation sequence so each work
		// order's rows arrive contiguously and in execution order,
		// letting us group and forward-schedule them in a single pass.
		rows, err := tx.Query(ctx,
			`SELECT wo.id, wo.planned_qty, wo.scheduled_start,
			        ro.sequence, ro.work_center_id,
			        ro.setup_time_minutes, ro.cycle_time_minutes
			   FROM work_orders wo
			   JOIN routing_operations ro
			     ON ro.tenant_id = wo.tenant_id AND ro.routing_id = wo.routing_id
			  WHERE wo.tenant_id = $1
			    AND wo.routing_id IS NOT NULL
			    AND wo.status IN ('released', 'in_progress')
			  ORDER BY wo.id, ro.sequence`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("manufacturing: query capacity load: %w", err)
		}
		defer rows.Close()

		var (
			curWO uuid.UUID
			group *woOperations
		)
		flush := func() {
			if group != nil {
				out = forwardSchedule(out, group, dayKeys, dayIndex, capacity)
			}
		}
		for rows.Next() {
			var (
				woID           uuid.UUID
				plannedQty     decimal.Decimal
				scheduledStart *time.Time
				op             RoutingOperation
			)
			if err := rows.Scan(&woID, &plannedQty, &scheduledStart,
				&op.Sequence, &op.WorkCenterID,
				&op.SetupTimeMinutes, &op.CycleTimeMinutes); err != nil {
				return fmt.Errorf("manufacturing: scan capacity load: %w", err)
			}
			if group == nil || woID != curWO {
				flush()
				curWO = woID
				group = &woOperations{plannedQty: plannedQty, scheduledStart: scheduledStart}
			}
			group.ops = append(group.ops, op)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		flush()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// forwardSchedule lays a single work order's operations onto the day
// grid and appends the resulting per-(work center, day) load chunks to
// dst. Operations are serial: each begins on the day the previous one
// finished, starting from the work order's anchor day. An operation's
// load is spread across consecutive days, each day taking at most the
// operation's work center's available minutes; a center with zero
// available minutes can't be spread, so its whole load lands on one day
// (surfacing as overloaded). Load that runs past the window's end is
// outside the grid and dropped — and because time only moves forward,
// once an operation passes the end the rest of the work order is too, so
// scheduling stops. A work order whose anchor falls outside the window
// contributes nothing.
func forwardSchedule(
	dst []operationLoad,
	wo *woOperations,
	dayKeys []string,
	dayIndex map[string]int,
	capacity map[uuid.UUID]decimal.Decimal,
) []operationLoad {
	cursor := 0 // anchor = window's first day when scheduled_start is NULL
	if wo.scheduledStart != nil {
		key := wo.scheduledStart.UTC().Truncate(24 * time.Hour).Format("2006-01-02")
		idx, ok := dayIndex[key]
		if !ok {
			// Anchored outside the planning window — its load belongs
			// to a different grid.
			return dst
		}
		cursor = idx
	}

	lastIdx := len(dayKeys) - 1
	for i := range wo.ops {
		op := wo.ops[i]
		if cursor > lastIdx {
			// Past the window end; every later operation is later
			// still, so nothing more of this work order fits.
			break
		}
		remaining := op.LoadMinutes(wo.plannedQty)
		if remaining.LessThanOrEqual(decimal.Zero) {
			// A zero-load operation occupies no time; the next
			// operation starts on the same day.
			continue
		}
		perDay := capacity[op.WorkCenterID]
		if perDay.LessThanOrEqual(decimal.Zero) {
			// No schedulable capacity (maintenance / retired / zero
			// hours): can't spread, so the whole load lands on the
			// cursor day and the operation finishes there.
			dst = append(dst, operationLoad{
				workCenterID: op.WorkCenterID,
				day:          dayKeys[cursor],
				minutes:      remaining,
			})
			continue
		}
		for remaining.IsPositive() {
			if cursor > lastIdx {
				break // remainder spills past the window — dropped
			}
			chunk := remaining
			if chunk.GreaterThan(perDay) {
				chunk = perDay
			}
			dst = append(dst, operationLoad{
				workCenterID: op.WorkCenterID,
				day:          dayKeys[cursor],
				minutes:      chunk,
			})
			remaining = remaining.Sub(chunk)
			if remaining.IsPositive() {
				cursor++ // operation continues onto the next day
			}
		}
	}
	return dst
}
