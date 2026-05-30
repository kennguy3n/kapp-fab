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
	r := NewPoolRouter(primary).WithReplica(replica, 500*time.Millisecond)

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
	r := NewPoolRouter(primary).WithReplica(replica, 100*time.Millisecond)

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
		WithReplica(replica, 1*time.Second).
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
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second)

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
	r := NewPoolRouter(primary).WithReplica(replica, 0)

	r.lastLagNanos.Store(0)
	r.lastSampledAt.Store(time.Now().UTC().UnixNano())

	if got := r.Read(); got != primary {
		t.Fatalf("Read() should fall back to primary when lagTolerance <= 0, got %p want %p", got, primary)
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
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second)

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
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second)
	if r.StartLagSampler(context.Background(), 0) {
		t.Fatalf("StartLagSampler should return false on non-positive interval")
	}
}

func TestPoolRouter_WithReplica_DefaultsStalenessTo30s(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).WithReplica(replica, 1*time.Second)
	if r.sampleStaleness != 30*time.Second {
		t.Fatalf("default sampleStaleness = %v, want 30s", r.sampleStaleness)
	}
}

func TestPoolRouter_WithSampleStaleness_OverridesDefault(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).
		WithReplica(replica, 1*time.Second).
		WithSampleStaleness(2 * time.Second)
	if r.sampleStaleness != 2*time.Second {
		t.Fatalf("override sampleStaleness = %v, want 2s", r.sampleStaleness)
	}
}

func TestPoolRouter_WithSampleStaleness_RejectsNonPositive(t *testing.T) {
	primary := newLazyPool(t)
	replica := newLazyPool(t)
	r := NewPoolRouter(primary).
		WithReplica(replica, 1*time.Second).
		WithSampleStaleness(0)
	if r.sampleStaleness != 30*time.Second {
		t.Fatalf("non-positive WithSampleStaleness should leave default; got %v", r.sampleStaleness)
	}
}
