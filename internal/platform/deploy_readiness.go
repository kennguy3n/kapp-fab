package platform

import (
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
)

// ReadinessProbe is the load-balancer-facing readiness signal used by
// the zero-downtime deploy flow (scripts/deploy.sh, Workstream 7). It
// is deliberately distinct from the existing /healthz liveness probe:
//
//   - /healthz answers "is this process alive and can it reach the
//     database" — a failing liveness check tells the orchestrator to
//     restart the pod.
//   - the readiness probe answers "should the LB route NEW traffic to
//     this instance right now" — a failing readiness check tells the
//     LB to drain in-flight connections and stop sending new ones,
//     WITHOUT restarting the process.
//
// During schema migration application the process is perfectly alive
// (so /healthz stays 200) but must be drained so no request observes a
// half-applied schema. ReadinessProbe returns 503 while a migration is
// in progress and 200 otherwise, giving the LB a clean drain signal.
//
// Two independent signals can flip the probe to "not ready", combined
// with OR:
//
//  1. An in-process flag (BeginMigration/EndMigration). Used when the
//     migration runs inside the same process as the probe.
//  2. An on-disk sentinel file. Used for the common case where the
//     migrate CLI (cmd/migrate) is a SEPARATE process from the API
//     server that serves the probe: deploy.sh (or `migrate apply
//     --readiness-sentinel`) creates the sentinel before applying
//     migrations and removes it afterwards, and every API replica's
//     probe observes the shared file on a mounted volume.
//
// The zero value is NOT usable; construct with NewReadinessProbe.
type ReadinessProbe struct {
	migrating atomic.Bool
	// sentinelPath, when non-empty, makes the probe report "not ready"
	// whenever a file exists at this path. Empty disables the
	// file-based signal and leaves only the in-process flag.
	sentinelPath string
	// statFn is os.Stat in production; overridable in tests so they do
	// not have to touch the real filesystem.
	statFn func(string) (os.FileInfo, error)
}

// NewReadinessProbe constructs a probe. sentinelPath may be empty to
// rely solely on the in-process BeginMigration/EndMigration flag; pass
// a path (e.g. /var/run/kapp/migrating) to additionally drain whenever
// that file exists, which is how a separate migrate process signals an
// in-flight migration to the API replicas.
func NewReadinessProbe(sentinelPath string) *ReadinessProbe {
	return &ReadinessProbe{
		sentinelPath: sentinelPath,
		statFn:       os.Stat,
	}
}

// BeginMigration marks the process as draining (probe reports 503).
// Safe for concurrent use.
func (p *ReadinessProbe) BeginMigration() { p.migrating.Store(true) }

// EndMigration clears the draining flag (probe reports 200 again,
// unless the sentinel file still forces 503). Safe for concurrent use.
func (p *ReadinessProbe) EndMigration() { p.migrating.Store(false) }

// Ready reports whether the instance should receive new traffic. When
// not ready it also returns a short human-readable reason, surfaced in
// the probe's JSON body so operators can tell WHY a node is draining.
//
// Sentinel semantics: only a successful stat (the file exists) forces
// "not ready". Any stat error — including a transient permission or
// I/O error, not just os.ErrNotExist — is treated as "sentinel
// absent" so a filesystem hiccup can never strand a node in a
// permanently-draining state (which would be an outage). The sentinel
// is created and removed by the deploy tooling, so a missing file is
// the steady-state "ready" condition.
func (p *ReadinessProbe) Ready() (ready bool, reason string) {
	if p.migrating.Load() {
		return false, "migration in progress"
	}
	if p.sentinelPath != "" {
		if _, err := p.statFn(p.sentinelPath); err == nil {
			return false, "migration in progress (deploy sentinel present)"
		}
	}
	return true, ""
}

// ServeHTTP implements http.Handler. It emits the same
// `{"status": ...}` JSON envelope shape the rest of the API surface
// uses: `{"status":"ready"}` with 200, or
// `{"status":"draining","reason":...}` with 503.
func (p *ReadinessProbe) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	ready, reason := p.Ready()
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "draining",
			"reason": reason,
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}
