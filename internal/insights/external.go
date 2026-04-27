package insights

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// ExternalSourceTimeout caps every external query end-to-end. Tighter
// than DefaultStatementTimeout because external connections often pay
// extra latency on dial and we still want to stay inside the 30s HTTP
// deadline.
const ExternalSourceTimeout = 15 * time.Second

// MaxExternalRows mirrors MaxResultRows but applies post-fetch on the
// external runner. Caps the rows the runner reads from the remote
// even if the user-supplied LIMIT is missing or too large.
const MaxExternalRows = 5000

// ExternalRunner executes a reporting.Definition whose `Source` is
// `external:<datasource_id>:<table>` against the tenant's registered
// external connection pool. Aggregations / filters / sort / limit
// flow through the same builder grammar; the executor here just
// re-points the FROM clause at the external table and binds the
// connection from the PoolManager.
type ExternalRunner struct {
	store *DataSourceStore
	pools *PoolManager
}

// NewExternalRunner wires an ExternalRunner. Both store and pools
// are required.
func NewExternalRunner(store *DataSourceStore, pools *PoolManager) *ExternalRunner {
	return &ExternalRunner{store: store, pools: pools}
}

// Run resolves the data source, opens / reuses an external pool, and
// executes the (validated) definition against the remote table.
// Returns ErrDataSourceNotFound / ErrDataSourceDisabled where
// applicable so the API surface can map to 404 / 409.
func (r *ExternalRunner) Run(ctx context.Context, tenantID uuid.UUID, def reporting.Definition) (*reporting.Result, error) {
	if r == nil {
		return nil, errors.New("insights: nil external runner")
	}
	if r.store == nil || r.pools == nil {
		return nil, errors.New("insights: external runner not wired")
	}
	dsID, table, err := reporting.ParseExternalSource(def.Source)
	if err != nil {
		return nil, err
	}
	if !isSafeExternalIdentifier(table) {
		return nil, validationErr("external source table %q invalid", table)
	}
	ds, err := r.store.Get(ctx, tenantID, dsID)
	if err != nil {
		return nil, err
	}
	if !ds.Enabled {
		return nil, validationErr("data source %q disabled", ds.Name)
	}
	if ds.Dialect != DialectPostgres {
		return nil, validationErr("dialect %q not supported by external runner", ds.Dialect)
	}

	// Build a query against the remote table by lifting the WHERE /
	// SELECT shape from the definition. We translate the definition
	// to a plain (non-tenant) Postgres query because the remote DB
	// does not have the kapp tenant_id column. The ktype/jsonb
	// projection logic is bypassed — `external:` sources are
	// expected to be plain typed tables.
	query, args, columns, err := buildExternalQuery(def, table)
	if err != nil {
		return nil, err
	}

	dsnSig := fingerprintDSN(ds.ConnectionString)
	pool, err := r.pools.Get(ctx, tenantID, ds.ID, ds.ConnectionString, dsnSig)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, errors.New("insights: external pool unavailable")
	}

	ctx, cancel := context.WithTimeout(ctx, ExternalSourceTimeout)
	defer cancel()

	rows := make([]map[string]any, 0)
	err = withSingleStmtConn(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		// The remote pool runs as whatever user the connection
		// string carries, so the tenant cannot widen the role's
		// SELECT scope through this layer. Defence-in-depth: hard-
		// cap row reads.
		pgxRows, err := conn.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("insights: external execute: %w", err)
		}
		defer pgxRows.Close()
		fieldDescs := pgxRows.FieldDescriptions()
		// Resolve "*" placeholder against actual field descs.
		if len(columns) == 1 && columns[0] == "*" {
			columns = columns[:0]
			for _, fd := range fieldDescs {
				columns = append(columns, string(fd.Name))
			}
		}
		count := 0
		for pgxRows.Next() {
			vals, err := pgxRows.Values()
			if err != nil {
				return err
			}
			row := make(map[string]any, len(vals))
			for i, v := range vals {
				row[string(fieldDescs[i].Name)] = coerceExternalValue(v)
			}
			rows = append(rows, row)
			count++
			if count >= MaxExternalRows {
				break
			}
		}
		return pgxRows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := &reporting.Result{
		Columns: columns,
		Rows:    rows,
		Chart:   def.Chart,
		Summary: map[string]any{"row_count": len(rows)},
	}
	return out, nil
}

// withSingleStmtConn checks out a connection, runs fn, and releases
// the connection without leaking it. Mirrors the pgxpool acquire +
// release dance used elsewhere in the codebase.
func withSingleStmtConn(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, conn *pgxpool.Conn) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("insights: external acquire: %w", err)
	}
	defer conn.Release()
	return fn(ctx, conn)
}

// buildExternalQuery emits a plain Postgres query against an external
// table with the same Filter / Aggregation / Sort / Limit grammar as
// the reporting builder, minus tenant filtering and JSONB projection.
// All identifiers are validated by reporting.Definition.Validate (and
// `table` is checked at the call site).
func buildExternalQuery(def reporting.Definition, table string) (string, []any, []string, error) {
	args := []any{}
	nextArg := 1

	selectExprs := []string{}
	outColumns := []string{}
	aggAliases := map[string]struct{}{}

	if len(def.Aggregations) == 0 && len(def.GroupBy) == 0 {
		for _, c := range def.Columns {
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", c, c))
			outColumns = append(outColumns, c)
		}
		if len(selectExprs) == 0 {
			selectExprs = append(selectExprs, "*")
			outColumns = append(outColumns, "*")
		}
	} else {
		for _, g := range def.GroupBy {
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", g, g))
			outColumns = append(outColumns, g)
		}
		for _, a := range def.Aggregations {
			alias := a.Alias
			if alias == "" {
				if a.Column == "" {
					alias = a.Op
				} else {
					alias = a.Op + "_" + a.Column
				}
			}
			expr, err := externalAggExpr(a)
			if err != nil {
				return "", nil, nil, err
			}
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", expr, alias))
			outColumns = append(outColumns, alias)
			aggAliases[alias] = struct{}{}
		}
	}

	where := []string{}
	for _, f := range def.Filters {
		expr, params, err := externalFilterExpr(f, nextArg)
		if err != nil {
			return "", nil, nil, err
		}
		args = append(args, params...)
		nextArg += len(params)
		where = append(where, expr)
	}
	groupBy := append([]string{}, def.GroupBy...)
	orderBy := []string{}
	for _, s := range def.Sort {
		dir := s.Direction
		if dir == "" {
			dir = "asc"
		}
		ref := s.Column
		if _, ok := aggAliases[s.Column]; !ok {
			ref = s.Column
		}
		orderBy = append(orderBy, fmt.Sprintf("%s %s", ref, strings.ToUpper(dir)))
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s%s",
		strings.Join(selectExprs, ", "),
		table,
		whereClause,
	)
	if len(groupBy) > 0 {
		query += " GROUP BY " + strings.Join(groupBy, ", ")
	}
	if len(orderBy) > 0 {
		query += " ORDER BY " + strings.Join(orderBy, ", ")
	}

	limit := def.Limit
	if limit <= 0 || limit > MaxExternalRows {
		limit = MaxExternalRows
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", nextArg)

	return query, args, outColumns, nil
}

func externalAggExpr(a reporting.Aggregation) (string, error) {
	if a.Op == reporting.AggCount && a.Column == "" {
		return "COUNT(*)", nil
	}
	if a.Column == "" {
		return "", errors.New("insights: external aggregation column required")
	}
	switch a.Op {
	case reporting.AggCount:
		return fmt.Sprintf("COUNT(%s)", a.Column), nil
	case reporting.AggSum:
		return fmt.Sprintf("COALESCE(SUM(%s), 0)", a.Column), nil
	case reporting.AggAvg:
		return fmt.Sprintf("AVG(%s)", a.Column), nil
	case reporting.AggMin:
		return fmt.Sprintf("MIN(%s)", a.Column), nil
	case reporting.AggMax:
		return fmt.Sprintf("MAX(%s)", a.Column), nil
	default:
		return "", fmt.Errorf("insights: unsupported external aggregation %q", a.Op)
	}
}

func externalFilterExpr(f reporting.Filter, startArg int) (string, []any, error) {
	col := f.Column
	op := strings.ToLower(f.Op)
	switch op {
	case "null":
		return fmt.Sprintf("%s IS NULL", col), nil, nil
	case "notnull":
		return fmt.Sprintf("%s IS NOT NULL", col), nil, nil
	case "in":
		var list []any
		if err := externalUnmarshalValue(f.Value, &list); err != nil {
			return "", nil, fmt.Errorf("insights: in-filter value must be array: %w", err)
		}
		if len(list) == 0 {
			return "FALSE", nil, nil
		}
		placeholders := make([]string, len(list))
		for i := range list {
			placeholders[i] = fmt.Sprintf("$%d", startArg+i)
		}
		return fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ", ")), list, nil
	default:
		var v any
		if len(f.Value) > 0 {
			if err := externalUnmarshalValue(f.Value, &v); err != nil {
				return "", nil, fmt.Errorf("insights: external filter value: %w", err)
			}
		}
		return fmt.Sprintf("%s %s $%d", col, op, startArg), []any{v}, nil
	}
}

// externalUnmarshalValue decodes a JSON RawMessage into an arbitrary
// Go value.
func externalUnmarshalValue(raw []byte, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// coerceExternalValue normalises external pgx-returned values for
// JSON serialisation. Mirrors reporting.coerceValue but inlined to
// keep the external runner free of internal symbols.
func coerceExternalValue(v any) any {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case []byte:
		return string(t)
	default:
		return v
	}
}

// fingerprintDSN returns a short hex fingerprint of the connection
// string. Used as a cache invalidation signature so a credential
// rotation closes the previous pool on next use without exposing
// the secret material to the cache.
func fingerprintDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(dsn))
	return hex.EncodeToString(sum[:8])
}

// isSafeExternalIdentifier mirrors reporting.isIdentifier but is
// exported via the external runner so call sites that don't own the
// reporting helper can still validate.
func isSafeExternalIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
