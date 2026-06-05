package manufacturing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TestBuildDayLoad pins the per-cell capacity math: utilisation is
// scheduled/available*100 rounded to two decimals, Overloaded fires
// strictly when scheduled exceeds available, and a center with load but
// zero capacity reports 100% (the divide-by-zero guard) rather than
// panicking.
func TestBuildDayLoad(t *testing.T) {
	t.Parallel()
	const day = "2026-01-15"
	cases := []struct {
		name           string
		scheduled      string
		available      string
		wantUtil       string
		wantOverloaded bool
	}{
		{name: "half utilised", scheduled: "240", available: "480", wantUtil: "50", wantOverloaded: false},
		{name: "exactly at capacity is not overloaded", scheduled: "480", available: "480", wantUtil: "100", wantOverloaded: false},
		{name: "over capacity flags overloaded", scheduled: "600", available: "480", wantUtil: "125", wantOverloaded: true},
		{name: "utilisation rounds to two decimals", scheduled: "100", available: "3", wantUtil: "3333.33", wantOverloaded: true},
		{name: "idle center", scheduled: "0", available: "480", wantUtil: "0", wantOverloaded: false},
		{name: "load against zero capacity reports 100pct overloaded", scheduled: "120", available: "0", wantUtil: "100", wantOverloaded: true},
		{name: "no load and no capacity is zero, not overloaded", scheduled: "0", available: "0", wantUtil: "0", wantOverloaded: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildDayLoad(day, mustDecimal(t, tc.scheduled), mustDecimal(t, tc.available))
			if got.Date != day {
				t.Errorf("Date = %q, want %q", got.Date, day)
			}
			if !got.UtilizationPercent.Equal(mustDecimal(t, tc.wantUtil)) {
				t.Errorf("UtilizationPercent = %s, want %s", got.UtilizationPercent.String(), tc.wantUtil)
			}
			if got.Overloaded != tc.wantOverloaded {
				t.Errorf("Overloaded = %v, want %v", got.Overloaded, tc.wantOverloaded)
			}
			if !got.ScheduledMinutes.Equal(mustDecimal(t, tc.scheduled)) {
				t.Errorf("ScheduledMinutes = %s, want %s", got.ScheduledMinutes.String(), tc.scheduled)
			}
			if !got.AvailableMinutes.Equal(mustDecimal(t, tc.available)) {
				t.Errorf("AvailableMinutes = %s, want %s", got.AvailableMinutes.String(), tc.available)
			}
		})
	}
}

// TestPlanRejectsInvalidRange verifies Plan validates the window before
// touching the database: an empty (end-before-start) window and one
// wider than maxCapacityWindowDays both return ErrCapacityRangeInvalid.
// The planner is backed by a store with a nil pool to prove the
// validation short-circuits before any query runs.
func TestPlanRejectsInvalidRange(t *testing.T) {
	t.Parallel()
	planner := NewCapacityPlanner(&PGStore{})
	tenantID := uuid.New()
	base := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "end before start", start: base, end: base.AddDate(0, 0, -1)},
		{name: "window exceeds max", start: base, end: base.AddDate(0, 0, maxCapacityWindowDays)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := planner.Plan(context.Background(), tenantID, DateRange{Start: tc.start, End: tc.end})
			if !errors.Is(err, ErrCapacityRangeInvalid) {
				t.Fatalf("Plan() error = %v, want ErrCapacityRangeInvalid", err)
			}
		})
	}
}

// TestPlanRejectsNilTenant guards the tenant-id precondition, which
// also short-circuits before any DB access.
func TestPlanRejectsNilTenant(t *testing.T) {
	t.Parallel()
	planner := NewCapacityPlanner(&PGStore{})
	base := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	_, err := planner.Plan(context.Background(), uuid.Nil, DateRange{Start: base, End: base})
	if err == nil {
		t.Fatal("Plan() with nil tenant id: expected error, got nil")
	}
	if errors.Is(err, ErrCapacityRangeInvalid) {
		t.Fatalf("Plan() with nil tenant id returned range error, want tenant-id error: %v", err)
	}
}

// makeDayGrid builds the ordered day-key slice and key→index map that
// forwardSchedule consumes, for a window of n days starting at base.
func makeDayGrid(base time.Time, n int) (keys []string, idx map[string]int) {
	keys = make([]string, 0, n)
	idx = make(map[string]int, n)
	for i := 0; i < n; i++ {
		key := base.AddDate(0, 0, i).Format("2006-01-02")
		idx[key] = len(keys)
		keys = append(keys, key)
	}
	return keys, idx
}

// sumLoads folds a forwardSchedule result into a work-center → day →
// total-minutes map so assertions are order-independent of chunking.
func sumLoads(loads []operationLoad) map[uuid.UUID]map[string]decimal.Decimal {
	out := make(map[uuid.UUID]map[string]decimal.Decimal)
	for _, l := range loads {
		m, ok := out[l.workCenterID]
		if !ok {
			m = make(map[string]decimal.Decimal)
			out[l.workCenterID] = m
		}
		m[l.day] = m[l.day].Add(l.minutes)
	}
	return out
}

// ptrTime returns a pointer to t, for the scheduledStart field.
func ptrTime(t time.Time) *time.Time { return &t }

// TestForwardSchedule exercises the v1 finite forward-scheduling model:
// serial operations laid down from the anchor day, each spread across
// consecutive days capped at its work center's daily capacity, the next
// operation resuming where the previous finished, with out-of-window
// anchors skipped and past-the-end load dropped.
func TestForwardSchedule(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	day := func(i int) string { return base.AddDate(0, 0, i).Format("2006-01-02") }

	wcA := uuid.New() // 480 min/day
	wcB := uuid.New() // 100 min/day
	wcDead := uuid.New()

	capacity := map[uuid.UUID]decimal.Decimal{
		wcA:    decimal.NewFromInt(480),
		wcB:    decimal.NewFromInt(100),
		wcDead: decimal.Zero,
	}
	cases := []struct {
		name      string
		days      int
		wo        *woOperations
		wantByWC  map[uuid.UUID]map[string]string
		wantEmpty bool
	}{
		{
			name: "single op fits in one day",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(100),
				scheduledStart: ptrTime(base),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcA, SetupTimeMinutes: decimal.NewFromInt(30), CycleTimeMinutes: decimal.NewFromInt(2)}, // setup 30 plus cycle 2 by qty 100 yields 230
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{wcA: {day(0): "230"}},
		},
		{
			name: "single op spans multiple days at capacity",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(250),
				scheduledStart: ptrTime(base),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcB, CycleTimeMinutes: decimal.NewFromInt(1)}, // load 250 against capacity 100 per day
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{wcB: {day(0): "100", day(1): "100", day(2): "50"}},
		},
		{
			name: "serial ops: second resumes where first finished",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(250),
				scheduledStart: ptrTime(base),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcB, CycleTimeMinutes: decimal.NewFromInt(1)},                                           // load 250 fills days 0,1,2 and ends on day 2
					{Sequence: 2, WorkCenterID: wcA, SetupTimeMinutes: decimal.NewFromInt(30), CycleTimeMinutes: decimal.NewFromInt(0)}, // load 30, resumes on day 2
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{
				wcB: {day(0): "100", day(1): "100", day(2): "50"},
				wcA: {day(2): "30"},
			},
		},
		{
			name: "null scheduled_start anchors at first window day",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(100),
				scheduledStart: nil,
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcA, SetupTimeMinutes: decimal.NewFromInt(30), CycleTimeMinutes: decimal.NewFromInt(2)},
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{wcA: {day(0): "230"}},
		},
		{
			name: "zero-capacity center dumps whole load on one day",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(200),
				scheduledStart: ptrTime(base),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcDead, CycleTimeMinutes: decimal.NewFromInt(1)}, // load 200 against zero capacity
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{wcDead: {day(0): "200"}},
		},
		{
			// A zero-capacity op can't be spread, so it neither advances
			// the cursor nor leaves room on its day; two in a row stack
			// their whole loads on the same (overloaded) day.
			name: "consecutive zero-capacity ops stack on the same day",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(50),
				scheduledStart: ptrTime(base),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcDead, CycleTimeMinutes: decimal.NewFromInt(1)}, // load 50 against zero capacity
					{Sequence: 2, WorkCenterID: wcDead, CycleTimeMinutes: decimal.NewFromInt(2)}, // load 100 against zero capacity, same day
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{wcDead: {day(0): "150"}},
		},
		{
			name: "load past window end is dropped",
			days: 2, // only day 0 and day 1
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(250),
				scheduledStart: ptrTime(base),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcB, CycleTimeMinutes: decimal.NewFromInt(1)}, // load 250 fills days 0 and 1 then the last 50 spills past the window end
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{wcB: {day(0): "100", day(1): "100"}},
		},
		{
			name: "anchor before window contributes nothing",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(100),
				scheduledStart: ptrTime(base.AddDate(0, 0, -1)),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcA, CycleTimeMinutes: decimal.NewFromInt(1)},
				},
			},
			wantEmpty: true,
		},
		{
			name: "anchor after window contributes nothing",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(100),
				scheduledStart: ptrTime(base.AddDate(0, 0, 10)),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcA, CycleTimeMinutes: decimal.NewFromInt(1)},
				},
			},
			wantEmpty: true,
		},
		{
			name: "zero-load op is skipped and does not advance the cursor",
			days: 5,
			wo: &woOperations{
				plannedQty:     decimal.NewFromInt(100),
				scheduledStart: ptrTime(base),
				ops: []RoutingOperation{
					{Sequence: 1, WorkCenterID: wcA, SetupTimeMinutes: decimal.Zero, CycleTimeMinutes: decimal.Zero}, // zero load, skipped without advancing the cursor
					{Sequence: 2, WorkCenterID: wcB, CycleTimeMinutes: decimal.NewFromInt(1)},                        // load 100, starts on day 0
				},
			},
			wantByWC: map[uuid.UUID]map[string]string{wcB: {day(0): "100"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dayKeys, dayIndex := makeDayGrid(base, tc.days)
			got := forwardSchedule(nil, tc.wo, dayKeys, dayIndex, capacity)

			if tc.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("expected no load, got %+v", got)
				}
				return
			}

			byWC := sumLoads(got)
			// Every expected cell present and equal.
			for wc, days := range tc.wantByWC {
				for d, want := range days {
					gotVal := byWC[wc][d]
					if !gotVal.Equal(mustDecimal(t, want)) {
						t.Errorf("wc %s day %s = %s, want %s", wc, d, gotVal.String(), want)
					}
				}
			}
			// No unexpected cells.
			for wc, days := range byWC {
				for d, v := range days {
					if _, ok := tc.wantByWC[wc][d]; !ok && v.IsPositive() {
						t.Errorf("unexpected load wc %s day %s = %s", wc, d, v.String())
					}
				}
			}
		})
	}
}

// TestForwardScheduleConservesLoadWithinWindow verifies that when the
// window is large enough to hold the whole work order, no load is lost:
// the sum of scheduled minutes equals the sum of each operation's
// LoadMinutes. This is the property the single-day-bucketing v0 also
// held; multi-day spreading must preserve it.
func TestForwardScheduleConservesLoadWithinWindow(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	dayKeys, dayIndex := makeDayGrid(base, 60)

	wc1, wc2 := uuid.New(), uuid.New()
	capacity := map[uuid.UUID]decimal.Decimal{
		wc1: decimal.NewFromInt(120),
		wc2: decimal.NewFromInt(75),
	}
	wo := &woOperations{
		plannedQty:     decimal.NewFromInt(40),
		scheduledStart: ptrTime(base),
		ops: []RoutingOperation{
			{Sequence: 1, WorkCenterID: wc1, SetupTimeMinutes: decimal.NewFromInt(15), CycleTimeMinutes: decimal.NewFromInt(3)}, // setup 15 plus cycle 3 by qty 40 yields 135
			{Sequence: 2, WorkCenterID: wc2, SetupTimeMinutes: decimal.NewFromInt(10), CycleTimeMinutes: decimal.NewFromInt(2)}, // setup 10 plus cycle 2 by qty 40 yields 90
		},
	}
	want := wo.ops[0].LoadMinutes(wo.plannedQty).Add(wo.ops[1].LoadMinutes(wo.plannedQty))

	got := forwardSchedule(nil, wo, dayKeys, dayIndex, capacity)
	total := decimal.Zero
	for _, l := range got {
		total = total.Add(l.minutes)
	}
	if !total.Equal(want) {
		t.Fatalf("total scheduled minutes = %s, want %s (load must be conserved within the window)", total.String(), want.String())
	}
}
