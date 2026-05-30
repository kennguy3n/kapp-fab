package dbutil

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolRouter picks between a primary write pool and an optional
// read-replica pool on a per-call basis. It is the single point of
// truth for "should this query run against the replica?" so the
// decision is centralised instead of being scattered across every
// caller that knows about both pools.
//
// The router is intentionally tiny and concrete (no interface) so the
// hot path on every read is two atomic loads (the cached lag sample +
// the cached sample timestamp) and a single time-delta comparison —
// no allocations, no virtual dispatch.
//
// Semantics:
//
//   - Primary() always returns the write pool. Callers that issue
//     INSERT/UPDATE/DELETE MUST route through Primary() explicitly
//     so a future replica with conflicting WAL position cannot
//     accept a stale write attempt that the application thinks
//     succeeded.
//   - Read() returns the replica when:
//     (a) a replica was registered via WithReplica, AND
//     (b) the most recent lag sample is within tolerance, AND
//     (c) the most recent lag sample is itself fresh enough (i.e.
//     the sampler hasn't fallen behind — if the last successful
//     sample is older than 2× the configured sample interval we
//     treat the replica as "unknown lag" and fall back to primary).
//     When any of those conditions fail, Read() returns the primary.
//     This is the "fail-safe to primary" default: if anything is in
//     doubt, route to the source of truth.
//   - Configuring no replica (NewPoolRouter without WithReplica) makes
//     Read() == Primary() — the router is a no-op wrapper that
//     existing single-pool callers can adopt without behaviour change.
//
// The router does NOT itself open the replica pool; the caller is
// responsible for opening both pools and passing them in. Lifetime
// management (Close on shutdown) is also the caller's job — the
// router holds borrowed references and intentionally does not
// shadow-own the pools' lifecycle.
type PoolRouter struct {
	primary *pgxpool.Pool
	replica *pgxpool.Pool

	// lagTolerance is the maximum acceptable observed replication
	// lag for the replica to be considered usable. Zero means
	// "never use the replica" (effectively disables it), so a
	// caller wiring a replica MUST set a positive tolerance.
	lagTolerance time.Duration

	// sampleStaleness bounds how old the most recent lag observation
	// may be before the router stops trusting it. Defaults to
	// `defaultStalenessMultiplier * sampleInterval` (i.e. 2× the
	// interval the caller passes to WithReplica), floored at
	// `minStalenessFloor` so ticker jitter doesn't flap routing.
	// Explicitly overridable via WithSampleStaleness for tests.
	sampleStaleness time.Duration

	// lastLagNanos holds the most recent observed replication lag
	// in nanoseconds. atomic so the hot read path (Read) never
	// blocks on a mutex.
	lastLagNanos atomic.Int64

	// lastSampledAt holds the wall-clock time (unix nanos UTC) when
	// lastLagNanos was last updated. Reads compare against this to
	// detect a stalled sampler. atomic so writes from the sampler
	// goroutine are visible without a memory barrier on the read.
	lastSampledAt atomic.Int64
}

// NewPoolRouter constructs a router that routes every call to the
// primary pool. Use WithReplica to attach a read replica.
//
// A nil primary panics — callers should always have a primary pool
// because the entire app depends on at least one DB connection;
// returning an error here would only delay the failure to the first
// query.
func NewPoolRouter(primary *pgxpool.Pool) *PoolRouter {
	if primary == nil {
		panic("dbutil: PoolRouter requires a non-nil primary pool")
	}
	return &PoolRouter{primary: primary}
}

// defaultStalenessMultiplier is how many sampler intervals may elapse
// before the most recent observation is considered "too old" to
// trust. The PoolRouter type contract documents this as 2× — see the
// `Read()` semantics in the type docstring above. Keep this in
// lock-step with the doc.
const defaultStalenessMultiplier = 2

// minStalenessFloor is the absolute lower bound for sampleStaleness
// even when 2× the sampler interval is smaller. A sub-second
// staleness window would cause routine ticker jitter (GC pauses,
// scheduling) to flap the router back to primary on every Read()
// call. One second is conservative; ~100 ms of jitter is well below
// it on a healthy host.
const minStalenessFloor = 1 * time.Second

// WithReplica attaches a read replica pool and the lag tolerance the
// router will enforce before routing any read to it. Pass nil
// replica to clear a previously-configured replica (useful for tests
// flipping between configurations); pass a non-positive tolerance to
// effectively disable routing to the replica without un-wiring it.
//
// sampleInterval is the cadence the sampler will tick at (i.e. the
// value the caller will subsequently pass to StartLagSampler). It is
// taken at WithReplica time so the router can derive the default
// sampleStaleness as `defaultStalenessMultiplier * sampleInterval`,
// honouring the documented contract on the type. Pass zero to opt
// out of the auto-default — the caller is then responsible for
// calling WithSampleStaleness explicitly.
//
// Returns the receiver so this can be chained from NewPoolRouter.
//
// IMPORTANT: WithReplica is not safe to call concurrently with
// Read() / SampleLag(). It is intended to be called once during
// process boot before any goroutine starts issuing reads through
// the router. Tests that flip configurations between subtests do so
// serially.
func (r *PoolRouter) WithReplica(replica *pgxpool.Pool, lagTolerance time.Duration, sampleInterval time.Duration) *PoolRouter {
	r.replica = replica
	r.lagTolerance = lagTolerance
	if r.sampleStaleness <= 0 && sampleInterval > 0 {
		staleness := time.Duration(defaultStalenessMultiplier) * sampleInterval
		if staleness < minStalenessFloor {
			staleness = minStalenessFloor
		}
		r.sampleStaleness = staleness
	}
	return r
}

// WithSampleStaleness overrides the "how old can the last sample be"
// bound used by Read(). Primarily for tests; production wiring sets
// this implicitly via WithReplica's sampleInterval (2× interval).
func (r *PoolRouter) WithSampleStaleness(d time.Duration) *PoolRouter {
	if d > 0 {
		r.sampleStaleness = d
	}
	return r
}

// Primary returns the write pool. Always non-nil.
func (r *PoolRouter) Primary() *pgxpool.Pool { return r.primary }

// HasReplica reports whether a replica pool is wired (regardless of
// whether the current lag observation would route a read to it).
// Useful for "do not even attempt the sampler" guards.
func (r *PoolRouter) HasReplica() bool { return r != nil && r.replica != nil }

// Read returns the pool the next read-only query should use. The
// caller MUST treat the returned pool as read-only — Read may return
// the replica, which will reject writes at the wire protocol level.
//
// The fall-through to primary on any uncertainty (no replica
// configured, lag exceeds tolerance, last sample too old) is
// deliberate: a stale or unresponsive replica is exactly when we
// must NOT degrade silently to "older data is fine".
func (r *PoolRouter) Read() *pgxpool.Pool {
	if r == nil || r.replica == nil {
		return r.primary
	}
	if r.lagTolerance <= 0 {
		return r.primary
	}
	sampledAt := r.lastSampledAt.Load()
	if sampledAt == 0 {
		// Sampler has never observed lag — could be a freshly-booted
		// process whose sampler goroutine hasn't yet ticked. Fall
		// back to primary; the next sample will flip Read() to the
		// replica without any caller-visible blip.
		return r.primary
	}
	if time.Since(time.Unix(0, sampledAt)) > r.sampleStaleness {
		// Sampler stalled (DB unreachable from sampler, panic, etc.).
		// Treat the replica as unknown-state and fall back.
		return r.primary
	}
	lag := time.Duration(r.lastLagNanos.Load())
	if lag < 0 || lag > r.lagTolerance {
		return r.primary
	}
	return r.replica
}

// SampleLag executes the replication-lag query against the replica
// and atomically updates the router's cached observation. Returns
// the observed lag.
//
// The query mirrors the one in docs/SCALING_RUNBOOK.md so an
// operator running it by hand sees the same number the router does:
//
//	SELECT GREATEST(0, extract(epoch FROM now() - pg_last_xact_replay_timestamp()))
//
// `pg_last_xact_replay_timestamp` is NULL on a primary (no upstream
// to replay from). When NULL, SampleLag reports zero lag — by
// definition a primary has zero lag relative to itself. This lets
// the same router code be used in dev where DB_URL and
// KAPP_READ_REPLICA_URL point at the same instance.
//
// SampleLag is safe to call from a single dedicated goroutine
// (typically StartLagSampler below); concurrent callers are
// supported via the atomic updates but produce no extra value.
func (r *PoolRouter) SampleLag(ctx context.Context) (time.Duration, error) {
	if r == nil || r.replica == nil {
		return 0, errors.New("dbutil: PoolRouter: no replica configured")
	}
	var lagSeconds float64
	err := r.replica.QueryRow(ctx,
		`SELECT COALESCE(
		    GREATEST(0, EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))),
		    0
		)`,
	).Scan(&lagSeconds)
	if err != nil {
		return 0, fmt.Errorf("dbutil: PoolRouter sample lag: %w", err)
	}
	lag := time.Duration(lagSeconds * float64(time.Second))
	r.lastLagNanos.Store(int64(lag))
	r.lastSampledAt.Store(time.Now().UTC().UnixNano())
	return lag, nil
}

// LastLag returns the most recently observed lag and the wall clock
// time it was observed. Returns (0, time.Time{}, false) when no
// sample has been recorded. Used by the /metrics gauge.
func (r *PoolRouter) LastLag() (time.Duration, time.Time, bool) {
	if r == nil {
		return 0, time.Time{}, false
	}
	at := r.lastSampledAt.Load()
	if at == 0 {
		return 0, time.Time{}, false
	}
	return time.Duration(r.lastLagNanos.Load()), time.Unix(0, at), true
}

// StartLagSampler launches a background goroutine that calls
// SampleLag every interval until ctx is cancelled. Errors during
// sampling are silently swallowed (logging is the caller's job — the
// router has no logger dependency) but the cached sample timestamp
// is NOT advanced on error, so a sustained sampling failure shows
// up as "sample too old" in Read() and routes back to primary.
//
// Returns immediately; sampler runs until ctx is cancelled.
// Returns false (and does nothing) when no replica is configured.
func (r *PoolRouter) StartLagSampler(ctx context.Context, interval time.Duration) bool {
	if r == nil || r.replica == nil || interval <= 0 {
		return false
	}
	go r.runLagSampler(ctx, interval)
	return true
}

// sampleQueryTimeout returns the per-tick query timeout for the
// sampler. Capped at interval/2 so a single slow query can't eat
// the full tick budget — if the replica needs more than half an
// interval just to answer the lag probe, that probe should fail and
// the sample-staleness check in Read() will route subsequent reads
// back to primary. The floor (100ms) protects against absurdly
// short test intervals collapsing to a zero-timeout context.
func sampleQueryTimeout(interval time.Duration) time.Duration {
	t := interval / 2
	if t < 100*time.Millisecond {
		t = 100 * time.Millisecond
	}
	return t
}

func (r *PoolRouter) runLagSampler(ctx context.Context, interval time.Duration) {
	// Take a first sample immediately so Read() can flip to the
	// replica without waiting for the first tick. The initial
	// sample uses the same per-call budget the periodic ticks use
	// so a stuck DB on first probe doesn't block process startup
	// (parent ctx still ultimately bounds it).
	firstCtx, firstCancel := context.WithTimeout(ctx, sampleQueryTimeout(interval))
	_, _ = r.SampleLag(firstCtx)
	firstCancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sampleCtx, cancel := context.WithTimeout(ctx, sampleQueryTimeout(interval))
			_, _ = r.SampleLag(sampleCtx)
			cancel()
		}
	}
}
