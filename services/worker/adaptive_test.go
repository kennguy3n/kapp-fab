package main

import (
	"testing"
	"time"
)

const testBudget = 100 * time.Millisecond

// TestAdaptiveBatcher_ConstructorClamps locks in that the constructor
// produces a usable batcher for any input — start out-of-range gets
// clamped, an inverted min/max gets normalised to min=max, a zero
// latency budget falls back to 250ms. The drain loop calls
// NewAdaptiveBatcher exactly once at boot and panics on any failure
// would crash the worker, so the constructor must never reject input.
func TestAdaptiveBatcher_ConstructorClamps(t *testing.T) {
	cases := []struct {
		name           string
		start, min, max int
		budget          time.Duration
		wantLimit       int
		wantBudget      time.Duration
	}{
		{"in-range start", 100, 50, 1000, testBudget, 100, testBudget},
		{"start below min clamps up", 10, 50, 1000, testBudget, 50, testBudget},
		{"start above max clamps down", 5000, 50, 1000, testBudget, 1000, testBudget},
		{"max below min normalises", 100, 200, 50, testBudget, 200, testBudget},
		{"min below 1 floors to 1", 5, 0, 100, testBudget, 5, testBudget},
		{"min below 1 with start 0", 0, -5, 100, testBudget, 1, testBudget},
		{"zero budget falls back to 250ms", 100, 50, 1000, 0, 100, 250 * time.Millisecond},
		{"negative budget falls back", 100, 50, 1000, -1, 100, 250 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewAdaptiveBatcher(tc.start, tc.min, tc.max, tc.budget)
			if got := b.Limit(); got != tc.wantLimit {
				t.Errorf("Limit() = %d, want %d", got, tc.wantLimit)
			}
			if got := b.Snapshot().LatencyBudget; got != tc.wantBudget {
				t.Errorf("budget = %s, want %s", got, tc.wantBudget)
			}
		})
	}
}

// TestAdaptiveBatcher_GrowsOnSaturation: the queue keeps returning a
// full batch in well under budget — we must keep growing until we hit
// max. This is the "we're behind on a backlogged queue and want
// throughput" path.
func TestAdaptiveBatcher_GrowsOnSaturation(t *testing.T) {
	b := NewAdaptiveBatcher(100, 50, 1000, testBudget)
	prev := b.Limit()
	for i := 0; i < 20; i++ {
		// Always full, always fast.
		b.Observe(b.Limit(), testBudget/4)
		got := b.Limit()
		if got < prev && prev < 1000 {
			t.Fatalf("iteration %d: limit shrunk from %d to %d under saturation+fast", i, prev, got)
		}
		prev = got
	}
	if prev != 1000 {
		t.Errorf("saturated growth did not reach max in 20 iterations; final=%d", prev)
	}
}

// TestAdaptiveBatcher_ShrinksOnIdle: zero events drained in well
// under budget — the queue is empty and we are paying roundtrip cost
// for nothing. Shrink toward min so an idle steady state costs less.
func TestAdaptiveBatcher_ShrinksOnIdle(t *testing.T) {
	b := NewAdaptiveBatcher(1000, 50, 1000, testBudget)
	prev := b.Limit()
	for i := 0; i < 30; i++ {
		b.Observe(0, testBudget/10)
		got := b.Limit()
		if got > prev && prev > 50 {
			t.Fatalf("iteration %d: limit grew from %d to %d on idle drain", i, prev, got)
		}
		prev = got
	}
	if prev != 50 {
		t.Errorf("idle shrink did not reach min in 30 iterations; final=%d", prev)
	}
}

// TestAdaptiveBatcher_ShrinksOnOverBudget: drain is going over the
// latency budget regardless of how full the batch was. Throughput
// must NOT be prioritised over latency — shrink eagerly.
func TestAdaptiveBatcher_ShrinksOnOverBudget(t *testing.T) {
	b := NewAdaptiveBatcher(500, 50, 1000, testBudget)
	prev := b.Limit()
	for i := 0; i < 10; i++ {
		// Saturated (full batch) but over budget. Latency wins.
		b.Observe(b.Limit(), testBudget*2)
		got := b.Limit()
		if got > prev {
			t.Fatalf("iteration %d: limit grew from %d to %d over budget", i, prev, got)
		}
		prev = got
	}
	if prev >= 500 {
		t.Errorf("over-budget shrink did not reduce limit; final=%d", prev)
	}
}

// TestAdaptiveBatcher_StableUnderModerateLoad: a non-full, non-idle
// batch under budget should NOT cause oscillation. The queue is
// keeping up at the current batch size and the batcher should hold
// steady.
func TestAdaptiveBatcher_StableUnderModerateLoad(t *testing.T) {
	b := NewAdaptiveBatcher(100, 50, 1000, testBudget)
	start := b.Limit()
	for i := 0; i < 50; i++ {
		// Half full, fast.
		b.Observe(b.Limit()/2, testBudget/4)
	}
	if got := b.Limit(); got != start {
		t.Errorf("limit drifted under moderate steady load: started %d, now %d", start, got)
	}
}

// TestAdaptiveBatcher_Snapshot pins the snapshot fields after a
// representative call so future refactors of the snapshot shape
// don't silently drop fields the metrics pipeline depends on.
func TestAdaptiveBatcher_Snapshot(t *testing.T) {
	b := NewAdaptiveBatcher(100, 50, 1000, testBudget)
	b.Observe(42, 75*time.Millisecond)
	s := b.Snapshot()
	if s.Min != 50 || s.Max != 1000 {
		t.Errorf("Min/Max not preserved: %+v", s)
	}
	if s.LatencyBudget != testBudget {
		t.Errorf("LatencyBudget not preserved: %s", s.LatencyBudget)
	}
	if s.LastDrainCount != 42 {
		t.Errorf("LastDrainCount = %d, want 42", s.LastDrainCount)
	}
	if s.LastDuration != 75*time.Millisecond {
		t.Errorf("LastDuration = %s, want 75ms", s.LastDuration)
	}
}
