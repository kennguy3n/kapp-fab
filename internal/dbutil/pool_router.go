package dbutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

	// sampleErrorCount is a monotonically-increasing count of
	// SampleLag errors observed by the background sampler. The
	// metrics layer reads this via LastErrorCount and publishes
	// the delta to kapp_replica_sample_errors_total, so an
	// operator can distinguish "replica unreachable" (counter
	// climbing fast) from "lag spiked above tolerance" (counter
	// flat, kapp_replica_lag_seconds high). The router itself does
	// not log or surface metrics — it stays a pure routing primitive
	// — it just exposes the counter through the accessor.
	sampleErrorCount atomic.Uint64

	// samplerMu guards samplerCancel / samplerDone. The fields are
	// only ever read or written from StartLagSampler and Close,
	// both of which are boot-time-only operations (see WithReplica
	// docstring), so the mutex is essentially uncontended — it
	// exists to make the "have we started a sampler?" check race-
	// free with respect to a concurrent Close from a shutdown
	// goroutine.
	samplerMu     sync.Mutex
	samplerCancel context.CancelFunc
	samplerDone   chan struct{}
	closeOnce     sync.Once
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
// the observed lag, or a negative sentinel when the replica is in
// recovery but has not yet replayed any WAL.
//
// The query asks Postgres three things in one round-trip so the
// caller can distinguish three modes without a second probe:
//
//	SELECT pg_is_in_recovery(),
//	       pg_last_xact_replay_timestamp(),
//	       EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))
//
//	1. in_recovery=FALSE: pool is a primary (or dev case where
//	   DB_URL == KAPP_READ_REPLICA_URL — single instance). A primary
//	   has zero lag relative to itself, so lag=0 and the router
//	   happily routes to it.
//
//	2. in_recovery=TRUE, replay_ts=NULL: pool is a freshly-attached
//	   standby that has not yet replayed any WAL. Its visible state
//	   is only as fresh as the base backup, which can be hours old.
//	   Reporting lag=0 here would route point reads (Get/ListPage/
//	   BulkFetch/Search) to a not-yet-caught-up replica until the
//	   first WAL apply lands. SampleLag instead stores a -1 ns lag
//	   sentinel so Read()'s existing `lag < 0` gate (see line ~213)
//	   falls back to primary. The sentinel is replaced by the real
//	   lag as soon as replay_ts becomes non-NULL on a subsequent
//	   sample. Note: keyset walks have a defense-in-depth gate of
//	   their own — record.snapshotVia returns errReplicaNotReady on
//	   the same condition — so they already fall back to primary
//	   regardless of what SampleLag reports.
//
//	3. in_recovery=TRUE, replay_ts set: normal lagging standby.
//	   lag = now() - replay_ts.
//
// SampleLag is safe to call from a single dedicated goroutine
// (typically StartLagSampler below); concurrent callers are
// supported via the atomic updates but produce no extra value.
func (r *PoolRouter) SampleLag(ctx context.Context) (time.Duration, error) {
	if r == nil || r.replica == nil {
		return 0, errors.New("dbutil: PoolRouter: no replica configured")
	}
	var (
		inRecovery bool
		replayTS   *time.Time
		lagSeconds *float64
	)
	err := r.replica.QueryRow(ctx,
		`SELECT
		    pg_is_in_recovery(),
		    pg_last_xact_replay_timestamp(),
		    EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))`,
	).Scan(&inRecovery, &replayTS, &lagSeconds)
	if err != nil {
		return 0, fmt.Errorf("dbutil: PoolRouter sample lag: %w", err)
	}

	var lag time.Duration
	switch {
	case inRecovery && replayTS == nil:
		// Standby has not yet replayed any WAL — store -1 ns so
		// Read() routes to primary via the lag<0 gate.
		lag = -1
	case lagSeconds == nil:
		// Defensive: in_recovery=FALSE with a NULL extract result
		// means we're on a primary (or a single-instance dev case).
		// Lag is zero relative to itself.
		lag = 0
	default:
		secs := *lagSeconds
		if secs < 0 {
			// Clock skew between primary and standby: replay_ts is
			// in the future relative to the standby's now(). Treat
			// as zero rather than a negative sentinel.
			secs = 0
		}
		lag = time.Duration(secs * float64(time.Second))
	}
	r.lastLagNanos.Store(int64(lag))
	r.lastSampledAt.Store(time.Now().UTC().UnixNano())
	return lag, nil
}

// LastLag returns the most recently observed lag and the wall clock
// time it was observed. Used by the /metrics gauge. The returned
// duration is ALWAYS >= 0 — the router's internal not-yet-replayed
// sentinel (a negative lastLagNanos value, see SampleLag) is folded
// into ok=false here so it never leaks out into a public metric.
//
// Returns (0, time.Time{}, false) when:
//
//   - no sample has been recorded yet (sampler hasn't ticked, or the
//     router has no replica configured), OR
//
//   - the most recent sample observed a standby that has not yet
//     replayed any WAL (lastLagNanos<0). Read() consults
//     lastLagNanos directly via its own atomic load and routes to
//     primary via the lag<0 gate (see Read at line ~213); the
//     external observable contract is "no usable sample", which is
//     exactly what callers of LastLag need.
//
// Keeping the sentinel scoped to the internal lastLagNanos field
// means a future change to how "not yet replayed" is represented
// (e.g. a separate atomic flag) does not have to also rewrite the
// public LastLag / metric contract.
func (r *PoolRouter) LastLag() (time.Duration, time.Time, bool) {
	if r == nil {
		return 0, time.Time{}, false
	}
	at := r.lastSampledAt.Load()
	if at == 0 {
		return 0, time.Time{}, false
	}
	lag := time.Duration(r.lastLagNanos.Load())
	if lag < 0 {
		// Standby has sampled but not yet replayed any WAL. Treat
		// as "no usable sample" externally; Read() still observes
		// the sentinel via its direct atomic load.
		return 0, time.Time{}, false
	}
	return lag, time.Unix(0, at), true
}

// StartLagSampler launches a background goroutine that calls
// SampleLag every interval. The goroutine runs until either the
// supplied ctx is cancelled or Close() is called — whichever
// happens first. Close() additionally waits for the goroutine to
// fully exit, so the caller can safely Close() the underlying
// replica pool immediately after Close() returns without racing an
// in-flight SampleLag query.
//
// Errors during sampling are NOT propagated (the router has no
// logger dependency) but each error increments LastErrorCount(),
// which the metrics layer publishes as
// kapp_replica_sample_errors_total — operators can alert on a
// sustained climb to distinguish "replica unreachable" from "lag
// spiked above tolerance". The cached sample timestamp is NOT
// advanced on error, so a sustained sampling failure also shows up
// as "sample too old" in Read() and routes back to primary.
//
// Returns true when the sampler was started, false when it was
// not. False return cases (each is benign — no replica means no
// sampler needed, non-positive interval means "disable sampling"):
//
//   - nil receiver
//   - no replica configured (the no-op single-pool case)
//   - interval <= 0 (treat as explicit "do not sample" — callers
//     who pass 0 typically also expect Read() to refuse the
//     replica because the staleness check will fire on the first
//     call; a non-positive interval is the documented opt-out
//     mirror of `lagTolerance <= 0`. The wiring helper logs a
//     boot warning when this combination is configured with a
//     non-empty KAPP_READ_REPLICA_URL so it isn't silent)
//   - a sampler was already started on this router (StartLagSampler
//     is boot-time-only; the second call is a no-op rather than
//     leaking a second goroutine)
//
// The sampler runs against an internal context derived from the
// supplied ctx — cancelling the supplied ctx OR calling Close()
// stops the goroutine. Close() additionally blocks on the
// goroutine's exit so resource cleanup ordering (sampler exits
// before pool Close()) can be enforced LIFO in the service
// entrypoint's cleanups stack.
func (r *PoolRouter) StartLagSampler(ctx context.Context, interval time.Duration) bool {
	if r == nil || r.replica == nil || interval <= 0 {
		return false
	}
	r.samplerMu.Lock()
	defer r.samplerMu.Unlock()
	if r.samplerDone != nil {
		// A sampler is already running; do not start a second one.
		// The boot-time-only contract means this should never
		// trigger in production wiring, but the guard keeps the
		// router safe under a misconfigured test or future caller.
		return false
	}
	samplerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.samplerCancel = cancel
	r.samplerDone = done
	go func() {
		defer close(done)
		r.runLagSampler(samplerCtx, interval)
	}()
	return true
}

// Close cancels the lag sampler goroutine and waits for it to fully
// exit. Safe to call multiple times (idempotent); safe to call when
// no sampler was started (no-op).
//
// Wire Close() into the service shutdown stack so it runs BEFORE
// the replica pool's Close(): if the pool closes while the sampler
// has an in-flight SampleLag query, pgx returns an error from the
// connection acquire (which is harmless and just bumps the error
// counter) but on some pool states the close races with the
// connection release and surfaces a panic. The router's Close()
// waits for the goroutine, eliminating the window entirely.
//
// Cleanups in service entrypoints use a LIFO slice, so the correct
// append order is:
//
//	cleanups = append(cleanups, func() { replicaPool.Close() })
//	// ... wire router, start sampler ...
//	cleanups = append(cleanups, func() { dbRouter.Close() })  // runs FIRST on shutdown
//	cleanups = append(cleanups, stopReplicaGauge)             // runs FIRST-FIRST
//
// LIFO unwinds: stopReplicaGauge → dbRouter.Close (sampler joins)
// → replicaPool.Close. Without this Close() the sampler goroutine
// is only bound to the outer ctx, which the caller typically
// cancels AFTER cleanup() returns — racing the pool close.
func (r *PoolRouter) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.samplerMu.Lock()
		cancel, done := r.samplerCancel, r.samplerDone
		r.samplerMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
	})
}

// LastErrorCount returns the monotonic count of SampleLag errors
// observed by the background sampler since process start. Used by
// the metrics layer to publish kapp_replica_sample_errors_total
// (by polling and tracking the delta between observations).
//
// Safe on a nil receiver; returns 0 when no sampler has been
// started or no errors have occurred.
func (r *PoolRouter) LastErrorCount() uint64 {
	if r == nil {
		return 0
	}
	return r.sampleErrorCount.Load()
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
	r.sampleOnce(ctx, interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sampleOnce(ctx, interval)
		}
	}
}

// sampleOnce performs a single sampling probe with the bounded
// per-tick timeout and books the result. Errors are not propagated
// (the router has no logger and the metric publication path runs
// out-of-band via LastErrorCount); they bump sampleErrorCount so
// the metrics layer can publish kapp_replica_sample_errors_total.
// Ctx.Done() during the probe is NOT counted as an error — it just
// means shutdown is in progress and the sampler is about to exit
// anyway, so counting it would generate a spurious bump on every
// graceful shutdown.
func (r *PoolRouter) sampleOnce(parent context.Context, interval time.Duration) {
	sampleCtx, cancel := context.WithTimeout(parent, sampleQueryTimeout(interval))
	_, err := r.SampleLag(sampleCtx)
	cancel()
	if err == nil {
		return
	}
	if parent.Err() != nil {
		// Parent ctx is done — either ctx.Canceled cascading from
		// the shutdown signal, or a per-tick deadline that fired
		// right as shutdown began. Either way the sampler is about
		// to exit on the next select; counting the error would
		// generate a spurious bump on every graceful shutdown and
		// pollute kapp_replica_sample_errors_total with false
		// positives. Real sampling failures (replica unreachable,
		// slow query) still bump the counter because parent.Err()
		// is nil in those cases.
		return
	}
	r.sampleErrorCount.Add(1)
}
