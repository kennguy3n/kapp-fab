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
// and buckets that load onto the work order's scheduled-start day. Work
// orders whose scheduled start falls outside the window are ignored;
// orders with no scheduled start are bucketed on the window's first day
// so their load is never silently dropped.
//
// Every work center in the tenant appears in the grid (even with zero
// load) so the UI can render available capacity, and a center that is
// in maintenance / retired but still carries scheduled load surfaces as
// overloaded (its available minutes are zero).
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

	// Ordered list of day keys spanning the window, plus a set for
	// O(1) "is this day in range?" checks while bucketing load.
	dayKeys := make([]string, 0, days)
	inRange := make(map[string]struct{}, days)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		dayKeys = append(dayKeys, key)
		inRange[key] = struct{}{}
	}
	firstDay := dayKeys[0]

	workCenters, err := p.store.ListWorkCenters(ctx, tenantID, "")
	if err != nil {
		return nil, err
	}

	loads, err := p.scheduledLoads(ctx, tenantID, firstDay, inRange)
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

// scheduledLoads joins released / in-progress work orders to their
// snapshotted routing operations and returns the per-operation load
// bucketed onto a calendar day. Bucketing rule: the work order's
// scheduled_start day when set and inside the window; the window's first
// day when scheduled_start is NULL; skipped entirely when scheduled_start
// falls outside the window.
func (p *CapacityPlanner) scheduledLoads(
	ctx context.Context,
	tenantID uuid.UUID,
	firstDay string,
	inRange map[string]struct{},
) ([]operationLoad, error) {
	out := make([]operationLoad, 0)
	err := dbutil.WithTenantTx(ctx, p.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT wo.planned_qty, wo.scheduled_start,
			        ro.work_center_id, ro.setup_time_minutes, ro.cycle_time_minutes
			   FROM work_orders wo
			   JOIN routing_operations ro
			     ON ro.tenant_id = wo.tenant_id AND ro.routing_id = wo.routing_id
			  WHERE wo.tenant_id = $1
			    AND wo.routing_id IS NOT NULL
			    AND wo.status IN ('released', 'in_progress')`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("manufacturing: query capacity load: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				plannedQty     decimal.Decimal
				scheduledStart *time.Time
				op             RoutingOperation
			)
			if err := rows.Scan(&plannedQty, &scheduledStart,
				&op.WorkCenterID, &op.SetupTimeMinutes, &op.CycleTimeMinutes); err != nil {
				return fmt.Errorf("manufacturing: scan capacity load: %w", err)
			}

			day := firstDay
			if scheduledStart != nil {
				key := scheduledStart.UTC().Truncate(24 * time.Hour).Format("2006-01-02")
				if _, ok := inRange[key]; !ok {
					// Scheduled outside the planning window — its
					// load belongs to a different grid.
					continue
				}
				day = key
			}
			out = append(out, operationLoad{
				workCenterID: op.WorkCenterID,
				day:          day,
				minutes:      op.LoadMinutes(plannedQty),
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
