package manufacturing

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestRoutingCanTransitionTo pins the routing status state machine.
// The matrix is the source of truth for both SetRoutingStatus and the
// agent / UI surfaces, so a regression here would silently allow an
// illegal lifecycle move (e.g. resurrecting an obsolete routing) in
// production.
func TestRoutingCanTransitionTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from string
		to   string
		want bool
	}{
		// Idempotent re-assertion is always allowed so HTTP / KChat
		// retries don't fail.
		{RoutingStatusDraft, RoutingStatusDraft, true},
		{RoutingStatusActive, RoutingStatusActive, true},
		{RoutingStatusObsolete, RoutingStatusObsolete, true},

		// Legal forward transitions.
		{RoutingStatusDraft, RoutingStatusActive, true},
		{RoutingStatusDraft, RoutingStatusObsolete, true},
		{RoutingStatusActive, RoutingStatusObsolete, true},

		// Illegal / backwards transitions.
		{RoutingStatusActive, RoutingStatusDraft, false},
		{RoutingStatusObsolete, RoutingStatusActive, false},
		{RoutingStatusObsolete, RoutingStatusDraft, false},

		// Unknown source status rejects every outbound move.
		{"bogus", RoutingStatusActive, false},
	}
	for _, tc := range cases {
		r := Routing{Status: tc.from}
		if got := r.CanTransitionTo(tc.to); got != tc.want {
			t.Errorf("Routing{%s}.CanTransitionTo(%s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestRoutingOperationLoadMinutes verifies the scheduling-load formula
// the capacity engine relies on: setup is a fixed per-run cost and
// cycle is per produced unit, so load = setup + cycle * qty. A negative
// quantity is clamped to zero so a bad input can never credit capacity
// back to a work center.
func TestRoutingOperationLoadMinutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup string
		cycle string
		qty   string
		want  string
	}{
		{name: "setup plus cycle scaled by qty", setup: "30", cycle: "2", qty: "100", want: "230"},
		{name: "zero qty leaves only setup", setup: "30", cycle: "2", qty: "0", want: "30"},
		{name: "fractional cycle and qty", setup: "0", cycle: "1.5", qty: "10", want: "15"},
		{name: "negative qty clamps to zero (setup only)", setup: "12", cycle: "5", qty: "-4", want: "12"},
		{name: "all zero", setup: "0", cycle: "0", qty: "0", want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op := RoutingOperation{
				SetupTimeMinutes: mustDecimal(t, tc.setup),
				CycleTimeMinutes: mustDecimal(t, tc.cycle),
			}
			got := op.LoadMinutes(mustDecimal(t, tc.qty))
			if !got.Equal(mustDecimal(t, tc.want)) {
				t.Fatalf("LoadMinutes(%s) = %s, want %s", tc.qty, got.String(), tc.want)
			}
		})
	}
}

// TestWorkCenterAvailableMinutesPerDay pins the capacity-derate math:
// available = operating_hours * 60 * efficiency/100, and a non-active
// work center contributes zero available minutes so any load scheduled
// against it surfaces as a conflict rather than being silently
// absorbed.
func TestWorkCenterAvailableMinutesPerDay(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status string
		hours  string
		eff    string
		want   string
	}{
		{name: "8h at 100pct = 480", status: WorkCenterStatusActive, hours: "8", eff: "100", want: "480"},
		{name: "8h at 90pct = 432", status: WorkCenterStatusActive, hours: "8", eff: "90", want: "432"},
		{name: "10h at 75pct = 450", status: WorkCenterStatusActive, hours: "10", eff: "75", want: "450"},
		{name: "over-100pct efficiency allowed", status: WorkCenterStatusActive, hours: "8", eff: "120", want: "576"},
		{name: "maintenance contributes zero", status: WorkCenterStatusMaintenance, hours: "8", eff: "100", want: "0"},
		{name: "retired contributes zero", status: WorkCenterStatusRetired, hours: "8", eff: "100", want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wc := WorkCenter{
				Status:               tc.status,
				OperatingHoursPerDay: mustDecimal(t, tc.hours),
				EfficiencyPercent:    mustDecimal(t, tc.eff),
			}
			got := wc.AvailableMinutesPerDay()
			if !got.Equal(mustDecimal(t, tc.want)) {
				t.Fatalf("AvailableMinutesPerDay() = %s, want %s", got.String(), tc.want)
			}
		})
	}
}

// TestWorkCenterAvailableMinutesPerDayZeroValue guards the degenerate
// case where a center is active but has zero operating hours — it must
// report zero available minutes (and not panic on the division).
func TestWorkCenterAvailableMinutesPerDayZeroValue(t *testing.T) {
	t.Parallel()
	wc := WorkCenter{
		Status:               WorkCenterStatusActive,
		OperatingHoursPerDay: decimal.Zero,
		EfficiencyPercent:    decimal.NewFromInt(100),
	}
	if got := wc.AvailableMinutesPerDay(); !got.IsZero() {
		t.Fatalf("AvailableMinutesPerDay() = %s, want 0", got.String())
	}
}
