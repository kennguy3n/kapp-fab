package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Automated database maintenance — Workstream 4 (NoOps Infrastructure).
//
// DBMaintenanceLoop runs as a platform-level loop in the worker, in the
// same style as the cell autoscaler (autoscaler.go): it is NOT a
// per-customer scheduled_actions row (those are customer-scoped by
// design) but a singleton control-plane sweep started on the elected
// leader. On each daily tick it:
//
//  1. Partition management — keeps the customer-range-partitioned tables
//     (krecords, events, audit_log, inventory_moves, journal_lines)
//     supplied with range partitions, carving the next slice of the
//     partition-key space once the existing partitions approach the
//     configured per-partition capacity.
//  2. Statistics refresh — ANALYZE on tables whose modified-since-analyze
//     count has crossed a churn fraction of their live-tuple estimate, so
//     the planner does not drift on a table that has mutated heavily.
//  3. Index maintenance — REINDEX CONCURRENTLY on a configured set of
//     high-churn indexes, gated to a weekly cadence.
//  4. Bloat detection — logs a warning (and records it) when a table's
//     dead-to-total tuple ratio exceeds a configurable threshold.
//  5. Dead-tuple cleanup — triggers VACUUM on tables whose dead-tuple
//     count exceeds a configurable threshold.
//
// Every action is appended to platform_maintenance_log (see
// migrations/000079_db_maintenance.sql) so an operator can audit the
// self-maintaining database without tailing logs. A failure in any single
// action is logged and recorded but never aborts the rest of the sweep —
// maintenance is best-effort and the next tick retries.

// ActionTypeDBMaintenance is the task label the maintenance loop records
// its runs under and the conventional name for the platform-scoped daily
// maintenance action registered in the worker.
const ActionTypeDBMaintenance = "db_maintenance"

// Maintenance task labels written to platform_maintenance_log.task.
const (
	// MaintenanceTaskPartitionCreate is recorded when a new range
	// partition is created (or a creation attempt is skipped/fails).
	MaintenanceTaskPartitionCreate = "partition_create"
	// MaintenanceTaskAnalyze is recorded when a table is ANALYZEd.
	MaintenanceTaskAnalyze = "analyze"
	// MaintenanceTaskReindex is recorded when an index is reindexed.
	MaintenanceTaskReindex = "reindex"
	// MaintenanceTaskVacuum is recorded when a table is VACUUMed.
	MaintenanceTaskVacuum = "vacuum"
	// MaintenanceTaskBloatCheck is recorded when a bloat threshold is
	// breached.
	MaintenanceTaskBloatCheck = "bloat_check"
)

// platform_maintenance_log.status values.
const (
	maintenanceStatusOK      = "ok"
	maintenanceStatusSkipped = "skipped"
	maintenanceStatusWarning = "warning"
	maintenanceStatusError   = "error"
)

// Range-partition bound sentinels. PostgreSQL spells the open ends of a
// RANGE partition FOR VALUES clause as the bare keywords MINVALUE /
// MAXVALUE (not quoted literals), so they are tracked as sentinels and
// emitted verbatim by boundLiteral.
const (
	boundMinValue = "MINVALUE"
	boundMaxValue = "MAXVALUE"
)

// PartitionedTables are the customer-range-partitioned tables the
// maintenance loop keeps supplied with range partitions. Each is declared
// PARTITION BY RANGE on its partition key in migrations/000001 (plus
// inventory_moves / journal_lines there) and currently ships with a single
// DEFAULT partition; the loop adds explicit range partitions as the data
// set grows.
var PartitionedTables = []string{
	"krecords",
	"events",
	"audit_log",
	"inventory_moves",
	"journal_lines",
}

// identRe matches a safe, unquoted SQL identifier (lower-snake, no schema
// qualifier). Every relation / index name interpolated into a DDL
// statement below is checked against it before use; nothing reaches a
// fmt.Sprintf'd statement that this regex would reject.
var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func validIdent(s string) bool { return identRe.MatchString(s) }

// DBMaintenanceConfig tunes the maintenance loop's thresholds. Zero-value
// fields are not meaningful; construct via DefaultDBMaintenanceConfig or
// LoadDBMaintenanceConfig so every bound is populated.
type DBMaintenanceConfig struct {
	// PartitionTargetCount is the maximum number of range partitions the
	// loop will ever create per partitioned table. Bounds are computed
	// over this fixed count so they stay stable as partitions are added
	// incrementally.
	PartitionTargetCount int
	// PartitionCapacity is the estimated row count one partition is sized
	// to hold; the desired partition count grows as estimated rows cross
	// multiples of it.
	PartitionCapacity int64
	// ReindexInterval is the minimum gap between REINDEX runs. Defaults
	// to weekly.
	ReindexInterval time.Duration
	// ChurnRatio is the modified-since-analyze fraction of live tuples at
	// or above which a table is ANALYZEd (0.10 == 10%).
	ChurnRatio float64
	// BloatWarnRatio is the dead-to-total tuple ratio above which a bloat
	// warning is logged and recorded.
	BloatWarnRatio float64
	// VacuumDeadTuples is the dead-tuple count at or above which a table
	// is VACUUMed.
	VacuumDeadTuples int64
	// HighChurnIndexes are the indexes REINDEXed on the weekly cadence.
	HighChurnIndexes []string
}

// DefaultDBMaintenanceConfig returns conservative production defaults.
func DefaultDBMaintenanceConfig() DBMaintenanceConfig {
	return DBMaintenanceConfig{
		PartitionTargetCount: 16,
		PartitionCapacity:    5_000_000,
		ReindexInterval:      7 * 24 * time.Hour,
		ChurnRatio:           0.10,
		BloatWarnRatio:       0.20,
		VacuumDeadTuples:     50_000,
		// Defaults are non-partitioned, high-churn indexes: a
		// partitioned-table index cannot be REINDEXed CONCURRENTLY in
		// one statement, so the default set deliberately avoids them.
		HighChurnIndexes: []string{
			"workflow_runs_tenant_record_idx",
			"approvals_tenant_state_idx",
		},
	}
}

// LoadDBMaintenanceConfig layers KAPP_DBM_* environment overrides over the
// defaults. Unset / unparseable / non-positive values keep the default for
// that field (matching the getenv* helper semantics used elsewhere in this
// package).
func LoadDBMaintenanceConfig() DBMaintenanceConfig {
	def := DefaultDBMaintenanceConfig()
	cfg := def
	cfg.PartitionTargetCount = getenvInt("KAPP_DBM_PARTITION_TARGET", cfg.PartitionTargetCount)
	cfg.PartitionCapacity = int64(getenvInt("KAPP_DBM_PARTITION_CAPACITY", int(cfg.PartitionCapacity)))
	cfg.ReindexInterval = getenvDuration("KAPP_DBM_REINDEX_INTERVAL", cfg.ReindexInterval)
	// getenvFloat — unlike getenvInt / getenvDuration — does not reject
	// non-positive values, so the ratios are guarded here to honor the
	// "non-positive keeps the default" contract above. A zero/negative
	// ChurnRatio would make AnalyzeNeeded fire on every table on every
	// sweep; a non-positive BloatWarnRatio would warn unconditionally.
	cfg.ChurnRatio = getenvFloat("KAPP_DBM_CHURN_RATIO", cfg.ChurnRatio)
	if cfg.ChurnRatio <= 0 {
		cfg.ChurnRatio = def.ChurnRatio
	}
	cfg.BloatWarnRatio = getenvFloat("KAPP_DBM_BLOAT_WARN_RATIO", cfg.BloatWarnRatio)
	if cfg.BloatWarnRatio <= 0 {
		cfg.BloatWarnRatio = def.BloatWarnRatio
	}
	cfg.VacuumDeadTuples = int64(getenvInt("KAPP_DBM_VACUUM_DEAD_TUPLES", int(cfg.VacuumDeadTuples)))
	if raw := getenv("KAPP_DBM_REINDEX_TARGETS", ""); raw != "" {
		cfg.HighChurnIndexes = parseIndexList(raw)
	}
	return cfg
}

// parseIndexList splits a comma-separated index list and keeps only the
// entries that are safe SQL identifiers.
func parseIndexList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name != "" && validIdent(name) {
			out = append(out, name)
		}
	}
	return out
}

// PartitionSpec is one planned range partition: its child relation name
// and the [Lower, Upper) bounds of the partition-key space it covers.
// Lower / Upper are either a quoted-on-emit UUID literal or one of the
// boundMinValue / boundMaxValue sentinels for the open ends.
type PartitionSpec struct {
	Name  string
	Lower string
	Upper string
}

// createSQL renders the CREATE TABLE ... PARTITION OF statement for this
// spec. parent and Name are validated by the caller (managePartitions)
// before this is reached.
func (s PartitionSpec) createSQL(parent string) string {
	// #nosec G201 -- parent and s.Name are validated against identRe and
	// the bounds are sentinels or hex UUID literals produced by
	// partitionBoundUUID; no caller-controlled text reaches this string.
	return fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%s) TO (%s)",
		s.Name, parent, boundLiteral(s.Lower), boundLiteral(s.Upper),
	)
}

// boundLiteral renders a bound for a FOR VALUES clause: the MINVALUE /
// MAXVALUE keywords pass through bare, everything else is single-quoted as
// a UUID literal.
func boundLiteral(b string) string {
	if b == boundMinValue || b == boundMaxValue {
		return b
	}
	return "'" + b + "'"
}

// PartitionPlan returns the full, stable set of range partitions for a
// parent table split into total equal slices of the 128-bit partition-key
// (UUID) space. The first slice opens at MINVALUE, the last closes at
// MAXVALUE; interior boundaries are evenly spaced. The plan is
// deterministic and total-stable: creating specs[0..k) then later
// extending to specs[0..n) never moves an already-created boundary, which
// is required because PostgreSQL range-partition bounds are immutable.
func PartitionPlan(parent string, total int) []PartitionSpec {
	if total < 1 {
		total = 1
	}
	specs := make([]PartitionSpec, 0, total)
	for i := 0; i < total; i++ {
		lower := boundMinValue
		if i > 0 {
			lower = partitionBoundUUID(i, total)
		}
		upper := boundMaxValue
		if i < total-1 {
			upper = partitionBoundUUID(i+1, total)
		}
		specs = append(specs, PartitionSpec{
			Name:  fmt.Sprintf("%s_p%02d", parent, i),
			Lower: lower,
			Upper: upper,
		})
	}
	return specs
}

// partitionBoundUUID returns the UUID sitting at fraction index/total of
// the 128-bit key space, formatted canonically. index is expected in
// (0, total); the open ends are handled by PartitionPlan as sentinels.
func partitionBoundUUID(index, total int) string {
	span := new(big.Int).Lsh(big.NewInt(1), 128) // 2^128
	b := new(big.Int).Mul(span, big.NewInt(int64(index)))
	b.Div(b, big.NewInt(int64(total)))
	raw := b.Bytes() // big-endian, minimal length
	buf := make([]byte, 16)
	copy(buf[16-len(raw):], raw)
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// DesiredPartitionCount returns how many of the planned partitions should
// exist given the current estimated row count. It grows one partition per
// PartitionCapacity rows, never drops below 1, and is capped at
// maxPartitions so the plan's bounds stay fixed.
func DesiredPartitionCount(estRows, perPartitionCapacity int64, maxPartitions int) int {
	if maxPartitions < 1 {
		return 1
	}
	if perPartitionCapacity <= 0 || estRows <= 0 {
		return 1
	}
	n := estRows / perPartitionCapacity
	if estRows%perPartitionCapacity != 0 {
		n++
	}
	if n < 1 {
		return 1
	}
	if n > int64(maxPartitions) {
		return maxPartitions
	}
	return int(n)
}

// AnalyzeNeeded reports whether a table's modified-since-analyze count has
// reached churnRatio of its live-tuple estimate. A table with no live
// tuples is ANALYZEd as soon as any modification is observed so a freshly
// populated table gets statistics promptly.
func AnalyzeNeeded(modSinceAnalyze, liveTuples int64, churnRatio float64) bool {
	if modSinceAnalyze <= 0 {
		return false
	}
	if liveTuples <= 0 {
		return true
	}
	return float64(modSinceAnalyze) >= churnRatio*float64(liveTuples)
}

// BloatRatio is the dead-to-total tuple ratio in [0, 1]. Zero when the
// table has no tuples accounted for.
func BloatRatio(deadTuples, liveTuples int64) float64 {
	total := deadTuples + liveTuples
	if total <= 0 {
		return 0
	}
	return float64(deadTuples) / float64(total)
}

// VacuumNeeded reports whether dead tuples have reached the threshold. A
// non-positive threshold disables the check.
func VacuumNeeded(deadTuples, threshold int64) bool {
	if threshold <= 0 {
		return false
	}
	return deadTuples >= threshold
}

// dueWeekly reports whether interval has elapsed since the last run. A
// zero last time (never run) is always due.
func dueWeekly(last, now time.Time, interval time.Duration) bool {
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= interval
}

// DBMaintenanceWorker performs one maintenance sweep against a database
// pool. Its verbs — VACUUM, ANALYZE, REINDEX, and CREATE TABLE ...
// PARTITION OF — all require ownership of the target relation on
// PostgreSQL 16 (the MAINTAIN privilege that would let a non-owner run them
// only exists from PostgreSQL 17). Because the sweep covers every user
// table, no per-table GRANT suffices: the pool passed here must belong to
// the role that owns the schema. The worker wires it to a dedicated
// MAINT_DB_URL pool (the bootstrap superuser the migrations run as) and
// only starts the loop when that DSN is configured, so it never silently
// no-ops against an under-privileged connection. See migration 000079.
type DBMaintenanceWorker struct {
	pool   *pgxpool.Pool
	cfg    DBMaintenanceConfig
	logger *slog.Logger
	now    func() time.Time
}

// NewDBMaintenanceWorker binds a config to a pool. A nil logger falls back
// to slog.Default.
func NewDBMaintenanceWorker(pool *pgxpool.Pool, cfg DBMaintenanceConfig, logger *slog.Logger) *DBMaintenanceWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DBMaintenanceWorker{
		pool:   pool,
		cfg:    cfg,
		logger: logger,
		now:    time.Now,
	}
}

// RunOnce performs a single maintenance sweep. Per-phase failures are
// logged and recorded but do not abort the sweep; an error is returned
// only when the worker is misconfigured (no pool).
func (w *DBMaintenanceWorker) RunOnce(ctx context.Context) error {
	if w == nil || w.pool == nil {
		return errors.New("platform: db maintenance worker not configured")
	}
	w.managePartitions(ctx)
	w.refreshStatistics(ctx)
	w.detectBloatAndVacuum(ctx)
	w.reindexHighChurn(ctx)
	return nil
}

// managePartitions creates the next missing range partitions for each
// partitioned table once estimated rows justify them.
func (w *DBMaintenanceWorker) managePartitions(ctx context.Context) {
	for _, parent := range PartitionedTables {
		if !validIdent(parent) {
			continue
		}
		estRows, err := w.estimateRows(ctx, parent)
		if err != nil {
			w.logger.Warn("db maintenance: estimate rows", "table", parent, "err", err)
			continue
		}
		desired := DesiredPartitionCount(estRows, w.cfg.PartitionCapacity, w.cfg.PartitionTargetCount)
		plan := PartitionPlan(parent, w.cfg.PartitionTargetCount)
		existing, err := w.existingPlannedPartitions(ctx, plan)
		if err != nil {
			w.logger.Warn("db maintenance: list partitions", "table", parent, "err", err)
			continue
		}
		// Create every planned partition below the desired count that does
		// not already exist, scanning the whole prefix rather than resuming
		// from a count. A count-as-next-index shortcut assumes the existing
		// partitions form a gapless prefix; a transient failure that creates
		// p01 but not p00 would then be skipped forever. Iterating the prefix
		// and consulting the existing-name set fills such a hole on a later
		// sweep, and IF NOT EXISTS keeps already-present partitions a no-op.
		for i := 0; i < desired && i < len(plan); i++ {
			if existing[plan[i].Name] {
				continue
			}
			w.createPartition(ctx, parent, plan[i], estRows)
		}
	}
}

// createPartition runs one CREATE TABLE ... PARTITION OF and records the
// outcome. A failure (for example a DEFAULT partition that still holds
// rows belonging to the new range) is recorded as an error row and logged,
// not propagated — the next sweep retries.
func (w *DBMaintenanceWorker) createPartition(ctx context.Context, parent string, spec PartitionSpec, estRows int64) {
	if !validIdent(spec.Name) {
		return
	}
	start := w.now()
	// CREATE TABLE ... PARTITION OF requires ownership of the parent table
	// on PostgreSQL 16, which is why the loop runs on the owner-role
	// MAINT_DB_URL pool (see the worker wiring and migration 000079). The
	// bounds and relation names are sentinels or identRe-validated, so the
	// DDL string is safe; it runs under the default protocol because, unlike
	// VACUUM / REINDEX CONCURRENTLY, plain CREATE TABLE is fine inside the
	// implicit transaction pgx uses.
	_, err := w.pool.Exec(ctx, spec.createSQL(parent))
	dur := w.now().Sub(start)
	if err != nil {
		w.logger.Warn("db maintenance: create partition",
			"partition", spec.Name, "parent", parent, "err", err)
		w.record(ctx, MaintenanceTaskPartitionCreate, spec.Name, maintenanceStatusError, err.Error(), dur)
		return
	}
	w.logger.Info("db maintenance: created partition",
		"partition", spec.Name, "parent", parent, "est_rows", estRows)
	w.record(ctx, MaintenanceTaskPartitionCreate, spec.Name, maintenanceStatusOK,
		fmt.Sprintf("est_rows=%d range=[%s,%s)", estRows, spec.Lower, spec.Upper), dur)
}

// tableStat is one row of pg_stat_user_tables relevant to maintenance.
type tableStat struct {
	relName         string
	modSinceAnalyze int64
	liveTuples      int64
	deadTuples      int64
}

// readTableStats reads the maintenance-relevant counters for every user
// table in the public schema.
func (w *DBMaintenanceWorker) readTableStats(ctx context.Context) ([]tableStat, error) {
	rows, err := w.pool.Query(ctx,
		`SELECT relname,
		        COALESCE(n_mod_since_analyze, 0),
		        COALESCE(n_live_tup, 0),
		        COALESCE(n_dead_tup, 0)
		   FROM pg_stat_user_tables
		  WHERE schemaname = 'public'`)
	if err != nil {
		return nil, fmt.Errorf("query table stats: %w", err)
	}
	defer rows.Close()
	out := make([]tableStat, 0, 64)
	for rows.Next() {
		var s tableStat
		if err := rows.Scan(&s.relName, &s.modSinceAnalyze, &s.liveTuples, &s.deadTuples); err != nil {
			return nil, fmt.Errorf("scan table stat: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate table stats: %w", err)
	}
	return out, nil
}

// refreshStatistics ANALYZEs every table whose churn has crossed the
// configured ratio.
func (w *DBMaintenanceWorker) refreshStatistics(ctx context.Context) {
	stats, err := w.readTableStats(ctx)
	if err != nil {
		w.logger.Warn("db maintenance: read stats for analyze", "err", err)
		return
	}
	for _, s := range stats {
		if !validIdent(s.relName) {
			continue
		}
		if !AnalyzeNeeded(s.modSinceAnalyze, s.liveTuples, w.cfg.ChurnRatio) {
			continue
		}
		start := w.now()
		// #nosec G201 -- s.relName comes from pg_stat_user_tables and is
		// re-validated against identRe immediately above.
		//
		// Run admin verbs (ANALYZE/VACUUM/REINDEX) under the simple protocol.
		// They execute fine under pgx's default cache-statement mode too, but
		// the relation name is interpolated into the SQL text, so on a
		// partitioned schema with many tables/partitions each distinct
		// statement would land its own one-shot entry in the per-connection
		// prepared-statement cache and thrash its LRU. These are dynamic,
		// single-use statements with no parameters, so the simple protocol is
		// the right fit: no Parse/Describe round-trip and nothing cached.
		_, err := w.pool.Exec(ctx, fmt.Sprintf("ANALYZE %s", s.relName), pgx.QueryExecModeSimpleProtocol)
		dur := w.now().Sub(start)
		if err != nil {
			w.logger.Warn("db maintenance: analyze", "table", s.relName, "err", err)
			w.record(ctx, MaintenanceTaskAnalyze, s.relName, maintenanceStatusError, err.Error(), dur)
			continue
		}
		w.record(ctx, MaintenanceTaskAnalyze, s.relName, maintenanceStatusOK,
			fmt.Sprintf("mod=%d live=%d", s.modSinceAnalyze, s.liveTuples), dur)
	}
}

// detectBloatAndVacuum reads table stats once, logs/records a warning for
// tables over the bloat threshold, and VACUUMs tables over the dead-tuple
// threshold.
func (w *DBMaintenanceWorker) detectBloatAndVacuum(ctx context.Context) {
	stats, err := w.readTableStats(ctx)
	if err != nil {
		w.logger.Warn("db maintenance: read stats for bloat/vacuum", "err", err)
		return
	}
	for _, s := range stats {
		if !validIdent(s.relName) {
			continue
		}
		if ratio := BloatRatio(s.deadTuples, s.liveTuples); ratio > w.cfg.BloatWarnRatio {
			w.logger.Warn("db maintenance: table bloat over threshold",
				"table", s.relName, "ratio", ratio, "dead", s.deadTuples, "live", s.liveTuples)
			w.record(ctx, MaintenanceTaskBloatCheck, s.relName, maintenanceStatusWarning,
				fmt.Sprintf("bloat_ratio=%.3f dead=%d live=%d", ratio, s.deadTuples, s.liveTuples), 0)
		}
		if !VacuumNeeded(s.deadTuples, w.cfg.VacuumDeadTuples) {
			continue
		}
		start := w.now()
		// #nosec G201 -- s.relName comes from pg_stat_user_tables and is
		// re-validated against identRe immediately above.
		//
		// Simple protocol for the same reason as ANALYZE above: a dynamic,
		// per-relation one-shot statement that should not populate the
		// prepared-statement cache.
		_, err := w.pool.Exec(ctx, fmt.Sprintf("VACUUM (ANALYZE) %s", s.relName), pgx.QueryExecModeSimpleProtocol)
		dur := w.now().Sub(start)
		if err != nil {
			w.logger.Warn("db maintenance: vacuum", "table", s.relName, "err", err)
			w.record(ctx, MaintenanceTaskVacuum, s.relName, maintenanceStatusError, err.Error(), dur)
			continue
		}
		w.record(ctx, MaintenanceTaskVacuum, s.relName, maintenanceStatusOK,
			fmt.Sprintf("dead=%d", s.deadTuples), dur)
	}
}

// reindexHighChurn REINDEXes the configured indexes, but only when the
// weekly cadence has elapsed since the last successful reindex run.
func (w *DBMaintenanceWorker) reindexHighChurn(ctx context.Context) {
	if len(w.cfg.HighChurnIndexes) == 0 {
		return
	}
	now := w.now()
	for _, idx := range w.cfg.HighChurnIndexes {
		if !validIdent(idx) {
			continue
		}
		// Gate the cadence per index, not per phase. A single last-success
		// timestamp for the whole reindex task lets one index's success
		// satisfy the gate and suppress retries of a sibling index that
		// failed, for up to a full interval. Scoping the lookup to this
		// index means a persistently failing index is retried every sweep
		// while healthy indexes still honor the weekly cadence.
		last, err := w.lastSuccessfulRunForTarget(ctx, MaintenanceTaskReindex, idx)
		if err != nil {
			w.logger.Warn("db maintenance: last reindex lookup", "index", idx, "err", err)
			continue
		}
		if !dueWeekly(last, now, w.cfg.ReindexInterval) {
			continue
		}
		start := w.now()
		// #nosec G201 -- idx is drawn from the operator-configured index
		// list and re-validated against identRe immediately above.
		//
		// Simple protocol for the same cache-hygiene reason as ANALYZE/VACUUM
		// above (dynamic, per-index one-shot statement).
		_, err = w.pool.Exec(ctx, fmt.Sprintf("REINDEX INDEX CONCURRENTLY %s", idx), pgx.QueryExecModeSimpleProtocol)
		dur := w.now().Sub(start)
		if err != nil {
			w.logger.Warn("db maintenance: reindex", "index", idx, "err", err)
			w.record(ctx, MaintenanceTaskReindex, idx, maintenanceStatusError, err.Error(), dur)
			continue
		}
		w.record(ctx, MaintenanceTaskReindex, idx, maintenanceStatusOK, "", dur)
	}
}

// estimateRows sums the planner's row estimate over every leaf partition
// of a (possibly partitioned) relation. A plain table is its own leaf, so
// the same query works for both.
func (w *DBMaintenanceWorker) estimateRows(ctx context.Context, relName string) (int64, error) {
	var est int64
	err := w.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(GREATEST(c.reltuples, 0)), 0)::bigint
		   FROM pg_partition_tree($1::regclass) pt
		   JOIN pg_class c ON c.oid = pt.relid
		  WHERE pt.isleaf`,
		relName,
	).Scan(&est)
	if err != nil {
		return 0, fmt.Errorf("estimate rows %s: %w", relName, err)
	}
	return est, nil
}

// existingPlannedPartitions returns the set of planned partition names that
// already exist as relations in the public schema. Returning the concrete
// set (rather than a bare count) lets managePartitions create exactly the
// missing partitions: a count used as the next-to-create index assumes the
// existing partitions form a gapless prefix, so a hole left by an earlier
// transient failure would be skipped permanently. The relnamespace filter
// keeps an unrelated relation of the same name in another schema from being
// mistaken for one of ours.
func (w *DBMaintenanceWorker) existingPlannedPartitions(ctx context.Context, plan []PartitionSpec) (map[string]bool, error) {
	names := make([]string, 0, len(plan))
	for _, s := range plan {
		names = append(names, s.Name)
	}
	rows, err := w.pool.Query(ctx,
		`SELECT relname FROM pg_class
		  WHERE relkind IN ('r', 'p')
		    AND relnamespace = 'public'::regnamespace
		    AND relname = ANY($1)`,
		names,
	)
	if err != nil {
		return nil, fmt.Errorf("list planned partitions: %w", err)
	}
	defer rows.Close()
	existing := make(map[string]bool, len(plan))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan planned partition: %w", err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate planned partitions: %w", err)
	}
	return existing, nil
}

// lastSuccessfulRunForTarget returns the timestamp of the most recent
// successful run of a task against a specific target (e.g. one index), or
// the zero time when that target has never succeeded. Scoping by target lets
// a cadence gate be evaluated per target instead of per task.
func (w *DBMaintenanceWorker) lastSuccessfulRunForTarget(ctx context.Context, task, target string) (time.Time, error) {
	var ts time.Time
	err := w.pool.QueryRow(ctx,
		`SELECT created_at FROM platform_maintenance_log
		  WHERE task = $1 AND target = $2 AND status = $3
		  ORDER BY created_at DESC
		  LIMIT 1`,
		task, target, maintenanceStatusOK,
	).Scan(&ts)
	if err != nil {
		// No rows is the expected "never run" case; surface it as the
		// zero time so the caller treats the cadence as due.
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("last successful run %s: %w", task, err)
	}
	return ts, nil
}

// record appends one row to platform_maintenance_log. Recording failures
// are logged but never propagated — losing an audit row must not abort the
// maintenance work it describes.
func (w *DBMaintenanceWorker) record(ctx context.Context, task, target, status, detail string, dur time.Duration) {
	_, err := w.pool.Exec(ctx,
		`INSERT INTO platform_maintenance_log (task, target, status, detail, duration_ms)
		 VALUES ($1, $2, $3, $4, $5)`,
		task, target, status, detail, dur.Milliseconds(),
	)
	if err != nil {
		w.logger.Warn("db maintenance: record log row", "task", task, "target", target, "err", err)
	}
}

// DBMaintenanceLoop runs RunOnce on the supplied tick until ctx is
// cancelled, mirroring AutoscaleLoop. A single tick's failure is logged
// and the loop continues.
type DBMaintenanceLoop struct {
	worker   *DBMaintenanceWorker
	interval time.Duration
}

// NewDBMaintenanceLoop wraps a worker with a tick interval, defaulting to
// daily when interval is non-positive.
func NewDBMaintenanceLoop(worker *DBMaintenanceWorker, interval time.Duration) *DBMaintenanceLoop {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &DBMaintenanceLoop{worker: worker, interval: interval}
}

// Run blocks until ctx is cancelled. The first tick fires immediately so a
// freshly-elected leader performs a sweep without waiting a full interval.
func (l *DBMaintenanceLoop) Run(ctx context.Context) {
	if l == nil || l.worker == nil {
		return
	}
	tick := func() {
		if err := l.worker.RunOnce(ctx); err != nil {
			l.worker.logger.Error("db maintenance: run", "err", err)
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
