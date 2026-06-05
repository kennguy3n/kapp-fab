package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
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
	// Status is the persisted cells.status lifecycle value (active or
	// draining; provisioning/deprovisioned cells are not snapshotted).
	// A cell already 'draining' is driven to finish teardown regardless
	// of its current metrics (see Evaluate).
	Status string `json:"status,omitempty"`
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

	// provisioning actuates scale decisions against real
	// infrastructure. All three are nil/false by default so the
	// engine preserves its historic observe-only behaviour
	// (persist decision + slog line) until an operator opts in via
	// WithProvisioning. Even when wired, provisioning is best-effort:
	// a provider failure is logged, never fatal to the tick.
	provisioner      CellProvisioner
	rebalancer       *Rebalancer
	provisionEnabled bool
	// manageCellLifecycle is true when the engine should own the
	// persisted cells.status transitions (active -> draining ->
	// deprovisioned) as it tears a cell down. It is false unless
	// provisioning is enabled with a provisioner that actually mutates
	// infrastructure: the NoopProvisioner opts out (observeOnly) so a
	// dry run still touches no control-plane rows.
	manageCellLifecycle bool
}

// NewAutoscaleEngine binds a policy to a pool. Pass nil logger to
// fall back to slog.Default.
func NewAutoscaleEngine(pool *pgxpool.Pool, policy AutoscalePolicy, logger *slog.Logger) *AutoscaleEngine {
	if logger == nil {
		logger = slog.Default()
	}
	return &AutoscaleEngine{pool: pool, policy: policy, logger: logger}
}

// WithProvisioning attaches a provisioner and rebalancer so the engine
// actuates scale decisions when enabled is true. When enabled is false
// (or provisioner is nil) the engine only records decisions, exactly as
// before. The rebalancer is optional: without it, a non-empty cell
// flagged scale_down is left in place (its tenants are never stranded).
// Returns the receiver for chaining.
func (e *AutoscaleEngine) WithProvisioning(provisioner CellProvisioner, rebalancer *Rebalancer, enabled bool) *AutoscaleEngine {
	e.provisioner = provisioner
	e.rebalancer = rebalancer
	e.provisionEnabled = enabled
	// Own the cells.status lifecycle only when we are actuating against a
	// provisioner that really changes infrastructure. A provisioner can
	// opt out (the noop dry-run does) so enabling provisioning with it
	// still mutates nothing — neither infra nor control-plane rows.
	e.manageCellLifecycle = enabled && provisioner != nil
	if oo, ok := provisioner.(interface{ observeOnly() bool }); ok && oo.observeOnly() {
		e.manageCellLifecycle = false
	}
	return e
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
	// actuatable holds only the decisions whose audit row was written.
	// We never actuate a decision we failed to record, so
	// platform_scale_events stays the authoritative log of what the
	// autoscaler acted on (no infrastructure change without an audit row).
	actuatable := make([]Decision, 0, len(snapshots))
	for i := range snapshots {
		d := Decide(snapshots[i], e.policy)
		// A cell already marked 'draining' is mid-teardown: drive it to
		// finish regardless of its current metrics, otherwise a drain
		// deferred to a later tick (maxDrainPerTick / transient capacity
		// shortage) would never resume and its tenants would be stranded.
		if e.provisionEnabled && snapshots[i].Status == CellStatusDraining && d.EventType != CellEventScaleDown {
			d.EventType = CellEventScaleDown
			d.Reason = "resuming teardown (cell already draining)"
		}
		out = append(out, d)
		if err := e.persistDecision(ctx, d); err != nil {
			// Persisting one decision must not block the rest of
			// the cells. Log and continue.
			e.logger.Error("autoscale: persist decision",
				"cell_id", d.CellID, "event_type", d.EventType, "err", err)
			continue
		}
		actuatable = append(actuatable, d)
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
	// Actuate the decisions against infrastructure when provisioning
	// is enabled. Done after the persist loop so platform_scale_events
	// always records what the policy decided even if the provider call
	// later fails. Only decisions whose audit row persisted are actuated
	// (see actuatable). snapshots is threaded through so scale_down can
	// pick a drain target from sibling cells.
	if e.provisionEnabled && e.provisioner != nil {
		e.actuate(ctx, snapshots, actuatable)
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
		        COALESCE(last_event.event_type, '')               AS last_event_type,
		        COALESCE(c.status, $1)                            AS status
		   FROM cells c
		   LEFT JOIN LATERAL (
		       SELECT created_at, event_type
		         FROM platform_scale_events e
		        WHERE e.cell_id = c.id
		          AND e.event_type <> 'hold'
		        ORDER BY e.created_at DESC
		        LIMIT 1
		   ) last_event ON TRUE
		  -- Evaluate cells that are serving tenants ('active') plus cells
		  -- mid-teardown ('draining') so a deferred drain resumes. Cells
		  -- still 'provisioning' or already 'deprovisioned' are skipped and
		  -- never chosen as drain targets. Uses cells_region_status_idx
		  -- (migration 000081). A 'draining' cell is never a drain target
		  -- (filtered in drainTargets by its Status).
		  WHERE COALESCE(c.status, $1) IN ($1, $2)
		  ORDER BY c.id`,
		CellStatusActive, CellStatusDraining,
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
			&s.LastScaleEventAt, &s.LastScaleEventType, &s.Status,
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

// maxDrainPerTick caps how many tenants the autoscaler migrates off a
// cell in a single tick before deferring the rest to the next tick.
// scale_down only fires well below MaxTenantsPerCell, so this is a
// guardrail against a pathological policy rather than the common path;
// it keeps one Evaluate from issuing an unbounded burst of migration
// transactions.
const maxDrainPerTick = 500

// actuate translates the recorded decisions into provider calls. It is
// only invoked when provisioning is enabled and a provisioner is wired.
// Every provider interaction is best-effort: failures are logged and
// the loop continues so one bad cell cannot wedge the others.
//
// snapshots is shared (and mutated) across every drain in the tick: as a
// drain places tenants on a sibling, that sibling's TenantCount in
// snapshots is bumped so a later drain in the same region sees the
// reduced headroom and cannot collectively overfill it past max_tenants.
// draining holds every cell scheduled for teardown this tick so none of
// them is ever chosen as a drain target (a cell being deprovisioned must
// not receive tenants, even if its stale snapshot still shows capacity).
func (e *AutoscaleEngine) actuate(ctx context.Context, snapshots []CellSnapshot, decisions []Decision) {
	draining := make(map[string]bool, len(decisions))
	for i := range decisions {
		if decisions[i].EventType == CellEventScaleDown {
			draining[decisions[i].CellID] = true
		}
	}
	for i := range decisions {
		switch decisions[i].EventType {
		case CellEventScaleUp:
			e.provisionForScaleUp(ctx, decisions[i])
		case CellEventScaleDown:
			e.drainAndDeprovision(ctx, decisions[i], snapshots, draining)
		}
	}
}

// provisionForScaleUp asks the provisioner to add capacity in the
// region of the cell that tripped the scale_up threshold.
func (e *AutoscaleEngine) provisionForScaleUp(ctx context.Context, d Decision) {
	spec := CellSpec{MaxTenants: e.policy.MaxTenantsPerCell}
	cell, err := e.provisioner.Provision(ctx, d.Snapshot.Region, spec)
	if err != nil {
		e.logger.Error("autoscale: provision cell",
			"region", d.Snapshot.Region, "trigger_cell", d.CellID, "err", err)
		return
	}
	e.logger.Info("autoscale: provisioned cell",
		"cell_id", cell.ID, "region", cell.Region, "trigger_cell", d.CellID)
}

// drainAndDeprovision empties a cell of its tenants (migrating them onto
// sibling cells in the same region) and then deprovisions it. A cell is
// only torn down once it is empty, so tenants are never stranded.
func (e *AutoscaleEngine) drainAndDeprovision(ctx context.Context, d Decision, snapshots []CellSnapshot, draining map[string]bool) {
	// The implicit 'default' cell is never deprovisioned: legacy and
	// NULL-cell tenants are accounted to it and it is the placement of
	// last resort.
	if d.CellID == DefaultCellID {
		e.logger.Debug("autoscale: skip deprovision of default cell", "cell_id", d.CellID)
		return
	}
	if d.Snapshot.TenantCount > 0 && e.rebalancer == nil {
		// Cannot empty it and we must not strand tenants: leave the cell
		// 'active' and serving. Deliberately NOT marked 'draining'.
		e.logger.Warn("autoscale: scale_down not actuated; cell not empty and no rebalancer wired",
			"cell_id", d.CellID, "tenants", d.Snapshot.TenantCount)
		return
	}
	// Committed to tearing this cell down. Mark it 'draining' up front so
	// the cell-router stops placing new tenants on it, it is excluded as a
	// drain target, and — if the drain has to be deferred across ticks —
	// the next Evaluate re-surfaces it and resumes the teardown. The mark
	// is best-effort and idempotent; a no-op for the noop provisioner.
	e.markCellStatus(ctx, d.CellID, CellStatusDraining)
	// Verify the cell is actually empty before tearing it down, using a
	// LIVE query rather than trusting d.Snapshot.TenantCount (captured by
	// snapshotCells at the START of the tick). This closes the TOCTOU
	// window where a tenant is placed — or an in-flight placement commits
	// — between the snapshot and teardown: such a tenant would otherwise
	// be stranded on a cell we are about to deprovision. (Skipped when no
	// pool is wired, e.g. unit tests: there is nothing to query.)
	needsDrain := d.Snapshot.TenantCount > 0
	if !needsDrain && e.pool != nil {
		liveIDs, err := e.tenantsOnCell(ctx, d.CellID)
		if err != nil {
			// Stays 'draining'; the next tick re-verifies and retries.
			e.logger.Error("autoscale: verify cell empty before deprovision",
				"cell_id", d.CellID, "err", err)
			return
		}
		if len(liveIDs) > 0 {
			if e.rebalancer == nil {
				// A tenant landed after the snapshot and we have no way to
				// move it. Leave the cell 'draining' (the router already
				// avoids it) so a later tick with a rebalancer finishes the
				// teardown; never strand a tenant by deprovisioning under it.
				e.logger.Warn("autoscale: deprovision deferred; tenant arrived after snapshot and no rebalancer wired",
					"cell_id", d.CellID, "tenants", len(liveIDs))
				return
			}
			needsDrain = true
		}
	}
	if needsDrain {
		drained, err := e.drainCell(ctx, d, snapshots, draining)
		if err != nil {
			// Stays 'draining'; the next tick resumes from where we left off.
			e.logger.Error("autoscale: drain cell", "cell_id", d.CellID, "err", err)
			return
		}
		if !drained {
			e.logger.Warn("autoscale: deprovision deferred; cell not fully drained",
				"cell_id", d.CellID)
			return
		}
	}
	if err := e.provisioner.Deprovision(ctx, d.CellID); err != nil {
		// Stays 'draining' (empty); the next tick retries the deprovision.
		e.logger.Error("autoscale: deprovision cell", "cell_id", d.CellID, "err", err)
		return
	}
	e.markCellStatus(ctx, d.CellID, CellStatusDeprovisioned)
	e.logger.Info("autoscale: deprovisioned cell", "cell_id", d.CellID)
}

// markCellStatus transitions a cell's persisted lifecycle status. It is
// a best-effort control-plane write (logged, never fatal to the tick)
// and a no-op when the engine does not own the lifecycle (provisioning
// disabled, or a dry-run/noop provisioner) or has no pool wired.
func (e *AutoscaleEngine) markCellStatus(ctx context.Context, cellID, status string) {
	if !e.manageCellLifecycle || e.pool == nil {
		return
	}
	if _, err := e.pool.Exec(ctx, `UPDATE cells SET status = $2 WHERE id = $1`, cellID, status); err != nil {
		e.logger.Error("autoscale: update cell status",
			"cell_id", cellID, "status", status, "err", err)
	}
}

// drainTarget is a candidate destination cell with its remaining
// headroom. remaining is mutated as tenants are assigned to it so we
// never overfill a target; snap points back into the shared snapshots
// slice so the same placement is reflected for any other cell drained
// later in the same tick (see actuate).
type drainTarget struct {
	id        string
	remaining int
	snap      *CellSnapshot
}

// drainCell migrates every tenant off d.CellID onto sibling cells in the
// same region, returning true when the cell ends up empty. It returns
// false (no error) when there is insufficient sibling capacity — the
// caller then declines to deprovision so no tenant is stranded.
func (e *AutoscaleEngine) drainCell(ctx context.Context, d Decision, snapshots []CellSnapshot, draining map[string]bool) (bool, error) {
	targets := e.drainTargets(d, snapshots, draining)
	if len(targets) == 0 {
		return false, nil
	}
	tenantIDs, err := e.tenantsOnCell(ctx, d.CellID)
	if err != nil {
		return false, err
	}
	migrated := 0
	for _, tid := range tenantIDs {
		if migrated >= maxDrainPerTick {
			return false, nil // defer the remainder to the next tick
		}
		tgt := pickTarget(targets)
		if tgt == nil {
			return false, nil // ran out of capacity; do not strand tenants
		}
		if err := e.rebalancer.MigrateTenant(ctx, tid, d.CellID, tgt.id); err != nil {
			// A tenant that already moved (concurrent migration) or a
			// no-op is fine to skip; anything else aborts the drain so
			// we don't deprovision a cell we failed to empty.
			if errors.Is(err, ErrTenantNotOnSourceCell) || errors.Is(err, ErrNoOpMigration) {
				continue
			}
			return false, err
		}
		tgt.remaining--
		// Reflect the placement in the shared snapshot so a later drain
		// in this same tick sees the reduced headroom on this target.
		if tgt.snap != nil {
			tgt.snap.TenantCount++
		}
		migrated++
	}
	return true, nil
}

// drainTargets builds the candidate destination list for draining
// d.CellID: every other cell in the SAME region with spare capacity.
// Cross-region moves are deliberately excluded — relocating a tenant to
// another region changes its data residency and must be an explicit
// operator decision, never an automatic side effect of autoscaling.
func (e *AutoscaleEngine) drainTargets(d Decision, snapshots []CellSnapshot, draining map[string]bool) []*drainTarget {
	out := make([]*drainTarget, 0, len(snapshots))
	for i := range snapshots {
		s := &snapshots[i]
		if s.ID == d.CellID || s.Region != d.Snapshot.Region {
			continue
		}
		// A cell that is itself being torn down must never receive
		// tenants, even if its snapshot shows headroom — whether it is
		// already persisted as 'draining' (a prior/in-flight teardown) or
		// is scheduled for scale_down in this same tick.
		if s.Status == CellStatusDraining || draining[s.ID] {
			continue
		}
		capacity := s.MaxTenants
		if capacity <= 0 {
			capacity = e.policy.MaxTenantsPerCell
		}
		if remaining := capacity - s.TenantCount; remaining > 0 {
			out = append(out, &drainTarget{id: s.ID, remaining: remaining, snap: s})
		}
	}
	return out
}

// pickTarget returns the target with the most remaining headroom, or nil
// when none has capacity left. Spreading onto the emptiest cell keeps
// the drained load balanced rather than refilling a single sibling.
func pickTarget(targets []*drainTarget) *drainTarget {
	var best *drainTarget
	for _, t := range targets {
		if t.remaining <= 0 {
			continue
		}
		if best == nil || t.remaining > best.remaining {
			best = t
		}
	}
	return best
}

// tenantsOnCell lists the ids of tenants currently placed on cellID,
// folding a NULL cell_id onto the implicit 'default' cell to match the
// snapshot query. Ordered by id for deterministic drain behaviour.
func (e *AutoscaleEngine) tenantsOnCell(ctx context.Context, cellID string) ([]uuid.UUID, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT id FROM tenants
		  WHERE COALESCE(cell_id, $1) = $2
		  ORDER BY id`,
		DefaultCellID, cellID)
	if err != nil {
		return nil, fmt.Errorf("autoscale: list tenants on cell %q: %w", cellID, err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("autoscale: scan tenant id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// txOpts is exported so callers / tests can override the lock mode
// if they want to wrap Evaluate inside a longer transaction. Unused
// in the current direct-write path but reserved for the future
// outbox-publish path.
var _ = pgx.TxOptions{}
