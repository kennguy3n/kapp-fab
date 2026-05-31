package platform

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// RegisterReplicaLagGauge wires Prometheus gauges + a counter that
// surface the replica router's runtime state for alerting. Three
// series are exported:
//
//   - kapp_replica_lag_seconds — the lag (seconds) of the most
//     recent sample. Always >= 0. Zero means "primary == replica
//     in WAL position OR no replica is configured" — the
//     associated kapp_replica_configured gauge distinguishes these.
//   - kapp_replica_configured — 1 when a replica is wired, 0
//     otherwise. Exists so alerts can be conditional ("lag > 5s AND
//     configured == 1") and skip the all-zero case where no replica
//     is registered.
//   - kapp_replica_sample_errors_total — monotonic counter of
//     SampleLag errors observed by the background sampler. An
//     operator can alert on a sustained climb ("errors > 0 in
//     5m") to distinguish "replica unreachable" (counter climbing
//     fast, lag gauge stale) from "lag spiked above tolerance"
//     (counter flat, lag gauge high). Without this counter the two
//     failure modes look identical at the lag-gauge level (the
//     router's read path falls back to primary in both cases).
//
// The function returns a stop func that cancels the background
// publisher goroutine; deps_build wires it into the standard
// cleanups slice so the goroutine exits cleanly during a graceful
// shutdown.
//
// publishInterval is independent of the sampler interval: the
// sampler is what queries the replica, this loop simply copies the
// cached observation into the gauge / counter. Default 5s matches
// the recommended sample cadence; passing zero disables the
// publisher entirely (use this in tests where you'd rather call
// PublishOnce manually).
func RegisterReplicaLagGauge(reg *MetricsRegistry, router *dbutil.PoolRouter, publishInterval time.Duration) (stop func()) {
	if reg == nil || router == nil {
		// No metrics or no router → no-op. Return a stop that
		// does nothing so callers don't need a nil-check.
		return func() {}
	}

	lagGauge := reg.Gauge(
		"kapp_replica_lag_seconds",
		"Replication lag of the configured read replica at the most recent sample (0 when no replica is wired).",
	)
	configuredGauge := reg.Gauge(
		"kapp_replica_configured",
		"1 when a read replica is wired into the router, 0 otherwise. Use to condition alerts on lag.",
	)
	sampleErrorCounter := reg.Counter(
		"kapp_replica_sample_errors_total",
		"Cumulative count of SampleLag errors from the background lag sampler. Climbs when the replica is unreachable; flat when only lag is high.",
	)

	// lastErrCount tracks the most recently published error count
	// so we can publish the delta into the (monotonic) Prometheus
	// counter. The router's LastErrorCount is also monotonic, but
	// publishOnce runs on a separate goroutine that can lag the
	// sampler, so we must accumulate the delta rather than Set on
	// the counter (counters don't support Set).
	//
	// Stored as atomic.Uint64 even though today there is exactly
	// one writer at any instant (the initial synchronous
	// publishOnce below runs to completion before the ticker
	// goroutine starts, and the goroutine is the only caller after
	// that): the atomic makes the goroutine-safety property
	// load-bearing on the type system, not on call-site ordering
	// that a future maintainer adding a second invoker (e.g. an
	// on-demand /metrics scrape hook, or a unit-test fixture
	// running publishOnce alongside the ticker) could silently
	// break.
	var lastErrCount atomic.Uint64

	// configured gauge can be set once at registration — the wiring
	// does not change at runtime.
	if router.HasReplica() {
		configuredGauge.Set(1)
	} else {
		configuredGauge.Set(0)
	}

	publishOnce := func() {
		lag, _, ok := router.LastLag()
		if ok {
			lagGauge.Set(lag.Seconds())
		} else {
			lagGauge.Set(0)
		}
		cur := router.LastErrorCount()
		// Atomic CAS loop so concurrent publishers never
		// double-count (each delta is emitted exactly once,
		// against the lastErrCount snapshot we read at the top of
		// this iteration). Without the CAS, two concurrent
		// publishers reading lastErrCount=10 with router count=12
		// would each Add(2), producing a counter of 14 instead of
		// the true 12.
		for {
			prev := lastErrCount.Load()
			if cur <= prev {
				return
			}
			if lastErrCount.CompareAndSwap(prev, cur) {
				sampleErrorCounter.Add(cur - prev)
				return
			}
		}
	}

	// Emit an initial value so the gauge isn't "missing" in a fresh
	// scrape before the first tick (Prometheus treats missing
	// series differently from explicit zero).
	publishOnce()

	if publishInterval <= 0 || !router.HasReplica() {
		// No replica → no point running the publisher loop; the
		// configured-gauge and zero lag are already set.
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(publishInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publishOnce()
			}
		}
	}()
	return cancel
}
