package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// ComponentStatus is the coarse health classification reported for a
// single dependency and, after aggregation, for the platform as a
// whole. The three values are deliberately the only states the
// public status page renders so operators and tenants share one
// vocabulary.
type ComponentStatus string

const (
	// StatusOperational means the dependency answered within its
	// probe budget and any associated lag is under the warn
	// threshold.
	StatusOperational ComponentStatus = "operational"
	// StatusDegraded means the dependency answered but a soft
	// threshold was crossed (e.g. replication or outbox lag), or a
	// non-critical dependency is unreachable. The platform still
	// serves traffic.
	StatusDegraded ComponentStatus = "degraded"
	// StatusDown means a critical dependency (PostgreSQL) is
	// unreachable; the platform cannot serve tenant requests.
	StatusDown ComponentStatus = "down"
)

// severity orders the three states so aggregation can take the
// worst-of without a switch ladder. Higher is worse.
func (s ComponentStatus) severity() int {
	switch s {
	case StatusDown:
		return 2
	case StatusDegraded:
		return 1
	default:
		return 0
	}
}

// ComponentHealth is the per-dependency probe result. Error is empty
// on success; Detail carries probe-specific numbers (replication lag
// seconds, undelivered event count, heartbeat age) that the admin
// surface renders and the public surface omits.
type ComponentHealth struct {
	Name      string          `json:"name"`
	Status    ComponentStatus `json:"status"`
	LatencyMS float64         `json:"latency_ms"`
	Error     string          `json:"error,omitempty"`
	Detail    map[string]any  `json:"detail,omitempty"`
}

// SystemHealth is the aggregate returned by HealthChecker.Check. The
// top-level Status is the worst component status; Components is
// sorted by name so callers and tests see deterministic output.
type SystemHealth struct {
	Status     ComponentStatus   `json:"status"`
	Components []ComponentHealth `json:"components"`
	CheckedAt  time.Time         `json:"checked_at"`
}

// HealthConfig configures a HealthChecker. Only Pool is required;
// the optional dependency probes are registered only when their
// connection string / endpoint is non-empty so a dev stack without
// Redis or NATS does not surface phantom "down" components.
type HealthConfig struct {
	// Pool is the tenant-scoped pool. Its Ping drives the
	// (critical) PostgreSQL probe and the replication-lag query;
	// both are RLS-independent so no tenant GUC is required.
	Pool *pgxpool.Pool

	// AdminPool is the BYPASSRLS pool used by the cross-tenant
	// probes (outbox backlog, worker heartbeat). When nil those two
	// probes are skipped because RLS on the tenant pool would scope
	// every count to zero and report a misleading "all clear".
	AdminPool *pgxpool.Pool

	// RedisURL / NATSURL / ZKFabricEndpoint opt their respective
	// connectivity probes in. Empty means "not configured for this
	// process" and the component is omitted from the report.
	RedisURL         string
	NATSURL          string
	ZKFabricEndpoint string

	// HTTPClient backs the ZK Object Fabric probe. Defaulted to a
	// client whose timeout matches ProbeTimeout when nil.
	HTTPClient *http.Client

	// ProbeTimeout bounds every individual probe so one stuck
	// dependency cannot hold the request open. Defaults to 2s.
	ProbeTimeout time.Duration

	// ReplicationLagWarn is the standby replay-lag above which the
	// PostgreSQL component reports degraded. Defaults to 10s. Only
	// meaningful when Pool points at (or fails over to) a standby;
	// on a primary the lag query returns NULL and is reported as
	// "not a standby".
	ReplicationLagWarn time.Duration

	// OutboxBacklogWarn / OutboxBacklogCritical are undelivered-event
	// counts. At/above warn the outbox component is degraded; at/above
	// critical it is down (non-critical, so it caps system health at
	// degraded). Defaults: 1000 / 10000.
	OutboxBacklogWarn     int64
	OutboxBacklogCritical int64

	// WorkerStaleWarn / WorkerStaleCritical bound the age of the most
	// recent scheduled-action execution. Older than warn → degraded,
	// older than critical → down (non-critical). Defaults: 5m / 15m.
	WorkerStaleWarn     time.Duration
	WorkerStaleCritical time.Duration

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// HealthChecker probes the platform's runtime dependencies and
// aggregates them into a SystemHealth. It holds no long-lived
// connections of its own beyond the supplied pgx pools: the Redis
// and NATS probes open and close a connection per check so a flaky
// dependency cannot leak sockets into a pool that the rest of the
// process never uses.
type HealthChecker struct {
	cfg HealthConfig
}

// probe is one named dependency check. critical=true means a Down
// result drags the whole system Down (only PostgreSQL); the rest cap
// the system at Degraded when they fail.
type probe struct {
	name     string
	critical bool
	run      func(ctx context.Context) ComponentHealth
}

// NewHealthChecker validates and defaults cfg, returning an error
// only when the mandatory Pool is missing. Optional probes are
// wired lazily inside Check based on which fields are populated.
func NewHealthChecker(cfg HealthConfig) (*HealthChecker, error) {
	if cfg.Pool == nil {
		return nil, errors.New("platform: health checker requires a non-nil Pool")
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 2 * time.Second
	}
	if cfg.ReplicationLagWarn <= 0 {
		cfg.ReplicationLagWarn = 10 * time.Second
	}
	if cfg.OutboxBacklogWarn <= 0 {
		cfg.OutboxBacklogWarn = 1000
	}
	if cfg.OutboxBacklogCritical <= 0 {
		cfg.OutboxBacklogCritical = 10000
	}
	if cfg.WorkerStaleWarn <= 0 {
		cfg.WorkerStaleWarn = 5 * time.Minute
	}
	if cfg.WorkerStaleCritical <= 0 {
		cfg.WorkerStaleCritical = 15 * time.Minute
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.ProbeTimeout}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &HealthChecker{cfg: cfg}, nil
}

// probes assembles the active probe set for this checker. PostgreSQL
// is always present; the rest are conditional on configuration so
// the component list reflects what is actually wired into the
// running process.
func (h *HealthChecker) probes() []probe {
	probes := []probe{
		{name: "postgres", critical: true, run: h.checkPostgres},
	}
	if h.cfg.RedisURL != "" {
		probes = append(probes, probe{name: "redis", run: h.checkRedis})
	}
	if h.cfg.NATSURL != "" {
		probes = append(probes, probe{name: "nats", run: h.checkNATS})
	}
	if h.cfg.ZKFabricEndpoint != "" {
		probes = append(probes, probe{name: "zk_object_fabric", run: h.checkZKFabric})
	}
	if h.cfg.AdminPool != nil {
		probes = append(probes,
			probe{name: "outbox", run: h.checkOutbox},
			probe{name: "worker", run: h.checkWorkerHeartbeat},
		)
	}
	return probes
}

// Check runs every active probe concurrently under its own timeout
// and folds the results into a SystemHealth. The whole call is
// bounded by ctx; each probe is additionally bounded by
// ProbeTimeout so a single unresponsive dependency cannot stall the
// others.
func (h *HealthChecker) Check(ctx context.Context) SystemHealth {
	probes := h.probes()
	results := make([]ComponentHealth, len(probes))
	critical := make([]bool, len(probes))

	var wg sync.WaitGroup
	wg.Add(len(probes))
	for i, p := range probes {
		go func(i int, p probe) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, h.cfg.ProbeTimeout)
			defer cancel()
			results[i] = p.run(pctx)
			critical[i] = p.critical
		}(i, p)
	}
	wg.Wait()

	overall := aggregateStatus(results, critical)
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return SystemHealth{
		Status:     overall,
		Components: results,
		CheckedAt:  h.cfg.now(),
	}
}

// aggregateStatus folds per-component results into the platform-wide
// status. The platform is only Down when a *critical* dependency is
// Down; a non-critical dependency being Down caps the system at
// Degraded, because the request path can still serve (just with a
// stalled outbox, a missing cache, etc.). critical[i] corresponds to
// results[i].
func aggregateStatus(results []ComponentHealth, critical []bool) ComponentStatus {
	overall := StatusOperational
	for i, c := range results {
		effective := c.Status
		if effective == StatusDown && i < len(critical) && !critical[i] {
			effective = StatusDegraded
		}
		if effective.severity() > overall.severity() {
			overall = effective
		}
	}
	return overall
}

// elapsedMS returns milliseconds since start as a rounded float so
// JSON output stays compact and tests are not at the mercy of
// sub-millisecond jitter.
func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}

// checkPostgres pings the pool and, on success, samples standby
// replay lag. Ping failure is the only Down condition in the whole
// system because PostgreSQL is the one dependency the request path
// cannot route around.
func (h *HealthChecker) checkPostgres(ctx context.Context) ComponentHealth {
	start := h.cfg.now()
	c := ComponentHealth{Name: "postgres", Status: StatusOperational}
	if err := h.cfg.Pool.Ping(ctx); err != nil {
		c.Status = StatusDown
		c.Error = err.Error()
		c.LatencyMS = elapsedMS(start)
		return c
	}
	c.LatencyMS = elapsedMS(start)

	// pg_last_xact_replay_timestamp() is non-NULL only on a standby
	// replaying WAL; on a primary it returns NULL, which we surface
	// as "not a standby" rather than an error.
	var lagSeconds *float64
	err := h.cfg.Pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))`,
	).Scan(&lagSeconds)
	if err != nil {
		// A failed lag sample does not flip the component to Down —
		// connectivity already succeeded. Record it for operators.
		c.Detail = map[string]any{"replication_lag_error": err.Error()}
		return c
	}
	if lagSeconds == nil {
		c.Detail = map[string]any{"replica": false}
		return c
	}
	c.Detail = map[string]any{"replica": true, "replication_lag_seconds": *lagSeconds}
	if *lagSeconds >= h.cfg.ReplicationLagWarn.Seconds() {
		c.Status = StatusDegraded
	}
	return c
}

// checkRedis opens a short-lived client, PINGs, and closes it. A
// per-probe client avoids coupling health to the rate-limiter's pool
// and guarantees a stuck Redis cannot accumulate idle sockets.
func (h *HealthChecker) checkRedis(ctx context.Context) ComponentHealth {
	start := h.cfg.now()
	c := ComponentHealth{Name: "redis", Status: StatusOperational}
	opts, err := redis.ParseURL(h.cfg.RedisURL)
	if err != nil {
		c.Status = StatusDown
		c.Error = fmt.Sprintf("parse redis url: %v", err)
		c.LatencyMS = elapsedMS(start)
		return c
	}
	client := redis.NewClient(opts)
	defer func() { _ = client.Close() }()
	if err := client.Ping(ctx).Err(); err != nil {
		c.Status = StatusDown
		c.Error = err.Error()
	}
	c.LatencyMS = elapsedMS(start)
	return c
}

// checkNATS dials NATS with a timeout derived from the probe budget
// and immediately drains the connection. nats.Connect retries by
// default, which would blow the probe budget, so retries are
// disabled and the dial timeout is clamped to the probe timeout.
func (h *HealthChecker) checkNATS(ctx context.Context) ComponentHealth {
	start := h.cfg.now()
	c := ComponentHealth{Name: "nats", Status: StatusOperational}

	// nats.Connect has no context parameter; it bounds itself only by
	// nats.Timeout. Run it off-goroutine and race it against ctx so a
	// caller-side cancellation (or the per-probe deadline firing
	// before the dial timeout) returns promptly. The result channel
	// is buffered so the dial goroutine never blocks even after we
	// have returned, and a late-arriving connection is still closed.
	type dialResult struct {
		nc  *nats.Conn
		err error
	}
	done := make(chan dialResult, 1)
	go func() {
		nc, err := nats.Connect(h.cfg.NATSURL,
			nats.Name("kapp-health"),
			nats.Timeout(h.cfg.ProbeTimeout),
			nats.RetryOnFailedConnect(false),
			nats.MaxReconnects(0),
		)
		done <- dialResult{nc: nc, err: err}
	}()

	select {
	case <-ctx.Done():
		go func() {
			if r := <-done; r.nc != nil {
				r.nc.Close()
			}
		}()
		c.Status = StatusDown
		c.Error = ctx.Err().Error()
	case r := <-done:
		if r.err != nil {
			c.Status = StatusDown
			c.Error = r.err.Error()
		} else {
			defer r.nc.Close()
			if status := r.nc.Status(); status != nats.CONNECTED {
				c.Status = StatusDown
				c.Error = fmt.Sprintf("connection status %v", status)
			}
		}
	}
	c.LatencyMS = elapsedMS(start)
	return c
}

// checkZKFabric issues a bounded GET against the fabric console. Any
// HTTP response — even 401/404 — proves the console is reachable, so
// only a transport error (refused connection, DNS, timeout) is
// treated as Down. The probe deliberately hits the bare endpoint and
// does not send the admin token: connectivity, not authorization, is
// what we are measuring.
func (h *HealthChecker) checkZKFabric(ctx context.Context) ComponentHealth {
	start := h.cfg.now()
	c := ComponentHealth{Name: "zk_object_fabric", Status: StatusOperational}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.ZKFabricEndpoint, http.NoBody)
	if err != nil {
		c.Status = StatusDown
		c.Error = err.Error()
		c.LatencyMS = elapsedMS(start)
		return c
	}
	resp, err := h.cfg.HTTPClient.Do(req)
	if err != nil {
		c.Status = StatusDown
		c.Error = err.Error()
		c.LatencyMS = elapsedMS(start)
		return c
	}
	defer func() { _ = resp.Body.Close() }()
	c.Detail = map[string]any{"http_status": resp.StatusCode}
	c.LatencyMS = elapsedMS(start)
	return c
}

// checkOutbox counts undelivered rows in the partitioned events
// outbox and the age of the oldest one. It runs on the BYPASSRLS
// admin pool so the count spans every tenant; on the tenant pool the
// RLS policy would scope it to zero. A large backlog signals the
// worker's drain loop has stalled.
func (h *HealthChecker) checkOutbox(ctx context.Context) ComponentHealth {
	start := h.cfg.now()
	c := ComponentHealth{Name: "outbox", Status: StatusOperational}
	var (
		backlog      int64
		oldestAgeSec float64
	)
	err := h.cfg.AdminPool.QueryRow(ctx,
		`SELECT COUNT(*),
		        COALESCE(EXTRACT(EPOCH FROM (now() - MIN(created_at))), 0)
		   FROM events
		  WHERE delivered_at IS NULL`,
	).Scan(&backlog, &oldestAgeSec)
	if err != nil {
		c.Status = StatusDown
		c.Error = err.Error()
		c.LatencyMS = elapsedMS(start)
		return c
	}
	c.Detail = map[string]any{
		"undelivered_events":       backlog,
		"oldest_event_age_seconds": oldestAgeSec,
	}
	switch {
	case backlog >= h.cfg.OutboxBacklogCritical:
		c.Status = StatusDown
	case backlog >= h.cfg.OutboxBacklogWarn:
		c.Status = StatusDegraded
	}
	c.LatencyMS = elapsedMS(start)
	return c
}

// checkWorkerHeartbeat reports the age of the most recent
// scheduled-action execution across all tenants. A worker that has
// stopped polling leaves last_run_at frozen, so a stale max is the
// cheapest available liveness signal without a dedicated heartbeat
// table. NULL (no action has ever run) is reported as operational
// with no age, since it cannot distinguish a dead worker from an
// empty schedule.
func (h *HealthChecker) checkWorkerHeartbeat(ctx context.Context) ComponentHealth {
	start := h.cfg.now()
	c := ComponentHealth{Name: "worker", Status: StatusOperational}
	var lastRun *time.Time
	err := h.cfg.AdminPool.QueryRow(ctx,
		`SELECT MAX(last_run_at) FROM scheduled_actions`,
	).Scan(&lastRun)
	if err != nil {
		c.Status = StatusDown
		c.Error = err.Error()
		c.LatencyMS = elapsedMS(start)
		return c
	}
	if lastRun == nil {
		c.Detail = map[string]any{"last_run_at": nil}
		c.LatencyMS = elapsedMS(start)
		return c
	}
	age := h.cfg.now().Sub(*lastRun)
	c.Detail = map[string]any{
		"last_run_at":           lastRun.UTC(),
		"heartbeat_age_seconds": age.Seconds(),
	}
	switch {
	case age >= h.cfg.WorkerStaleCritical:
		c.Status = StatusDown
	case age >= h.cfg.WorkerStaleWarn:
		c.Status = StatusDegraded
	}
	c.LatencyMS = elapsedMS(start)
	return c
}

// TenantHealth is the tenant-scoped health snapshot served at
// /api/v1/tenants/me/health. Unlike SystemHealth it carries no
// infrastructure detail — only the facts a tenant operator needs:
// how close they are to their quota, which features are live, when
// their data was last exported (the closest proxy for "backup"), and
// how fresh their record data is.
type TenantHealth struct {
	TenantID      uuid.UUID          `json:"tenant_id"`
	Status        ComponentStatus    `json:"status"`
	QuotaUsage    []TenantQuotaUsage `json:"quota_usage"`
	Features      map[string]bool    `json:"features"`
	LastBackupAt  *time.Time         `json:"last_backup_at"`
	DataFreshness *time.Time         `json:"data_freshness"`
	CheckedAt     time.Time          `json:"checked_at"`
}

// TenantQuotaUsage pairs a metered counter with its plan limit and a
// derived percentage so the frontend renders a usage bar without a
// second round-trip.
type TenantQuotaUsage struct {
	Metric  string  `json:"metric"`
	Used    int64   `json:"used"`
	Limit   int64   `json:"limit"`
	Percent float64 `json:"percent"`
}

// quotaMetrics is the canonical metered counter set, in display
// order. Mirrors the zero-fill list in the metering handler so the
// health page and the usage page agree on which bars exist.
var quotaMetrics = []string{"api_calls", "storage_bytes", "krecord_count", "user_seats"}

// TenantHealth computes a tenant's health snapshot inside a single
// read-only, tenant-scoped transaction. Every query runs under
// `SET LOCAL app.tenant_id` (via dbutil) so RLS — not handler
// discipline — guarantees the caller can only observe its own rows.
// planLimits supplies the tenant's plan ceilings (already resolved
// by the caller from plan_definitions); a nil/empty map renders the
// bars with a zero limit (unlimited / unknown) rather than failing.
func (h *HealthChecker) TenantHealth(ctx context.Context, tenantID uuid.UUID, planLimits map[string]int64) (TenantHealth, error) {
	out := TenantHealth{
		TenantID:  tenantID,
		Status:    StatusOperational,
		Features:  map[string]bool{},
		CheckedAt: h.cfg.now(),
	}
	if tenantID == uuid.Nil {
		return out, errors.New("platform: tenant health requires a tenant id")
	}

	periodStart := time.Date(h.cfg.now().UTC().Year(), h.cfg.now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)

	err := dbutil.WithReadOnlyTenantTxOnPool(ctx, h.cfg.Pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		usage := map[string]int64{}
		rows, err := tx.Query(ctx,
			`SELECT metric, value FROM tenant_usage WHERE period_start = $1`,
			periodStart,
		)
		if err != nil {
			return fmt.Errorf("query tenant usage: %w", err)
		}
		for rows.Next() {
			var metric string
			var value int64
			if err := rows.Scan(&metric, &value); err != nil {
				rows.Close()
				return fmt.Errorf("scan tenant usage: %w", err)
			}
			usage[metric] = value
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate tenant usage: %w", err)
		}
		for _, m := range quotaMetrics {
			used := usage[m]
			limit := planLimits[m]
			pct := 0.0
			if limit > 0 {
				pct = float64(used) / float64(limit) * 100
			}
			out.QuotaUsage = append(out.QuotaUsage, TenantQuotaUsage{
				Metric:  m,
				Used:    used,
				Limit:   limit,
				Percent: pct,
			})
		}

		featRows, err := tx.Query(ctx, `SELECT feature_key, enabled FROM tenant_features`)
		if err != nil {
			return fmt.Errorf("query tenant features: %w", err)
		}
		for featRows.Next() {
			var key string
			var enabled bool
			if err := featRows.Scan(&key, &enabled); err != nil {
				featRows.Close()
				return fmt.Errorf("scan tenant feature: %w", err)
			}
			out.Features[key] = enabled
		}
		featRows.Close()
		if err := featRows.Err(); err != nil {
			return fmt.Errorf("iterate tenant features: %w", err)
		}

		// Last completed export is the closest user-facing proxy
		// for "last backup" — the export queue is how a tenant pulls
		// a full data dump out of the platform.
		if err := tx.QueryRow(ctx,
			`SELECT MAX(completed_at) FROM export_jobs WHERE status = 'completed'`,
		).Scan(&out.LastBackupAt); err != nil {
			return fmt.Errorf("query last export: %w", err)
		}

		// Data freshness = most recent record write. A tenant whose
		// newest record is months old is "stale" in the data sense
		// even though every dependency is operational.
		if err := tx.QueryRow(ctx,
			`SELECT MAX(updated_at) FROM krecords WHERE deleted_at IS NULL`,
		).Scan(&out.DataFreshness); err != nil {
			return fmt.Errorf("query data freshness: %w", err)
		}
		return nil
	})
	if err != nil {
		return TenantHealth{}, err
	}

	// A tenant at or over any quota ceiling is degraded — requests
	// will start getting rejected by the quota middleware.
	for _, q := range out.QuotaUsage {
		if q.Limit > 0 && q.Used >= q.Limit {
			out.Status = StatusDegraded
			break
		}
	}
	return out, nil
}
