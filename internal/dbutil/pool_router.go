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
	// may be before the router stops trusting it. Defaults to 2× the
	// sampler interval; explicitly settable for tests.
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

// WithReplica attaches a read replica pool and the lag tolerance the
// router will enforce before routing any read to it. Pass nil
// replica to clear a previously-configured replica (useful for tests
// flipping between configurations); pass a non-positive tolerance to
// effectively disable routing to the replica without un-wiring it.
//
// Returns the receiver so this can be chained from NewPoolRouter.
func (r *PoolRouter) WithReplica(replica *pgxpool.Pool, lagTolerance time.Duration) *PoolRouter {
	r.replica = replica
	r.lagTolerance = lagTolerance
	if r.sampleStaleness <= 0 {
		// Default to 30s — generous enough that a slow sampler tick
		// doesn't immediately disqualify the replica, but tight
		// enough that a fully stalled sampler is detected within
		// one Prometheus scrape cycle (15s default).
		r.sampleStaleness = 30 * time.Second
	}
	return r
}

// WithSampleStaleness overrides the "how old can the last sample be"
// bound used by Read(). Primarily for tests; production wiring
// should leave this at the default (30s).
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

func (r *PoolRouter) runLagSampler(ctx context.Context, interval time.Duration) {
	// Take a first sample immediately so Read() can flip to the
	// replica without waiting for the first tick. The initial
	// sample is bounded by the parent ctx so a stuck DB doesn't
	// block process shutdown.
	_, _ = r.SampleLag(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sampleCtx, cancel := context.WithTimeout(ctx, interval)
			_, _ = r.SampleLag(sampleCtx)
			cancel()
		}
	}
}
