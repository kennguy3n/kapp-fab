package main

import (
	"sync/atomic"
	"time"
)

// AdaptiveBatcher computes the next DrainBatch `limit` argument based
// on the observed throughput and latency of recent drains. The goal is
// to keep one drain comfortably under a latency budget (so the outbox
// stays responsive under bursty load) while also growing the batch
// when the queue is consistently saturated (so tail latency on a
// backlogged outbox doesn't get stuck at the initial small batch).
//
// Design choices:
//
//   - Bounded growth/shrink. The batcher operates between min and max.
//     A misbehaving deliver function cannot run away with megabatch
//     sizes that monopolise the worker, and a slow steady-state
//     cannot starve the batcher down to single-event batches that
//     amortise pgxpool roundtrip overhead poorly.
//
//   - Latency-first. If the most recent drain exceeded the latency
//     budget we shrink REGARDLESS of how full the batch was. Throughput
//     is meaningless if it comes at the cost of unbounded p99.
//
//   - Saturation-driven growth. We only grow when the LAST batch came
//     back full (n == limit) AND the latency was comfortably under
//     budget. A non-full batch means the queue is keeping up, so
//     growing would just sit on items longer.
//
//   - Conservative shrink. Shrink only when the queue is genuinely
//     not contributing useful work (n == 0 or n < limit/4) AND
//     latency was well under budget. This prevents the limit from
//     oscillating between min and max on a steady moderate load.
//
//   - Multiplicative steps. Grow by ~1.5x, shrink by ~1.5x. This
//     converges quickly on order-of-magnitude regime changes and
//     resists thrash on near-the-threshold workloads (a single
//     batch sliding above-budget doesn't reset us to min).
//
//   - Atomic Limit() read. The drain loop reads Limit() once per
//     iteration; Observe() writes once per iteration. The two are
//     coupled by sync/atomic so the batcher is safe to read from
//     a future telemetry goroutine without holding a mutex.
type AdaptiveBatcher struct {
	min            int64
	max            int64
	latencyBudget  time.Duration
	current        atomic.Int64
	lastDrainCount atomic.Int64
	lastDuration   atomic.Int64 // nanoseconds; widened from time.Duration for atomic
}

// NewAdaptiveBatcher constructs a batcher with sensible defaults and
// the supplied bounds. start is clamped into [min, max] so a
// misconfigured caller (e.g. start above max) gets a usable batcher
// at the boundary rather than a panic.
func NewAdaptiveBatcher(start, min, max int, latencyBudget time.Duration) *AdaptiveBatcher {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if start < min {
		start = min
	}
	if start > max {
		start = max
	}
	if latencyBudget <= 0 {
		latencyBudget = 250 * time.Millisecond
	}
	b := &AdaptiveBatcher{
		min:           int64(min),
		max:           int64(max),
		latencyBudget: latencyBudget,
	}
	b.current.Store(int64(start))
	return b
}

// Limit returns the recommended `limit` argument for the next
// DrainBatch call. Safe for concurrent reads.
func (b *AdaptiveBatcher) Limit() int {
	return int(b.current.Load())
}

// Observe records the outcome of a drain and updates the recommended
// limit for the next call. n is the number of events actually drained,
// duration is the wall-clock time the drain took (including the
// deliver callback). Safe for concurrent writes; the policy is
// formulated such that interleaved Observes converge correctly.
func (b *AdaptiveBatcher) Observe(n int, duration time.Duration) {
	b.lastDrainCount.Store(int64(n))
	b.lastDuration.Store(int64(duration))
	current := b.current.Load()
	limit := current

	switch {
	case duration > b.latencyBudget:
		// Over-budget. Shrink eagerly.
		limit = shrink(limit)
	case int64(n) == current && duration < b.latencyBudget/2:
		// Saturated AND fast. Room to grow.
		limit = grow(limit)
	case n == 0 && duration < b.latencyBudget/2:
		// Idle. The drain is paying the round-trip cost for
		// nothing. Shrink so the next idle drain costs less if
		// it stays idle, and so a sudden burst lands on a
		// reasonable starting point.
		limit = shrink(limit)
	case int64(n)*4 < current && duration < b.latencyBudget/2:
		// Under-utilised: the queue gave us less than a quarter
		// of what we asked for, and we had time to spare. Shrink
		// to match observed throughput.
		limit = shrink(limit)
	}

	if limit < b.min {
		limit = b.min
	}
	if limit > b.max {
		limit = b.max
	}
	b.current.Store(limit)
}

// Snapshot returns the batcher's current state for telemetry. All
// values are reads of atomic counters so the snapshot may not be
// internally consistent across the four fields, but each individual
// value is well-defined.
func (b *AdaptiveBatcher) Snapshot() AdaptiveBatcherSnapshot {
	return AdaptiveBatcherSnapshot{
		Limit:          int(b.current.Load()),
		Min:            int(b.min),
		Max:            int(b.max),
		LatencyBudget:  b.latencyBudget,
		LastDrainCount: int(b.lastDrainCount.Load()),
		LastDuration:   time.Duration(b.lastDuration.Load()),
	}
}

// AdaptiveBatcherSnapshot is a value-typed view of the batcher's
// state used by Snapshot. Exposed for Phase 4 metric emission.
type AdaptiveBatcherSnapshot struct {
	Limit          int
	Min            int
	Max            int
	LatencyBudget  time.Duration
	LastDrainCount int
	LastDuration   time.Duration
}

func grow(v int64) int64 {
	// Multiplicative growth (1.5x) rounded up. The +1 guarantees
	// forward progress for small values where /2 would round to
	// zero increment.
	return v + v/2 + 1
}

func shrink(v int64) int64 {
	// Multiplicative shrink (1/1.5 ≈ 0.667). Floor at v-1 so we
	// always make progress even when v is small.
	out := (v * 2) / 3
	if out >= v {
		out = v - 1
	}
	return out
}
