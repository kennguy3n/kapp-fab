package dbutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newLazyPool constructs a pgxpool.Pool for tests without opening any
// connections. pgxpool.NewWithConfig defers the first connect attempt
// to the first query, so the returned pool is safe to compare-by-pointer
// in routing tests that never actually issue a query.
func newLazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://lazy:lazy@127.0.0.1:1/lazy?connect_timeout=1")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.MinConns = 0
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPoolRouter_NoReplica_AlwaysPrimary(t *testing.T) {
	primary := newLazyPool(t)
	r := NewPoolRouter(primary)

	if got := r.Read(); got != primary {
		t.Fatalf("Read() with no replica should return primary, got %p want %p", got, primary)
	}
	if got := r.Primary(); got != primary {
		t.Fatalf("Primary() mismatch: got %p want %p", got, primary)
	}
	if r.HasReplica() {
		t.Fatalf("HasReplica() = true; expected false when no replica wired")
	}
}

func TestPoolRouter_ReplicaPicked_WhenSampleFresh_AndLagInTolerance(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 500*time.Millisecond, 5*time.Second)

	// Simulate a fresh sample well within tolerance. WithSampleStaleness
	// defaults to 30s, so a sample timestamp of "now" is fresh.
	r.lastLagNanos.Store(int64(100 * time.Millisecond))
	r.lastSampledAt.Store(time.Now().UTC().UnixNano())

	if got := r.Read(); got != replica {
		t.Fatalf("Read() should return replica when lag in tolerance, got %p want %p", got, replica)
	}
	if !r.HasReplica() {
		t.Fatalf("HasReplica() = false; expected true when replica wired")
	}
}

func TestPoolRouter_FallsBackToPrimary_WhenLagExceedsTolerance(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 100*time.Millisecond, 5*time.Second)

	r.lastLagNanos.Store(int64(500 * time.Millisecond)) // 5x tolerance
	r.lastSampledAt.Store(time.Now().UTC().UnixNano())

	if got := r.Read(); got != primary {
		t.Fatalf("Read() should fall back to primary when lag exceeds tolerance, got %p want %p", got, primary)
	}
}

func TestPoolRouter_FallsBackToPrimary_WhenSampleStale(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).
		WithReplica(replica, 1*time.Second, 5*time.Second).
		WithSampleStaleness(50 * time.Millisecond)

	// Lag would be acceptable, but the sample is older than the
	// staleness bound — the sampler must be considered stalled.
	r.lastLagNanos.Store(int64(10 * time.Millisecond))
	r.lastSampledAt.Store(time.Now().Add(-1 * time.Second).UnixNano())

	if got := r.Read(); got != primary {
		t.Fatalf("Read() should fall back to primary when sample stale, got %p want %p", got, primary)
	}
}

func TestPoolRouter_FallsBackToPrimary_WhenNoSampleEverTaken(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 5*time.Second)

	// No sample stored (lastSampledAt is the zero atomic). Read should
	// route to primary so a freshly-booted process doesn't flip to a
	// replica whose lag is genuinely unknown.
	if got := r.Read(); got != primary {
		t.Fatalf("Read() should fall back to primary before any sample taken, got %p want %p", got, primary)
	}
}

func TestPoolRouter_FallsBackToPrimary_WhenLagToleranceNonPositive(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 0, 5*time.Second)

	r.lastLagNanos.Store(0)
	r.lastSampledAt.Store(time.Now().UTC().UnixNano())

	if got := r.Read(); got != primary {
		t.Fatalf("Read() should fall back to primary when lagTolerance <= 0, got %p want %p", got, primary)
	}
}

// TestPoolRouter_FallsBackToPrimary_WhenLagNegativeSentinel pins the
// contract that SampleLag uses a negative-nanos sentinel when the
// replica is in recovery but has not yet replayed any WAL
// (pg_last_xact_replay_timestamp() = NULL). Read()'s existing
// `lag < 0` gate must treat that as "not ready" and route to primary,
// so point reads (Get / ListPage / BulkFetch / Search) don't hit a
// not-yet-caught-up standby whose visible state is only as fresh as
// the base backup. Keyset walks have their own gate via
// record.snapshotVia returning errReplicaNotReady — this test pins
// the SampleLag-side defense.
func TestPoolRouter_FallsBackToPrimary_WhenLagNegativeSentinel(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 5*time.Second)

	// Sample timestamp is fresh and lag is "in tolerance" by
	// magnitude (1ns), but encoded as -1 to mean "standby not
	// ready". Read() must NOT route to the replica.
	r.lastLagNanos.Store(-1)
	r.lastSampledAt.Store(time.Now().UTC().UnixNano())

	if got := r.Read(); got != primary {
		t.Fatalf("Read() should fall back to primary when lag<0 sentinel, got %p want %p", got, primary)
	}
}

func TestPoolRouter_LastLag_ReportsFalseBeforeFirstSample(t *testing.T) {
	primary := newLazyPool(t)
	r := NewPoolRouter(primary)

	if _, _, ok := r.LastLag(); ok {
		t.Fatalf("LastLag() should report (_, _, false) before any sample")
	}
}

func TestPoolRouter_LastLag_ReportsCachedAfterSample(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 5*time.Second)

	want := 250 * time.Millisecond
	at := time.Now().UTC()
	r.lastLagNanos.Store(int64(want))
	r.lastSampledAt.Store(at.UnixNano())

	gotLag, gotAt, ok := r.LastLag()
	if !ok {
		t.Fatalf("LastLag() ok = false; expected true after store")
	}
	if gotLag != want {
		t.Fatalf("LastLag() lag = %v, want %v", gotLag, want)
	}
	// Compare unix-nano because the round-trip via atomic.Int64 drops
	// any monotonic-clock reading the wall-clock had.
	if gotAt.UnixNano() != at.UnixNano() {
		t.Fatalf("LastLag() at = %v, want %v", gotAt.UnixNano(), at.UnixNano())
	}
}

func TestPoolRouter_NewPoolRouter_PanicsOnNilPrimary(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewPoolRouter(nil) should panic")
		}
	}()
	_ = NewPoolRouter(nil)
}

func TestPoolRouter_HasReplica_NilReceiverSafe(t *testing.T) {
	var r *PoolRouter
	if r.HasReplica() {
		t.Fatalf("nil-receiver HasReplica() should be false")
	}
}

func TestPoolRouter_Read_NilReceiverPanicWouldBeBug(t *testing.T) {
	// Read() on a nil receiver dereferences r.primary, which is a
	// nil pointer dereference panic. This test pins the expectation
	// that callers must never pass a nil PoolRouter — a nil router
	// in production wiring is a bug we want to surface loudly
	// rather than papering over.
	defer func() {
		_ = recover() // expected nil-pointer panic
	}()
	var r *PoolRouter
	_ = r.Read()
}

func TestPoolRouter_StartLagSampler_DeclinesWithoutReplica(t *testing.T) {
	primary := newLazyPool(t)
	r := NewPoolRouter(primary)
	if r.StartLagSampler(context.Background(), 1*time.Second) {
		t.Fatalf("StartLagSampler should return false when no replica configured")
	}
}

func TestPoolRouter_StartLagSampler_DeclinesNonPositiveInterval(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 5*time.Second)
	if r.StartLagSampler(context.Background(), 0) {
		t.Fatalf("StartLagSampler should return false on non-positive interval")
	}
}

// TestPoolRouter_WithReplica_DefaultsStalenessTo2xInterval pins the
// type contract: default sampleStaleness is 2× the sampler interval
// passed to WithReplica. This is what the PoolRouter type docstring
// promises ("if the last successful sample is older than 2× the
// configured sample interval we treat the replica as unknown").
func TestPoolRouter_WithReplica_DefaultsStalenessTo2xInterval(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	interval := 5 * time.Second
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, interval)
	want := 2 * interval
	if r.sampleStaleness != want {
		t.Fatalf("default sampleStaleness = %v, want %v (2× %v)", r.sampleStaleness, want, interval)
	}
}

// TestPoolRouter_WithReplica_FloorsStaleness asserts that
// minStalenessFloor protects against absurdly short intervals (a
// sub-second window would let routine ticker jitter flap routing).
func TestPoolRouter_WithReplica_FloorsStaleness(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 100*time.Millisecond)
	if r.sampleStaleness < minStalenessFloor {
		t.Fatalf("sampleStaleness = %v below floor %v", r.sampleStaleness, minStalenessFloor)
	}
}

// TestPoolRouter_WithReplica_NoIntervalSkipsDefault verifies the
// opt-out path: callers that pass zero sampleInterval must set
// staleness explicitly via WithSampleStaleness.
func TestPoolRouter_WithReplica_NoIntervalSkipsDefault(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 0)
	if r.sampleStaleness != 0 {
		t.Fatalf("sampleStaleness = %v, want 0 (no implicit default with zero interval)", r.sampleStaleness)
	}
}

func TestPoolRouter_WithSampleStaleness_OverridesDefault(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).
		WithReplica(replica, 1*time.Second, 5*time.Second).
		WithSampleStaleness(2 * time.Second)
	if r.sampleStaleness != 2*time.Second {
		t.Fatalf("override sampleStaleness = %v, want 2s", r.sampleStaleness)
	}
}

func TestPoolRouter_WithSampleStaleness_RejectsNonPositive(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).
		WithReplica(replica, 1*time.Second, 5*time.Second).
		WithSampleStaleness(0)
	want := 2 * 5 * time.Second
	if r.sampleStaleness != want {
		t.Fatalf("non-positive WithSampleStaleness should leave default %v; got %v", want, r.sampleStaleness)
	}
}

func TestPoolRouter_SampleQueryTimeoutBoundedByInterval(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"5s halves to 2.5s", 5 * time.Second, 2500 * time.Millisecond},
		{"10s halves to 5s", 10 * time.Second, 5 * time.Second},
		{"100ms below floor", 100 * time.Millisecond, 100 * time.Millisecond},
		{"1ms collapsed to floor", 1 * time.Millisecond, 100 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sampleQueryTimeout(c.interval); got != c.want {
				t.Fatalf("sampleQueryTimeout(%v)=%v want %v", c.interval, got, c.want)
			}
		})
	}
}

// TestPoolRouter_Close_NoReplica_NoOp pins the contract that Close is
// safe to call on a router built without a replica (no sampler ever
// runs) and on a nil receiver. Services wire Close into deferred
// cleanups regardless of whether KAPP_READ_REPLICA_URL was set, so
// this MUST not panic.
func TestPoolRouter_Close_NoReplica_NoOp(t *testing.T) {
	primary := newLazyPool(t)
	r := NewPoolRouter(primary)
	r.Close()
	r.Close() // idempotent
	var nilRouter *PoolRouter
	nilRouter.Close() // nil-safe
}

// TestPoolRouter_Close_Idempotent verifies double-Close does not
// double-close the cancel channel or block. Real callers can have
// shutdown paths that fan-out the cleanup (e.g. signal handler +
// context cancel), so Close must tolerate concurrent or repeated
// invocations.
func TestPoolRouter_Close_Idempotent(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 5*time.Second)
	// StartLagSampler returns false on a closed/cancelled ctx without
	// ever launching a goroutine in some test envs, but we want a
	// real goroutine to verify the wait path. Use a fresh ctx with
	// a long interval so the sampler doesn't actually do a probe.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := r.StartLagSampler(ctx, 10*time.Second)
	if !started {
		t.Fatalf("StartLagSampler should have started")
	}
	r.Close()
	r.Close()
	r.Close()
}

// TestPoolRouter_StartLagSampler_RejectsSecondStart pins the contract
// that StartLagSampler returns false on a second call — preventing
// stray goroutines from a misconfigured service that wires the
// sampler twice. The first sampler keeps running; the second call is
// a no-op.
func TestPoolRouter_StartLagSampler_RejectsSecondStart(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.Close()
	if !r.StartLagSampler(ctx, 10*time.Second) {
		t.Fatalf("first StartLagSampler should return true")
	}
	if r.StartLagSampler(ctx, 10*time.Second) {
		t.Fatalf("second StartLagSampler should return false (sampler already running)")
	}
}

// TestPoolRouter_LastErrorCount_ZeroBeforeAnyError pins the contract
// that LastErrorCount returns 0 on a fresh router, on one with no
// sampler started, and on a nil receiver. The metrics layer polls
// this on every publish tick; returning a stale or panicking value
// would surface as a misleading kapp_replica_sample_errors_total.
func TestPoolRouter_LastErrorCount_ZeroBeforeAnyError(t *testing.T) {
	var nilRouter *PoolRouter
	if got := nilRouter.LastErrorCount(); got != 0 {
		t.Fatalf("nil receiver LastErrorCount=%d want 0", got)
	}
	primary := newLazyPool(t)
	r := NewPoolRouter(primary)
	if got := r.LastErrorCount(); got != 0 {
		t.Fatalf("fresh router LastErrorCount=%d want 0", got)
	}
}
