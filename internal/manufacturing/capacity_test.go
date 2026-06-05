package manufacturing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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
