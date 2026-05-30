package platform

import (
	"context"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// RegisterReplicaLagGauge wires a Prometheus gauge that publishes the
// most recent replica lag observation from the supplied router. Two
// gauges are exported:
//
//   - kapp_replica_lag_seconds — the lag (seconds) of the most
//     recent sample. Always >= 0. Zero means "primary == replica
//     in WAL position OR no replica is configured" — the
//     associated kapp_replica_configured gauge distinguishes these.
//   - kapp_replica_configured — 1 when a replica is wired, 0
//     otherwise. Exists so alerts can be conditional ("lag > 5s AND
//     configured == 1") and skip the all-zero case where no replica
//     is registered.
//
// The function returns a stop func that cancels the background
// publisher goroutine; deps_build wires it into the standard
// cleanups slice so the goroutine exits cleanly during a graceful
// shutdown.
//
// publishInterval is independent of the sampler interval: the
// sampler is what queries the replica, this loop simply copies the
// cached observation into the gauge. Default 5s matches the
// recommended sample cadence; passing zero disables the publisher
// entirely (use this in tests where you'd rather call PublishOnce
// manually).
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

	// configured gauge can be set once at registration — the wiring
	// does not change at runtime.
	if router.HasReplica() {
		configuredGauge.Set(1)
	} else {
		configuredGauge.Set(0)
	}

	publishOnce := func() {
		lag, _, ok := router.LastLag()
		if !ok {
			lagGauge.Set(0)
			return
		}
		lagGauge.Set(lag.Seconds())
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
