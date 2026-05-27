package inventory

import (
	"testing"
)

func TestIsKnownCycleCountStatus(t *testing.T) {
	for _, ok := range []string{
		CycleCountStatusDraft, CycleCountStatusCounting,
		CycleCountStatusReconciled, CycleCountStatusPosted,
	} {
		if !isKnownCycleCountStatus(ok) {
			t.Errorf("isKnownCycleCountStatus(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "foo", "DRAFT", "counted"} {
		if isKnownCycleCountStatus(bad) {
			t.Errorf("isKnownCycleCountStatus(%q) = true, want false", bad)
		}
	}
}

func TestCanTransitionCycleCount(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{CycleCountStatusDraft, CycleCountStatusDraft, true},
		{CycleCountStatusDraft, CycleCountStatusCounting, true},
		{CycleCountStatusDraft, CycleCountStatusReconciled, false},
		{CycleCountStatusDraft, CycleCountStatusPosted, false},
		{CycleCountStatusCounting, CycleCountStatusReconciled, true},
		{CycleCountStatusCounting, CycleCountStatusDraft, true},
		{CycleCountStatusCounting, CycleCountStatusPosted, false},
		{CycleCountStatusReconciled, CycleCountStatusCounting, true},
		{CycleCountStatusReconciled, CycleCountStatusDraft, false},
		{CycleCountStatusReconciled, CycleCountStatusPosted, false},
		{CycleCountStatusPosted, CycleCountStatusReconciled, false},
		{CycleCountStatusPosted, CycleCountStatusDraft, false},
	}
	for _, tc := range cases {
		got := canTransitionCycleCount(tc.from, tc.to)
		if got != tc.want {
			t.Errorf("canTransitionCycleCount(%q, %q) = %v, want %v",
				tc.from, tc.to, got, tc.want)
		}
	}
}
