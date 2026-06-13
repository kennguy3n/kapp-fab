package warehouse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/insights"
)

// DefaultBatchCopyTimeout bounds a single source export (read + write
// of one relation). A sync that selects many sources gets this budget
// per source so one slow/huge relation cannot pin a worker iteration
// indefinitely.
const DefaultBatchCopyTimeout = 10 * time.Minute

// destResolver is the subset of the insights data-source layer the
// exporter needs: look up + decrypt a destination connection, and hand
// back a pooled connection to it. Both production collaborators
// (*insights.DataSourceStore, *insights.PoolManager) satisfy this via
// the small adapter below; tests can substitute a fake that points at
// a local schema.
type dataSourceLookup interface {
	Get(ctx context.Context, tenantID, id uuid.UUID) (*insights.DataSource, error)
}

type poolProvider interface {
	Get(ctx context.Context, tenantID, dataSourceID uuid.UUID, dsn, dsnSig string) (*pgxpool.Pool, error)
}

// Exporter moves a tenant's selected sources into a destination
// warehouse. Reads run under tenant RLS against the local primary;
// writes go to the external destination resolved through the reused
// insights datasource + pool model. The exporter is stateless beyond
// its collaborators, so a single instance is shared across all tenants
// by the worker.
type Exporter struct {
	local       *pgxpool.Pool
	dataSources dataSourceLookup
	pools       poolProvider
	timeout     time.Duration
}

// NewExporter wires the exporter. local is the kapp primary pool
// (tenant-scoped reads); dataSources + pools are the insights
// collaborators reused as the destination connection model.
func NewExporter(local *pgxpool.Pool, dataSources *insights.DataSourceStore, pools *insights.PoolManager) *Exporter {
	return &Exporter{
		local:       local,
		dataSources: dataSources,
		pools:       pools,
		timeout:     DefaultBatchCopyTimeout,
	}
}

// WithTimeout overrides the per-source copy budget. Returns the
// receiver for chaining.
func (e *Exporter) WithTimeout(d time.Duration) *Exporter {
	if d > 0 {
		e.timeout = d
	}
	return e
}

// ExportResult is the outcome of exporting one config: per-source row
// counts that landed and the advanced per-source watermark cursors.
type ExportResult struct {
	RowsBySource map[string]int64
	Watermarks   map[string]json.RawMessage
}

// Export runs every source of a config into the destination warehouse.
// It resolves the destination once (so all sources share one pooled
// connection set) and processes sources sequentially. A per-source
// error aborts the run: the export is all-or-nothing within a run so a
// partially-mirrored warehouse never advertises a successful sync.
// Watermarks for the sources that DID land are still returned so a
// re-run resumes from the furthest durable point.
func (e *Exporter) Export(ctx context.Context, cfg *Config) (ExportResult, error) {
	res := ExportResult{
		RowsBySource: map[string]int64{},
		Watermarks:   map[string]json.RawMessage{},
	}
	// Seed the returned watermark map with the existing cursors so a
	// caller persisting res.Watermarks never drops a source that was
	// not reached this run.
	for k, v := range cfg.Watermarks {
		res.Watermarks[k] = v
	}

	dest, err := e.resolveDestination(ctx, cfg.TenantID, cfg.DestinationDataSourceID)
	if err != nil {
		return res, err
	}
	if err := ensureSchema(ctx, dest, cfg.DestinationSchema); err != nil {
		return res, err
	}

	for _, key := range cfg.Sources {
		d, err := resolveSource(key)
		if err != nil {
			return res, err
		}
		rows, wm, err := e.exportSource(ctx, cfg, d, dest)
		if err != nil {
			return res, fmt.Errorf("warehouse: export source %q: %w", key, err)
		}
		res.RowsBySource[key] = rows
		if wm != nil {
			res.Watermarks[key] = wm
		}
	}
	return res, nil
}

// resolveDestination looks up + decrypts the destination datasource
// and returns a pooled connection to it, reusing the insights pool
// cache (keyed by the same DSN fingerprint so a credential rotation
// invalidates the stale pool).
func (e *Exporter) resolveDestination(ctx context.Context, tenantID, dsID uuid.UUID) (*pgxpool.Pool, error) {
	ds, err := e.dataSources.Get(ctx, tenantID, dsID)
	if err != nil {
		return nil, fmt.Errorf("warehouse: resolve destination: %w", err)
	}
	if !ds.Enabled {
		return nil, fmt.Errorf("warehouse: destination datasource %s is disabled", dsID)
	}
	if ds.Dialect != "postgres" {
		return nil, fmt.Errorf("warehouse: unsupported destination dialect %q", ds.Dialect)
	}
	sig := insights.FingerprintDSN(ds.ConnectionString)
	pool, err := e.pools.Get(ctx, tenantID, ds.ID, ds.ConnectionString, sig)
	if err != nil {
		return nil, fmt.Errorf("warehouse: open destination pool: %w", err)
	}
	return pool, nil
}

// exportSource streams one source into its destination table and
// returns the rows that landed plus the advanced watermark (nil when
// the source has no incremental cursor or no new rows). The source is
// read in a single ordered cursor under tenant RLS; the rows feed the
// destination COPY directly so memory stays bounded regardless of
// relation size.
func (e *Exporter) exportSource(ctx context.Context, cfg *Config, d sourceDescriptor, dest *pgxpool.Pool) (int64, json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// stock_levels-style sources have no per-row cursor, so they are
	// always fully replaced regardless of the config's mode.
	effectiveFull := cfg.Mode == ModeFull || d.wmKind == watermarkNone

	cur, err := parseCursor(d, cfg.Watermarks[d.key])
	if err != nil {
		return 0, nil, err
	}
	// Full mode reads from the beginning (and truncates the target),
	// so it ignores any stored cursor.
	if effectiveFull {
		cur = cursor{}
	}

	target := d.destTable()
	if !isIdentifier(target) {
		return 0, nil, fmt.Errorf("warehouse: unsafe target table %q", target)
	}

	// Prepare the destination table up front (idempotent) so an empty
	// source still produces a queryable mirror table.
	destConn, err := dest.Acquire(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("warehouse: acquire destination: %w", err)
	}
	defer destConn.Release()

	if err := ensureTable(ctx, destConn.Conn(), cfg.DestinationSchema, target, d); err != nil {
		return 0, nil, err
	}

	var (
		rowsLanded int64
		newWM      json.RawMessage
	)
	err = dbutil.WithReadOnlyTenantTxOnPool(ctx, e.local, cfg.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		sql, args := buildSelectSQL(cfg.TenantID, d, cur)
		rows, qerr := tx.Query(ctx, sql, args...)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()

		src := &copyFromSource{rows: rows, desc: d}
		// Wrap the destination write in a transaction so the swap is
		// atomic: a full reload never exposes a truncated-but-not-yet-
		// repopulated table, and an incremental upsert is all-or-nothing.
		destTx, terr := destConn.Begin(ctx)
		if terr != nil {
			return fmt.Errorf("warehouse: begin destination tx: %w", terr)
		}
		committed := false
		defer func() {
			if !committed {
				// WithoutCancel so an aborted run still rolls back
				// cleanly even when the caller's ctx is already done.
				_ = destTx.Rollback(context.WithoutCancel(ctx))
			}
		}()
		if effectiveFull {
			if cerr := copyFull(ctx, destTx, cfg.DestinationSchema, target, d, src); cerr != nil {
				return cerr
			}
		} else {
			if cerr := copyIncremental(ctx, destTx, cfg.DestinationSchema, target, d, src); cerr != nil {
				return cerr
			}
		}
		if src.err != nil {
			return src.err
		}
		if rerr := rows.Err(); rerr != nil {
			return rerr
		}
		if cerr := destTx.Commit(ctx); cerr != nil {
			return fmt.Errorf("warehouse: commit destination tx: %w", cerr)
		}
		committed = true
		rowsLanded = src.count
		if src.sawRow {
			newWM = src.watermark.serialize(d)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return rowsLanded, newWM, nil
}

// copyFull truncates the target and streams every source row into it
// in one COPY, inside the caller's destination transaction so the
// reload is atomic. Safe because the keyset cursor yields each primary
// key at most once, so a truncated-then-copied table cannot conflict.
func copyFull(ctx context.Context, tx pgx.Tx, schema, table string, d sourceDescriptor, src *copyFromSource) error {
	fq := pgx.Identifier{schema, table}.Sanitize()
	if _, err := tx.Exec(ctx, "TRUNCATE TABLE "+fq); err != nil {
		return fmt.Errorf("warehouse: truncate %s: %w", fq, err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{schema, table}, d.destColumns(), src); err != nil {
		return fmt.Errorf("warehouse: copy into %s: %w", fq, err)
	}
	return nil
}

// copyIncremental streams the new/changed source rows into a temp
// staging table, then upserts them into the target by primary key.
// This is the standard high-throughput incremental pattern: COPY is
// the fast bulk path, the ON CONFLICT merge applies inserts and
// updates in one set-based statement.
func copyIncremental(ctx context.Context, tx pgx.Tx, schema, table string, d sourceDescriptor, src *copyFromSource) error {
	staging := "_wh_stage_" + table
	fq := pgx.Identifier{schema, table}.Sanitize()
	stageFQ := pgx.Identifier{staging}.Sanitize()

	// ON COMMIT DROP ties the staging table's lifetime to this
	// transaction so a rolled-back or committed run leaves nothing
	// behind on the pooled connection.
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE %s (LIKE %s) ON COMMIT DROP", stageFQ, fq)); err != nil {
		return fmt.Errorf("warehouse: create staging: %w", err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{staging}, d.destColumns(), src); err != nil {
		return fmt.Errorf("warehouse: copy into staging: %w", err)
	}
	if _, err := tx.Exec(ctx, buildUpsertSQL(schema, table, staging, d)); err != nil {
		return fmt.Errorf("warehouse: upsert into %s: %w", fq, err)
	}
	return nil
}

// ensureSchema creates the destination schema if absent.
func ensureSchema(ctx context.Context, dest *pgxpool.Pool, schema string) error {
	if !isIdentifier(schema) {
		return fmt.Errorf("warehouse: unsafe destination schema %q", schema)
	}
	if _, err := dest.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return fmt.Errorf("warehouse: ensure schema %q: %w", schema, err)
	}
	return nil
}

// ensureTable creates the destination mirror table if absent.
func ensureTable(ctx context.Context, conn *pgx.Conn, schema, table string, d sourceDescriptor) error {
	if _, err := conn.Exec(ctx, buildCreateTableSQL(schema, table, d)); err != nil {
		return fmt.Errorf("warehouse: ensure table %q.%q: %w", schema, table, err)
	}
	return nil
}

// buildCreateTableSQL renders the idempotent DDL for a mirror table.
// Column names + types come from the code-defined descriptor (never
// user input) and the schema/table identifiers are sanitised, so no
// untrusted value reaches the statement.
func buildCreateTableSQL(schema, table string, d sourceDescriptor) string {
	cols := make([]string, 0, len(d.columns))
	for _, c := range d.columns {
		cols = append(cols, fmt.Sprintf("%s %s", pgx.Identifier{c.dest}.Sanitize(), c.typ))
	}
	pkCols := make([]string, 0, len(d.pk))
	for _, p := range d.pk {
		pkCols = append(pkCols, pgx.Identifier{p}.Sanitize())
	}
	def := strings.Join(cols, ", ")
	if len(pkCols) > 0 {
		def += ", PRIMARY KEY (" + strings.Join(pkCols, ", ") + ")"
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)",
		pgx.Identifier{schema, table}.Sanitize(), def)
}

// buildUpsertSQL renders the INSERT … SELECT … ON CONFLICT merge that
// applies a staging batch to the target table. Non-PK columns are
// overwritten from the staged row so an update to a mirrored record
// (e.g. a soft-deleted krecord) propagates.
func buildUpsertSQL(schema, table, staging string, d sourceDescriptor) string {
	dest := d.destColumns()
	quoted := make([]string, len(dest))
	for i, c := range dest {
		quoted[i] = pgx.Identifier{c}.Sanitize()
	}
	pkSet := make(map[string]struct{}, len(d.pk))
	pkCols := make([]string, len(d.pk))
	for i, p := range d.pk {
		pkCols[i] = pgx.Identifier{p}.Sanitize()
		pkSet[p] = struct{}{}
	}
	setClauses := make([]string, 0, len(dest))
	for _, c := range dest {
		if _, isPK := pkSet[c]; isPK {
			continue
		}
		q := pgx.Identifier{c}.Sanitize()
		setClauses = append(setClauses, fmt.Sprintf("%s = EXCLUDED.%s", q, q))
	}
	colList := strings.Join(quoted, ", ")
	conflict := strings.Join(pkCols, ", ")
	action := "DO NOTHING"
	if len(setClauses) > 0 {
		action = "DO UPDATE SET " + strings.Join(setClauses, ", ")
	}
	return fmt.Sprintf(
		"INSERT INTO %s (%s) SELECT %s FROM %s ON CONFLICT (%s) %s",
		pgx.Identifier{schema, table}.Sanitize(), colList, colList,
		pgx.Identifier{staging}.Sanitize(), conflict, action)
}

// buildSelectSQL renders the ordered source read for a descriptor. The
// column list, relation, and order key are all code-defined; the only
// parameters are tenantID, the optional KType filter, and the keyset
// lower bound, all bound as $N placeholders. args is returned in the
// exact order the placeholders are numbered.
//
// The leading tenant_id = $1 predicate is defense-in-depth: the read
// already runs under tenant RLS, but ledger.stock_levels is a plain
// (non security_invoker) VIEW that executes as its owner and so
// bypasses RLS on the underlying inventory_moves table. Without an
// explicit tenant filter that single source would export every
// tenant's rows. The reporting engine pins the same predicate for the
// same reason (internal/reporting/builder.go buildQuery), so every
// source here is filtered identically regardless of whether the
// relation is an RLS-protected table or an RLS-bypassing view.
func buildSelectSQL(tenantID uuid.UUID, d sourceDescriptor, cur cursor) (query string, args []any) {
	cols := make([]string, len(d.columns))
	for i, c := range d.columns {
		cols[i] = c.src
	}
	conds := []string{"tenant_id = $1"}
	args = append(args, tenantID)
	n := 2
	if d.ktype != "" {
		conds = append(conds, fmt.Sprintf("ktype = $%d", n))
		args = append(args, d.ktype)
		n++
	}
	var orderCols []string
	switch d.wmKind {
	case watermarkTimestampUUID:
		orderCols = []string{d.wmTimeCol, d.wmUUIDCol}
		if cur.hasTime {
			conds = append(conds, fmt.Sprintf("(%s, %s) > ($%d, $%d::uuid)", d.wmTimeCol, d.wmUUIDCol, n, n+1))
			args = append(args, cur.ts, cur.id)
		}
	case watermarkBigint:
		orderCols = []string{d.wmBigCol}
		if cur.hasSeq {
			conds = append(conds, fmt.Sprintf("%s > $%d", d.wmBigCol, n))
			args = append(args, cur.seq)
		}
	default:
		orderCols = append(orderCols, d.pk...)
	}
	query = "SELECT " + strings.Join(cols, ", ") + " FROM " + d.relation
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	if len(orderCols) > 0 {
		query += " ORDER BY " + strings.Join(orderCols, ", ")
	}
	return query, args
}
