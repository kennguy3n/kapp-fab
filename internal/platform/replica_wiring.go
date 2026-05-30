package platform

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// WireReplicaRouter opens the optional read-replica pool described
// by cfg, builds a PoolRouter that routes reads to it (with the
// configured lag tolerance), starts the background lag sampler, and
// registers the lag/error metrics. It returns:
//
//   - the PoolRouter (always non-nil; falls back to a single-pool
//     router around primary when cfg.ReadReplicaURL is unset, so the
//     caller can wire it into reads unconditionally)
//
//   - a single Stop closure the caller invokes (typically via
//     `defer stop()`) to tear everything down in the only safe order.
//     The closure internally runs:
//
//     1. stopGauge          — unsubscribe the /metrics publisher first
//     so it does not observe a half-torn router
//     2. router.Close()     — cancel the sampler goroutine and BLOCK
//     on its exit (LIFO with respect to start)
//     3. replicaPool.Close()— only AFTER the sampler is fully drained,
//     so an in-flight SampleLag query cannot race with
//     pgx's connection-release path on pool close
//
//     Returning a single closure (instead of a []func() slice the
//     caller was previously expected to consume LIFO) makes the
//     ordering inherent to the API. Callers cannot accidentally
//     iterate the slice forward and tear the replica pool down
//     before the sampler goroutine exits.
//
//   - an error if opening the replica pool fails. When no replica
//     URL is configured the closure still runs (to unsubscribe the
//     "configured=0" gauge that gets registered so alerts have a
//     stable series on bootstrap) — it is always safe to call.
//
// service is a short tag used as the log prefix (e.g. "api",
// "worker") so boot-time messages from each entrypoint are
// distinguishable in operator logs.
//
// All five service entrypoints (api/worker/importer/kchat-bridge/
// agent-tools) previously inlined this wiring with subtly different
// log prefixes and (in some cases) missing cleanup order. Extracting
// the helper closes the duplication AND fixes the sampler-vs-pool
// race in one place rather than five.
func WireReplicaRouter(
	ctx context.Context,
	service string,
	cfg *Config,
	primary *pgxpool.Pool,
	metrics *MetricsRegistry,
) (*dbutil.PoolRouter, func(), error) {
	router := dbutil.NewPoolRouter(primary)

	if cfg.ReadReplicaURL == "" {
		// Still wire the configured/lag gauge so /metrics surfaces
		// "no replica configured" as an explicit zero rather than a
		// missing series — alerts conditioned on
		// kapp_replica_configured won't misfire on bootstrap. The
		// returned cleanup stops the gauge publisher (no-op when no
		// replica means no sampler is running anyway, but still
		// idiomatic — cleanup pairs with registration).
		stopGauge := RegisterReplicaLagGauge(metrics, router, 0)
		return router, stopGauge, nil
	}

	replicaPool, err := NewPoolWithSize(
		ctx, cfg.ReadReplicaURL, cfg.ReadReplicaMaxConns, cfg.ReadReplicaMinConns,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: open replica pool: %w", service, err)
	}

	router = router.WithReplica(replicaPool, cfg.ReadReplicaLagTolerance, cfg.ReadReplicaLagSampleInterval)
	started := router.StartLagSampler(ctx, cfg.ReadReplicaLagSampleInterval)
	stopGauge := RegisterReplicaLagGauge(metrics, router, cfg.ReadReplicaLagSampleInterval)

	// Single Stop closure with the only safe shutdown order baked in.
	// Previously this was returned as `[]func(){pool.Close, router.Close,
	// stopGauge}` and callers were expected to consume LIFO. Two of the
	// five entrypoints did `for _, fn := range cleanups { defer fn() }`
	// which only walks LIFO under Go 1.22+ loop-var semantics, and a
	// caller iterating the slice forward would tear the replica pool
	// down before the sampler goroutine exited — racing the in-flight
	// SampleLag connection release path. Baking the order into a single
	// closure makes that misuse unrepresentable. See PoolRouter.Close()
	// docstring for the full rationale.
	stop := func() {
		stopGauge()
		router.Close()
		replicaPool.Close()
	}

	switch {
	case !started && cfg.ReadReplicaLagSampleInterval <= 0:
		// Documented opt-out: KAPP_READ_REPLICA_URL is set but
		// _LAG_SAMPLE_INTERVAL=0. Routing will fall back to
		// primary on every Read() because the staleness check
		// will fire (lastSampledAt never advances), so the
		// replica connection is wired but unused. Log a WARN so
		// the operator sees the silent fallback rather than
		// debugging a "why isn't my replica getting traffic?"
		// mystery from the metrics alone.
		log.Printf(
			"%s: WARN read replica configured (KAPP_READ_REPLICA_URL set) but lag sampler is disabled (KAPP_READ_REPLICA_LAG_SAMPLE_INTERVAL=%s); all reads will route to primary. Set a positive interval (default 5s) to enable replica routing.",
			service, cfg.ReadReplicaLagSampleInterval,
		)
	case !started:
		// Defensive: StartLagSampler returned false for some
		// other reason (e.g. a second call). This branch should
		// never trigger from this helper because we only call
		// StartLagSampler once, but it's cheap insurance against
		// a future caller wiring two routers.
		log.Printf(
			"%s: WARN read replica configured but lag sampler did not start; routing will fall back to primary.",
			service,
		)
	default:
		log.Printf(
			"%s: read replica enabled (lag tolerance=%s, sample every=%s)",
			service, cfg.ReadReplicaLagTolerance, cfg.ReadReplicaLagSampleInterval,
		)
	}

	return router, stop, nil
}
