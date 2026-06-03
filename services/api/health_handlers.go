package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// healthHandlerTimeout bounds an entire health request — the
// concurrent probes (each already capped at HealthConfig.ProbeTimeout)
// plus the handler's own DB reads. The public /api/v1/health route is
// mounted outside the router's 30s timeout group (mirroring /healthz),
// so without this ceiling the incident/aggregate queries would inherit
// an unbounded request context. 5s comfortably exceeds the ~2s probe
// budget and a fast indexed read while still guaranteeing the endpoint
// can never hang on a stuck dependency.
const healthHandlerTimeout = 5 * time.Second

// healthHandlers backs Workstream 6's health surface: a public
// component-status endpoint, an admin-only operator dashboard feed,
// and a tenant-scoped self-health endpoint. The checker probes
// shared infrastructure; the handlers layer auth-appropriate
// projections on top of it (public callers never see internal error
// strings or per-dependency detail).
type healthHandlers struct {
	checker   *platform.HealthChecker
	plans     *tenant.PlanStore
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
}

// newHealthHandlers builds the checker from the dependency bag and
// returns the handler set. ZK fabric connectivity is probed against
// the same console endpoint the setup wizard provisions against
// (ZK_FABRIC_CONSOLE_ENDPOINT); Redis/NATS come from the loaded
// config so the probe set mirrors what the process is actually
// wired to talk to.
func newHealthHandlers(d *apiDeps, zkFabricEndpoint string) (*healthHandlers, error) {
	checker, err := platform.NewHealthChecker(platform.HealthConfig{
		Pool:             d.pool,
		AdminPool:        d.adminPool,
		RedisURL:         d.cfg.RedisURL,
		NATSURL:          d.cfg.EventBusURL,
		ZKFabricEndpoint: zkFabricEndpoint,
	})
	if err != nil {
		return nil, err
	}
	return &healthHandlers{
		checker:   checker,
		plans:     d.meth.plans,
		pool:      d.pool,
		adminPool: d.adminPool,
	}, nil
}

// publicComponent is the sanitized per-component view served to
// unauthenticated callers: status + latency only. Internal error
// strings and probe detail (hostnames, lag numbers, backlog counts)
// stay behind the admin endpoint so a public scrape cannot
// fingerprint the deployment's topology. Name is a generic functional
// label (see publicComponentName), never the raw probe name.
type publicComponent struct {
	Name      string                   `json:"name"`
	Status    platform.ComponentStatus `json:"status"`
	LatencyMS float64                  `json:"latency_ms"`
}

// publicComponentNames maps internal probe names to generic,
// technology-agnostic labels for the unauthenticated surface. The raw
// probe names (postgres, redis, nats, zk_object_fabric, …) name the
// underlying tech stack, so emitting them verbatim would let a public
// scrape fingerprint the deployment — exactly what the publicComponent
// sanitization claims to prevent. The public endpoint reports these
// functional labels instead; the admin endpoint keeps the raw names
// for operators.
var publicComponentNames = map[string]string{
	"postgres":         "database",
	"redis":            "cache",
	"nats":             "event_bus",
	"zk_object_fabric": "object_storage",
	"outbox":           "event_delivery",
	"worker":           "background_jobs",
}

// publicComponentName returns the generic label for an internal probe
// name. Any probe missing from the map collapses to the constant
// "service" rather than falling through to its raw name, so a
// future probe can never accidentally leak its technology on the
// public surface — it just shows up as an unnamed service until the
// mapping is extended.
func publicComponentName(internal string) string {
	if label, ok := publicComponentNames[internal]; ok {
		return label
	}
	return "service"
}

// publicIncident is a deliberately lossy projection of a
// platform_scale_events row: only the kind of capacity change and
// when it happened. The raw reason (which embeds tenant counts and
// thresholds) and the cell id are dropped so the public feed cannot
// leak fleet-capacity internals.
type publicIncident struct {
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

type publicHealthResponse struct {
	Status platform.ComponentStatus `json:"status"`
	// ComponentAvailabilityPercent is the share of probed
	// components currently operational. It is an instantaneous
	// reading, not a historical SLA — named explicitly so the UI
	// does not imply a 90-day uptime number the platform does not
	// retain.
	ComponentAvailabilityPercent float64           `json:"component_availability_percent"`
	Components                   []publicComponent `json:"components"`
	Incidents                    []publicIncident  `json:"incidents"`
	CheckedAt                    time.Time         `json:"checked_at"`
}

// publicHealth serves GET /api/v1/health. No auth, no tenant
// context: it reports the platform's component health, an
// instantaneous availability percentage, and a sanitized capacity-
// change history.
func (h *healthHandlers) publicHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthHandlerTimeout)
	defer cancel()

	sys := h.checker.Check(ctx)

	comps := make([]publicComponent, 0, len(sys.Components))
	operational := 0
	for _, c := range sys.Components {
		if c.Status == platform.StatusOperational {
			operational++
		}
		comps = append(comps, publicComponent{
			Name:      publicComponentName(c.Name),
			Status:    c.Status,
			LatencyMS: c.LatencyMS,
		})
	}
	availability := 100.0
	if len(comps) > 0 {
		availability = float64(operational) / float64(len(comps)) * 100
	}

	writeJSON(w, http.StatusOK, publicHealthResponse{
		Status:                       sys.Status,
		ComponentAvailabilityPercent: availability,
		Components:                   comps,
		Incidents:                    h.recentIncidents(ctx),
		CheckedAt:                    sys.CheckedAt,
	})
}

// recentIncidents reads the last few real capacity changes
// (scale_up / scale_down, never "hold") from platform_scale_events
// and maps them to public-safe summaries. platform_scale_events is a
// control-plane table with no RLS, so the tenant pool may read it
// without a GUC. Read failures collapse to an empty list — the
// incident feed is best-effort decoration, not a hard dependency of
// the status endpoint.
func (h *healthHandlers) recentIncidents(ctx context.Context) []publicIncident {
	incidents := []publicIncident{}
	rows, err := h.pool.Query(ctx,
		`SELECT event_type, created_at
		   FROM platform_scale_events
		  WHERE event_type IN ('scale_up', 'scale_down')
		  ORDER BY created_at DESC
		  LIMIT 10`,
	)
	if err != nil {
		return incidents
	}
	defer rows.Close()
	for rows.Next() {
		var eventType string
		var at time.Time
		if err := rows.Scan(&eventType, &at); err != nil {
			return incidents
		}
		summary := "Capacity adjusted"
		switch eventType {
		case "scale_up":
			summary = "Platform capacity increased to absorb load"
		case "scale_down":
			summary = "Platform capacity reduced after load subsided"
		}
		incidents = append(incidents, publicIncident{Summary: summary, At: at})
	}
	return incidents
}

// cellHealth is the per-cell operator view used by the admin
// dashboard. UtilizationPercent is tenant_count / max_tenants so the
// frontend can render a saturation bar without re-deriving it.
type cellHealth struct {
	ID                string  `json:"id"`
	Region            string  `json:"region"`
	MaxTenants        int     `json:"max_tenants"`
	TenantCount       int     `json:"tenant_count"`
	CPUPct            float32 `json:"cpu_pct"`
	MemPct            float32 `json:"mem_pct"`
	ConnSaturationPct float32 `json:"conn_saturation_pct"`
	UtilizationPct    float64 `json:"utilization_pct"`
}

// poolHealth surfaces the pgx pool's live connection accounting so
// operators can spot saturation before it turns into request
// queueing. SaturationPercent is total/max — the headroom the pool
// has left.
type poolHealth struct {
	MaxConns          int32   `json:"max_conns"`
	TotalConns        int32   `json:"total_conns"`
	AcquiredConns     int32   `json:"acquired_conns"`
	IdleConns         int32   `json:"idle_conns"`
	SaturationPercent float64 `json:"saturation_percent"`
}

// topTenant is one row of the "noisiest tenants" leaderboard, ranked
// by API calls in the current metering period.
type topTenant struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	APICalls int64  `json:"api_calls"`
}

type adminHealthResponse struct {
	System     platform.SystemHealth `json:"system"`
	Cells      []cellHealth          `json:"cells"`
	Pool       poolHealth            `json:"pool"`
	TopTenants []topTenant           `json:"top_tenants"`
}

// adminHealthDetailed serves GET /api/v1/admin/health/detailed. It is
// mounted behind adminChain (JWT + IsPlatformAdmin) and returns the
// full, unredacted component detail plus the cross-tenant operator
// aggregates the dashboard renders. Cross-tenant reads use adminPool
// (BYPASSRLS); when it is not configured the aggregates are returned
// empty rather than failing the whole request.
func (h *healthHandlers) adminHealthDetailed(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthHandlerTimeout)
	defer cancel()

	resp := adminHealthResponse{
		System:     h.checker.Check(ctx),
		Cells:      []cellHealth{},
		TopTenants: []topTenant{},
		Pool:       poolStats(h.pool),
	}
	if h.adminPool != nil {
		resp.Cells = h.cellHealth(ctx)
		resp.TopTenants = h.topTenants(ctx)
	}
	writeJSON(w, http.StatusOK, resp)
}

// poolStats snapshots the pool's connection accounting. A nil pool
// (defensive — the API always has one) yields a zero-value reading.
func poolStats(pool *pgxpool.Pool) poolHealth {
	if pool == nil {
		return poolHealth{}
	}
	s := pool.Stat()
	ph := poolHealth{
		MaxConns:      s.MaxConns(),
		TotalConns:    s.TotalConns(),
		AcquiredConns: s.AcquiredConns(),
		IdleConns:     s.IdleConns(),
	}
	if s.MaxConns() > 0 {
		ph.SaturationPercent = float64(s.TotalConns()) / float64(s.MaxConns()) * 100
	}
	return ph
}

// cellHealth joins the cells registry with a live tenant count per
// cell. Tenants with a NULL cell_id are bucketed under the implicit
// 'default' cell, matching the autoscaler's convention. Read errors
// degrade to an empty slice so a transient control-plane hiccup does
// not blank the entire dashboard.
func (h *healthHandlers) cellHealth(ctx context.Context) []cellHealth {
	out := []cellHealth{}
	rows, err := h.adminPool.Query(ctx,
		`SELECT c.id, c.region, c.max_tenants, c.cpu_pct, c.mem_pct,
		        c.conn_saturation_pct,
		        COALESCE(t.cnt, 0) AS tenant_count
		   FROM cells c
		   LEFT JOIN (
		        SELECT COALESCE(cell_id, 'default') AS cell_id, COUNT(*) AS cnt
		          FROM tenants
		         GROUP BY COALESCE(cell_id, 'default')
		   ) t ON t.cell_id = c.id
		  ORDER BY c.id`,
	)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c cellHealth
		if err := rows.Scan(&c.ID, &c.Region, &c.MaxTenants, &c.CPUPct,
			&c.MemPct, &c.ConnSaturationPct, &c.TenantCount); err != nil {
			return out
		}
		if c.MaxTenants > 0 {
			c.UtilizationPct = float64(c.TenantCount) / float64(c.MaxTenants) * 100
		}
		out = append(out, c)
	}
	return out
}

// topTenants ranks tenants by API-call volume in the current metering
// period. Runs on adminPool because tenant_usage is RLS-scoped; the
// join to tenants resolves a human-readable name for the dashboard.
func (h *healthHandlers) topTenants(ctx context.Context) []topTenant {
	out := []topTenant{}
	// Sample the clock once: reading time.Now() separately for Year
	// and Month could straddle a month/year rollover and compute a
	// period_start in the wrong month.
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := h.adminPool.Query(ctx,
		`SELECT u.tenant_id, COALESCE(t.name, ''), u.value
		   FROM tenant_usage u
		   LEFT JOIN tenants t ON t.id = u.tenant_id
		  WHERE u.metric = 'api_calls' AND u.period_start = $1
		  ORDER BY u.value DESC
		  LIMIT 10`,
		periodStart,
	)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t topTenant
		if err := rows.Scan(&t.TenantID, &t.Name, &t.APICalls); err != nil {
			return out
		}
		out = append(out, t)
	}
	return out
}

// tenantHealth serves GET /api/v1/tenants/me/health. It is mounted
// behind tenantChain so the tenant is taken from JWT claims, never a
// request header. The handler resolves the tenant's plan ceilings,
// then delegates to the checker which runs every read under the
// tenant's RLS context.
func (h *healthHandlers) tenantHealth(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthHandlerTimeout)
	defer cancel()

	limits := map[string]int64{}
	if h.plans != nil {
		if plan, err := h.plans.Get(ctx, t.Plan); err == nil {
			limits = map[string]int64{
				"api_calls":     plan.Limits.APICalls,
				"storage_bytes": plan.Limits.StorageBytes,
				"krecord_count": plan.Limits.KRecordCount,
				"user_seats":    plan.Limits.UserSeats,
			}
		} else if !errors.Is(err, tenant.ErrPlanNotFound) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	th, err := h.checker.TenantHealth(ctx, t.ID, limits)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, th)
}
