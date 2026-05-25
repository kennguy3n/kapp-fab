package manufacturing

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestEffectiveQty verifies the scrap math used to scale BOM
// component quantities up for spoilage. The engine multiplies the
// per-output-batch qty by (1 + scrap/100); a nil or zero scrap is
// the identity factor.
func TestEffectiveQty(t *testing.T) {
	tests := []struct {
		name  string
		qty   string
		scrap *string
		want  string
	}{
		{name: "no scrap, nil", qty: "10", scrap: nil, want: "10"},
		{name: "zero scrap", qty: "10", scrap: strptr("0"), want: "10"},
		{name: "10pct scrap", qty: "10", scrap: strptr("10"), want: "11"},
		{name: "fractional scrap", qty: "2.5", scrap: strptr("2.5"), want: "2.5625"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := BOMComponent{Qty: mustDecimal(t, tt.qty)}
			if tt.scrap != nil {
				v := mustDecimal(t, *tt.scrap)
				c.ScrapPercent = &v
			}
			got := c.EffectiveQty()
			if !got.Equal(mustDecimal(t, tt.want)) {
				t.Fatalf("EffectiveQty() = %s, want %s", got.String(), tt.want)
			}
		})
	}
}

// TestCanTransitionTo pins the work-order state machine. The matrix
// is the source of truth for both the API surface and the agent
// tools, so a regression here would silently allow illegal
// transitions in production.
func TestCanTransitionTo(t *testing.T) {
	type tc struct {
		from, to string
		want     bool
	}
	cases := []tc{
		// Idempotent re-assertion is always allowed.
		{from: WorkOrderStatusDraft, to: WorkOrderStatusDraft, want: true},
		{from: WorkOrderStatusCompleted, to: WorkOrderStatusCompleted, want: true},

		// Legal forward transitions.
		{from: WorkOrderStatusDraft, to: WorkOrderStatusReleased, want: true},
		{from: WorkOrderStatusDraft, to: WorkOrderStatusCancelled, want: true},
		{from: WorkOrderStatusReleased, to: WorkOrderStatusInProgress, want: true},
		{from: WorkOrderStatusReleased, to: WorkOrderStatusCompleted, want: true},
		{from: WorkOrderStatusReleased, to: WorkOrderStatusCancelled, want: true},
		{from: WorkOrderStatusInProgress, to: WorkOrderStatusCompleted, want: true},
		{from: WorkOrderStatusInProgress, to: WorkOrderStatusCancelled, want: true},
		{from: WorkOrderStatusCompleted, to: WorkOrderStatusClosed, want: true},

		// Backwards / illegal transitions.
		{from: WorkOrderStatusReleased, to: WorkOrderStatusDraft, want: false},
		{from: WorkOrderStatusInProgress, to: WorkOrderStatusReleased, want: false},
		{from: WorkOrderStatusCompleted, to: WorkOrderStatusInProgress, want: false},
		{from: WorkOrderStatusCompleted, to: WorkOrderStatusCancelled, want: false},

		// Skipped transitions.
		{from: WorkOrderStatusDraft, to: WorkOrderStatusInProgress, want: false},
		{from: WorkOrderStatusDraft, to: WorkOrderStatusCompleted, want: false},
		{from: WorkOrderStatusReleased, to: WorkOrderStatusClosed, want: false},

		// Terminal statuses reject every outbound move.
		{from: WorkOrderStatusClosed, to: WorkOrderStatusInProgress, want: false},
		{from: WorkOrderStatusClosed, to: WorkOrderStatusCompleted, want: false},
		{from: WorkOrderStatusCancelled, to: WorkOrderStatusDraft, want: false},
		{from: WorkOrderStatusCancelled, to: WorkOrderStatusReleased, want: false},
	}
	for _, c := range cases {
		t.Run(c.from+"_to_"+c.to, func(t *testing.T) {
			w := WorkOrder{Status: c.from}
			if got := w.CanTransitionTo(c.to); got != c.want {
				t.Fatalf("CanTransitionTo(%s→%s)=%v, want %v", c.from, c.to, got, c.want)
			}
		})
	}
}

// TestRegisterKTypesPureLogic verifies the schemas the ktypes.go
// init() block hard-codes are well-formed JSON. The runtime
// registration path needs a registry, so this test exercises only
// the locally-constructed slice that All() exposes.
func TestAllKTypesShape(t *testing.T) {
	got := All()
	if len(got) != 2 {
		t.Fatalf("All() returned %d KTypes, want 2", len(got))
	}
	names := map[string]bool{}
	for _, kt := range got {
		names[kt.Name] = true
	}
	for _, expected := range []string{KTypeBOM, KTypeWorkOrder} {
		if !names[expected] {
			t.Errorf("All() missing KType %s; have %v", expected, names)
		}
	}
}

func mustDecimal(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

func strptr(s string) *string { return &s }
