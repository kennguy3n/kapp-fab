// Package platform — leader election via PostgreSQL session-scope advisory
// locks.
//
// Rationale: Phase 3 unblocks multi-replica worker deployments. Several
// worker sub-loops (event outbox drain, scheduler, export queue, autoscaler)
// MUST run from exactly one replica at a time — running them on N replicas
// either produces duplicates (scheduler), corrupts shared state (autoscaler
// decisions racing), or wastes work (overlapping outbox drains). Leader
// election guarantees the single-runner property while still letting
// operators run N>1 worker pods for HA.
//
// Why Postgres advisory locks (not Redis SET NX, not etcd, not Kubernetes
// leases):
//
//   1. Zero new infrastructure. The worker already has authenticated
//      Postgres access and we want HA worker without bolting on a second
//      coordination service.
//   2. Session-scope advisory locks release automatically on connection
//      death. A crashed leader (OOM kill, kernel panic, network partition
//      lasting longer than tcp_keepalive_time) causes Postgres to tear
//      down its session and release the lock, so a peer can take over
//      within seconds without an external arbiter.
//   3. pg_try_advisory_lock is non-blocking and the loser sees the
//      contention immediately, so failover is fast and we never block
//      a worker thread on lock acquisition.
//
// What this does NOT provide:
//
//   - Fencing tokens. There is a brief window after a network partition
//      heals where two replicas might both believe they are leader (the
//      old leader's TCP session times out asynchronously; the new leader
//      already acquired the lock). All leader-only writes must therefore
//      be idempotent or guarded by a per-row optimistic lock at the
//      application layer. The outbox drain is already idempotent (FOR
//      UPDATE SKIP LOCKED + delivered_at write inside the same tx).
//   - Sub-second failover. The dead-leader detection time is bounded by
//      TCP keepalive (~75s default on Linux) plus our heartbeat interval.
//      We make this fast by setting a short heartbeat (15s default) and
//      relying on the heartbeat's Conn.Ping to surface a dead socket
//      faster than the OS would. Even so, a partitioned leader can
//      believe it is leader for up to one heartbeat interval after the
//      partition starts.
package platform

import (
	"context"
	"hash/fnv"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// LeaderElection coordinates a single leader among N workers using
// Postgres session-scope advisory locks. The dedicated *pgx.Conn (NOT a
// pgxpool slot) holds the lock; on connection death (crash, network
// partition, DB restart) Postgres tears down the session and releases
// the lock, so a peer can take over within one poll interval.
//
// Construction is via NewLeaderElection; the zero value is unusable.
type LeaderElection struct {
	connString string
	lockKey    int64
	identity   string

	// pollInterval is how often a non-leader retries acquisition. Short
	// enough to keep failover responsive (default 5s = upper bound on
	// the gap between the old leader losing the lock and a new leader
	// claiming it).
	pollInterval time.Duration

	// heartbeat is how often the active leader pings its conn to
	// detect a silently dead socket. Lower bound for "how long can a
	// partitioned leader believe it is leader". 15s default.
	heartbeat time.Duration
}

// NewLeaderElection derives a deterministic int64 lock key from the
// supplied namespace string and constructs a new elector. Two electors
// configured with the same namespace contest the SAME advisory lock and
// therefore the same singleton role; distinct namespaces (e.g.
// "outbox-drain" vs "export-queue") allow independent leaders.
//
// identity is a free-form label for log lines (typically hostname or
// Kubernetes pod name) so an operator scanning logs from N replicas can
// tell which one currently holds the lock.
func NewLeaderElection(connString, namespace, identity string) *LeaderElection {
	h := fnv.New64a()
	_, _ = h.Write([]byte("kapp:leader:" + namespace))
	// int64 conversion is a deliberate truncation: pg_advisory_lock
	// takes a bigint and we just need a deterministic, collision-
	// resistant value within that domain.
	lockKey := int64(h.Sum64())
	return &LeaderElection{
		connString:   connString,
		lockKey:      lockKey,
		identity:     identity,
		pollInterval: 5 * time.Second,
		heartbeat:    15 * time.Second,
	}
}

// WithPollInterval overrides the default acquisition retry cadence.
// Shorter = faster failover but more LOCK attempts under steady-state
// contention.
func (le *LeaderElection) WithPollInterval(d time.Duration) *LeaderElection {
	if d > 0 {
		le.pollInterval = d
	}
	return le
}

// WithHeartbeat overrides the default leader-side conn health-check
// cadence. Shorter = faster detection of a silently-dead leader socket
// but more SELECT 1 round-trips.
func (le *LeaderElection) WithHeartbeat(d time.Duration) *LeaderElection {
	if d > 0 {
		le.heartbeat = d
	}
	return le
}

// LockKey returns the int64 advisory-lock key for this election. Exposed
// for tests and for operators who want to inspect pg_locks directly.
func (le *LeaderElection) LockKey() int64 { return le.lockKey }

// Run blocks until ctx is cancelled, repeatedly attempting to acquire
// the advisory lock. While holding the lock it invokes leaderFn with a
// derived context that is cancelled the moment leadership is lost
// (heartbeat failure, ctx cancellation, leaderFn return). On heartbeat
// failure or leaderFn return, the conn is closed (releasing the lock),
// then Run loops back to retry acquisition unless ctx is cancelled.
//
// Contract for leaderFn:
//   - MUST honour the supplied ctx. On ctx cancellation the function
//     MUST return promptly so the conn can be released.
//   - SHOULD be idempotent or guarded by per-row locks at writes,
//     because the failover window admits a brief two-leader overlap
//     as described in the package doc.
//   - A returned error is logged but does NOT propagate out of Run —
//     Run only exits when the outer ctx is cancelled.
func (le *LeaderElection) Run(ctx context.Context, leaderFn func(ctx context.Context) error) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		acquired, conn := le.acquire(ctx)
		if !acquired {
			// Either ctx cancelled or no lock available; in either
			// case acquire has already slept the pollInterval (or
			// observed ctx cancel).
			if ctx.Err() != nil {
				return nil
			}
			continue
		}

		// Lead until heartbeat fails, leaderFn returns, or ctx
		// cancels.
		le.lead(ctx, conn, leaderFn)
		_ = conn.Close(context.Background())
		log.Printf("leader[%s]: released (lockKey=%d)", le.identity, le.lockKey)
	}
}

// acquire dials a dedicated *pgx.Conn and attempts to acquire the
// advisory lock. On failure to dial / failure to lock, it sleeps the
// pollInterval (cancellation-aware) and returns false.
func (le *LeaderElection) acquire(ctx context.Context) (bool, *pgx.Conn) {
	conn, err := pgx.Connect(ctx, le.connString)
	if err != nil {
		log.Printf("leader[%s]: dial: %v; retrying in %s", le.identity, err, le.pollInterval)
		_ = sleepCtx(ctx, le.pollInterval)
		return false, nil
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", le.lockKey).Scan(&got); err != nil {
		_ = conn.Close(context.Background())
		log.Printf("leader[%s]: lock attempt: %v; retrying in %s", le.identity, err, le.pollInterval)
		_ = sleepCtx(ctx, le.pollInterval)
		return false, nil
	}
	if !got {
		_ = conn.Close(context.Background())
		_ = sleepCtx(ctx, le.pollInterval)
		return false, nil
	}
	log.Printf("leader[%s]: acquired (lockKey=%d)", le.identity, le.lockKey)
	return true, conn
}

// lead runs leaderFn in a goroutine while holding the lock. It returns
// when (a) ctx cancels, (b) the heartbeat ping fails (meaning the
// conn — and the lock with it — is gone), or (c) leaderFn returns on
// its own. In all cases the returned function has stopped and any
// goroutines it spawned have unwound through the sub-context cancel.
func (le *LeaderElection) lead(ctx context.Context, conn *pgx.Conn, leaderFn func(ctx context.Context) error) {
	leaderCtx, leaderCancel := context.WithCancel(ctx)
	defer leaderCancel()

	done := make(chan error, 1)
	go func() {
		done <- leaderFn(leaderCtx)
	}()

	ticker := time.NewTicker(le.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			leaderCancel()
			<-done // wait for leaderFn to unwind
			return
		case err := <-done:
			if err != nil {
				log.Printf("leader[%s]: leader fn exited with error: %v", le.identity, err)
			}
			return
		case <-ticker.C:
			// Ping the dedicated conn. If it's dead the OS may not
			// have surfaced the failure yet (TCP keepalive ~75s on
			// Linux). Ping forces a roundtrip; on error we drop
			// leadership eagerly so a peer can take over.
			pingCtx, cancel := context.WithTimeout(ctx, le.heartbeat)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				log.Printf("leader[%s]: heartbeat failed: %v; stepping down", le.identity, err)
				leaderCancel()
				<-done
				return
			}
		}
	}
}

// sleepCtx waits for d unless ctx is cancelled first. Returns true if
// the full duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// keyForNamespace exposes the deterministic hash → lock-key derivation
// for tests and operator tooling without requiring the caller to also
// construct a LeaderElection.
func keyForNamespace(namespace string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("kapp:leader:" + namespace))
	return int64(h.Sum64())
}
