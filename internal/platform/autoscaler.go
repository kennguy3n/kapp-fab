package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cell autoscaling — Phase G.
//
// Cells are independent control-plane shards each hosting a bounded
// number of tenants. The autoscaler runs as a periodic platform-level
// loop in the worker (it is NOT a per-tenant scheduled_actions row;
// scheduled_actions are tenant-scoped by design and a cell straddles
// every tenant on it). On each tick it:
//
//  1. Walks the `cells` table for every cell's last observed CPU /
//     memory / connection-pool saturation reading. An external
//     collector populates these columns; the autoscaler is purely a
//     consumer of that signal.
//  2. Counts the active tenants per cell via the `tenants.cell_id`
//     column.
//  3. Applies the configured Policy thresholds to each cell's
//     snapshot and produces a Decision (scale_up, scale_down, hold).
//  4. Writes the decision into platform_scale_events for audit and
//     emits a structured slog line so the cell-router or a human
//     operator can act on it.
//
// Events are deliberately not pushed onto the tenant outbox in this
// iteration — the outbox is RLS-bound to a tenant id, and a cell
// event has no tenant. The platform_scale_events table fills the
// same role at the control-plane scope. A NATS subject can be added
// alongside it later without changing the policy logic.

// CellEventScaleUp is the event_type the autoscaler writes when a
// cell should grow (provision capacity, add another replica, or
// rebalance some tenants away).
const CellEventScaleUp = "scale_up"

// CellEventScaleDown is the event_type the autoscaler writes when a
// cell can shrink (return capacity to the pool).
const CellEventScaleDown = "scale_down"

// CellEventHold is the event_type the autoscaler writes when no
// action is required. We persist holds so the operator can confirm
// the loop is running even on quiet days.
const CellEventHold = "hold"

// AutoscalePolicy captures the configurable thresholds the engine
// applies to each cell. Defaults are conservative: the loop is a
// soft observer until an operator dials it in.
type AutoscalePolicy struct {
	// MaxTenantsPerCell triggers scale_up when current tenant count
	// reaches this value. 0 disables the per-tenant fence.
	MaxTenantsPerCell int
	// CPUThreshold is the percent above which the cell should
	// scale up. 0 disables.
	CPUThreshold float32
	// MemoryThreshold is the percent above which the cell should
	// scale up. 0 disables.
	MemoryThreshold float32
	// ConnectionPoolSaturation is the percent above which the cell
	// should scale up. 0 disables.
	ConnectionPoolSaturation float32
	// ScaleDownTenantsRatio: when current tenants are below this
	// fraction of MaxTenantsPerCell AND every utilisation metric
	// is below half of its threshold, emit scale_down. Bounded
	// 0..1; 0 disables scale-down.
	ScaleDownTenantsRatio float32
	// MinHoldBetweenScales is the minimum interval between two
	// non-hold decisions on the same cell. Prevents flapping when
	// a cell hovers around a threshold. Defaults to 10 minutes
	// when unset.
	MinHoldBetweenScales time.Duration
}

// DefaultAutoscalePolicy returns the policy the worker uses unless
// overridden via configuration. Picked to mirror the SLO targets
// documented in docs/PHASE_G_ACCEPTANCE.md.
func DefaultAutoscalePolicy() AutoscalePolicy {
	return AutoscalePolicy{
		MaxTenantsPerCell:        1000,
		CPUThreshold:             80,
		MemoryThreshold:          80,
		ConnectionPoolSaturation: 75,
		ScaleDownTenantsRatio:    0.30,
		MinHoldBetweenScales:     10 * time.Minute,
	}
}

// CellSnapshot is the joined view of a cell's row plus its current
// tenant count. The engine takes one of these per cell on every
// tick.
type CellSnapshot struct {
	ID                 string    `json:"id"`
	Region             string    `json:"region"`
	MaxTenants         int       `json:"max_tenants"`
	CPUPct             float32   `json:"cpu_pct"`
	MemoryPct          float32   `json:"memory_pct"`
	ConnSaturationPct  float32   `json:"conn_saturation_pct"`
	ObservedAt         time.Time `json:"observed_at"`
	TenantCount        int       `json:"tenant_count"`
	LastScaleEventAt   time.Time `json:"last_scale_event_at,omitempty"`
	LastScaleEventType string    `json:"last_scale_event_type,omitempty"`
}

// Decision is what Decide returns for a single cell snapshot. The
// engine persists every decision; the operator (or cell-router)
// only needs to act on EventType != CellEventHold.
type Decision struct {
	CellID    string       `json:"cell_id"`
	EventType string       `json:"event_type"`
	Reason    string       `json:"reason"`
	Snapshot  CellSnapshot `json:"snapshot"`
}

// Decide applies the policy to a cell snapshot and returns the
// chosen decision. Pure function: no I/O, deterministic, easy to
// unit-test.
func Decide(s CellSnapshot, p AutoscalePolicy) Decision {
	d := Decision{CellID: s.ID, Snapshot: s, EventType: CellEventHold, Reason: "within thresholds"}
	// Cooldown — if the last non-hold decision on this cell was
	// within MinHoldBetweenScales, refuse to flip again until the
	// window closes. Prevents a slow oscillation around a
	// threshold from generating a torrent of scale events.
	hold := p.MinHoldBetweenScales
	if hold == 0 {
		hold = 10 * time.Minute
	}
	cooling := !s.LastScaleEventAt.IsZero() &&
		s.LastScaleEventType != CellEventHold &&
		time.Since(s.LastScaleEventAt) < hold
	// Scale-up checks (any one trips the action).
	if p.MaxTenantsPerCell > 0 && s.TenantCount >= p.MaxTenantsPerCell {
		if cooling {
			d.Reason = fmt.Sprintf("scale_up blocked by cooldown (tenants %d >= max %d)", s.TenantCount, p.MaxTenantsPerCell)
			return d
		}
		d.EventType = CellEventScaleUp
		d.Reason = fmt.Sprintf("tenants %d >= max %d", s.TenantCount, p.MaxTenantsPerCell)
		return d
	}
	if p.CPUThreshold > 0 && s.CPUPct >= p.CPUThreshold {
		if cooling {
			d.Reason = fmt.Sprintf("scale_up blocked by cooldown (cpu %.1f%% >= %.1f%%)", s.CPUPct, p.CPUThreshold)
			return d
		}
		d.EventType = CellEventScaleUp
		d.Reason = fmt.Sprintf("cpu %.1f%% >= %.1f%%", s.CPUPct, p.CPUThreshold)
		return d
	}
	if p.MemoryThreshold > 0 && s.MemoryPct >= p.MemoryThreshold {
		if cooling {
			d.Reason = fmt.Sprintf("scale_up blocked by cooldown (mem %.1f%% >= %.1f%%)", s.MemoryPct, p.MemoryThreshold)
			return d
		}
		d.EventType = CellEventScaleUp
		d.Reason = fmt.Sprintf("mem %.1f%% >= %.1f%%", s.MemoryPct, p.MemoryThreshold)
		return d
	}
	if p.ConnectionPoolSaturation > 0 && s.ConnSaturationPct >= p.ConnectionPoolSaturation {
		if cooling {
			d.Reason = fmt.Sprintf("scale_up blocked by cooldown (conn %.1f%% >= %.1f%%)", s.ConnSaturationPct, p.ConnectionPoolSaturation)
			return d
		}
		d.EventType = CellEventScaleUp
		d.Reason = fmt.Sprintf("conn %.1f%% >= %.1f%%", s.ConnSaturationPct, p.ConnectionPoolSaturation)
		return d
	}
	// Scale-down: very few tenants AND comfortable utilisation on
	// every metric. A single hot metric blocks the scale-down.
	if p.ScaleDownTenantsRatio > 0 && p.MaxTenantsPerCell > 0 {
		tenantFloor := float32(p.MaxTenantsPerCell) * p.ScaleDownTenantsRatio
		// Half of each scale-up threshold is the comfort target.
		cpuOK := p.CPUThreshold == 0 || s.CPUPct < p.CPUThreshold/2
		memOK := p.MemoryThreshold == 0 || s.MemoryPct < p.MemoryThreshold/2
		connOK := p.ConnectionPoolSaturation == 0 || s.ConnSaturationPct < p.ConnectionPoolSaturation/2
		if float32(s.TenantCount) < tenantFloor && cpuOK && memOK && connOK {
			if cooling {
				d.Reason = "scale_down blocked by cooldown"
				return d
			}
			d.EventType = CellEventScaleDown
			d.Reason = fmt.Sprintf("tenants %d < %.0f and utilisation comfortable", s.TenantCount, tenantFloor)
			return d
		}
	}
	return d
}

// AutoscaleEngine wires the policy engine to the database. The
// worker constructs one and calls Evaluate from a periodic ticker.
type AutoscaleEngine struct {
	pool   *pgxpool.Pool
	policy AutoscalePolicy
	logger *slog.Logger
}

// NewAutoscaleEngine binds a policy to a pool. Pass nil logger to
// fall back to slog.Default.
func NewAutoscaleEngine(pool *pgxpool.Pool, policy AutoscalePolicy, logger *slog.Logger) *AutoscaleEngine {
	if logger == nil {
		logger = slog.Default()
	}
	return &AutoscaleEngine{pool: pool, policy: policy, logger: logger}
}

// Evaluate snapshots every cell, applies the policy, persists each
// decision into platform_scale_events, and returns the decisions to
// the caller (handy for tests).
func (e *AutoscaleEngine) Evaluate(ctx context.Context) ([]Decision, error) {
	if e == nil || e.pool == nil {
		return nil, errors.New("platform: autoscaler not configured")
	}
	snapshots, err := e.snapshotCells(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Decision, 0, len(snapshots))
	for _, s := range snapshots {
		d := Decide(s, e.policy)
		out = append(out, d)
		if err := e.persistDecision(ctx, d); err != nil {
			// Persisting one decision must not block the rest of
			// the cells. Log and continue.
			e.logger.Error("autoscale: persist decision",
				"cell_id", d.CellID, "event_type", d.EventType, "err", err)
			continue
		}
		switch d.EventType {
		case CellEventScaleUp, CellEventScaleDown:
			e.logger.Info("autoscale: scale event",
				"cell_id", d.CellID, "event_type", d.EventType,
				"reason", d.Reason, "tenants", d.Snapshot.TenantCount,
				"cpu", d.Snapshot.CPUPct, "mem", d.Snapshot.MemoryPct,
				"conn", d.Snapshot.ConnSaturationPct)
		default:
			e.logger.Debug("autoscale: hold",
				"cell_id", d.CellID, "reason", d.Reason)
		}
	}
	return out, nil
}

// snapshotCells reads every cell's last observed metrics plus the
// current count of tenants assigned to it (NULL cell_id is bucketed
// onto 'default' so legacy tenants are accounted for).
func (e *AutoscaleEngine) snapshotCells(ctx context.Context) ([]CellSnapshot, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT c.id, c.region, c.max_tenants, c.cpu_pct, c.mem_pct,
		        c.conn_saturation_pct, c.observed_at,
		        COALESCE((
		            SELECT COUNT(*) FROM tenants t
		            WHERE COALESCE(t.cell_id, 'default') = c.id
		        ), 0)::int AS tenant_count,
		        COALESCE(last_event.created_at, 'epoch'::timestamptz) AS last_event_at,
		        COALESCE(last_event.event_type, '')               AS last_event_type
		   FROM cells c
		   LEFT JOIN LATERAL (
		       SELECT created_at, event_type
		         FROM platform_scale_events e
		        WHERE e.cell_id = c.id
		          AND e.event_type <> 'hold'
		        ORDER BY e.created_at DESC
		        LIMIT 1
		   ) last_event ON TRUE
		  ORDER BY c.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("autoscale: query cells: %w", err)
	}
	defer rows.Close()
	out := make([]CellSnapshot, 0, 8)
	for rows.Next() {
		var s CellSnapshot
		if err := rows.Scan(
			&s.ID, &s.Region, &s.MaxTenants, &s.CPUPct, &s.MemoryPct,
			&s.ConnSaturationPct, &s.ObservedAt, &s.TenantCount,
			&s.LastScaleEventAt, &s.LastScaleEventType,
		); err != nil {
			return nil, fmt.Errorf("autoscale: scan cell: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// persistDecision inserts a row into platform_scale_events. Every
// decision (including hold) is written so an operator can confirm
// the loop is alive without tailing logs.
func (e *AutoscaleEngine) persistDecision(ctx context.Context, d Decision) error {
	payload, err := json.Marshal(d.Snapshot)
	if err != nil {
		return fmt.Errorf("autoscale: marshal snapshot: %w", err)
	}
	_, err = e.pool.Exec(ctx,
		`INSERT INTO platform_scale_events (cell_id, event_type, reason, snapshot)
		 VALUES ($1, $2, $3, $4)`,
		d.CellID, d.EventType, d.Reason, payload,
	)
	if err != nil {
		return fmt.Errorf("autoscale: insert event: %w", err)
	}
	return nil
}

// AutoscaleLoop runs Evaluate on the supplied tick until ctx is
// cancelled. Errors from a single tick are logged and the loop
// continues; an operator monitoring slog will see them.
type AutoscaleLoop struct {
	engine   *AutoscaleEngine
	interval time.Duration
}

// NewAutoscaleLoop wraps an engine with a tick interval. Defaults
// the interval to 60s when zero.
func NewAutoscaleLoop(engine *AutoscaleEngine, interval time.Duration) *AutoscaleLoop {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &AutoscaleLoop{engine: engine, interval: interval}
}

// Run blocks until ctx is cancelled. The first tick fires immediately
// so a freshly-started worker logs a snapshot without waiting one
// full interval.
func (l *AutoscaleLoop) Run(ctx context.Context) {
	if l == nil || l.engine == nil {
		return
	}
	tick := func() {
		if _, err := l.engine.Evaluate(ctx); err != nil {
			l.engine.logger.Error("autoscale: evaluate", "err", err)
		}
	}
	tick()
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// txOpts is exported so callers / tests can override the lock mode
// if they want to wrap Evaluate inside a longer transaction. Unused
// in the current direct-write path but reserved for the future
// outbox-publish path.
var _ = pgx.TxOptions{}
