// Package reporting implements the Phase I report builder: a metadata-
// driven engine that runs user-authored report definitions against
// krecords (any KType) and the typed ledger tables. Definitions are
// stored as JSONB on saved_reports and interpreted here — there is no
// SQL generation on untrusted input because every operation is
// expressed in a bounded grammar that the runner validates and maps to
// parameterised queries.
//
// Reference: frappe/frappe Report Builder. The grammar here covers the
// subset that matters for SME reporting: columns, filters, group-by,
// aggregations (sum/count/avg/min/max), sort, limit, pivot, and chart
// serialization. Anything richer lands via explicit handlers.
package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Supported data sources. `ktype:<name>` selects a KRecord table slice
// filtered by ktype; `ledger.<table>` selects one of a handful of
// allowed typed tables so callers can't point the engine at arbitrary
// SQL tables; `external:<datasource_id>:<table>` routes to a tenant-
// owned external Postgres pool resolved by insights.Runner. The
// reporting runner does not execute external sources itself — it
// validates the prefix shape and the calling layer (insights) routes
// the query to the external pool via PoolManager + DataSourceStore.
const (
	SourceKTypePrefix    = "ktype:"
	SourceLedger         = "ledger."
	SourceExternalPrefix = "external:"
)

// Allowed ledger sources — explicit allow-list so a typo can't leak
// rows from tables we haven't blessed for reporting.
var allowedLedgerSources = map[string]struct{}{
	"ledger.journal_lines":   {},
	"ledger.journal_entries": {},
	"ledger.stock_levels":    {},
}

// Aggregation operators. `count` is the only one that works on non-
// numeric fields; the others require JSONB → numeric casts handled by
// the runner.
const (
	AggCount = "count"
	AggSum   = "sum"
	AggAvg   = "avg"
	AggMin   = "min"
	AggMax   = "max"
)

// Filter operators accepted by the runner. Maintained explicitly so
// stringly-typed JSON can't smuggle SQL fragments through.
var allowedFilterOps = map[string]struct{}{
	"=":       {},
	"!=":      {},
	">":       {},
	">=":      {},
	"<":       {},
	"<=":      {},
	"like":    {},
	"in":      {},
	"null":    {},
	"notnull": {},
}

// Chart types the definition can request. The runner doesn't render
// the chart — it just validates the hint so the web layer can pick a
// renderer.
const (
	ChartBar  = "bar"
	ChartLine = "line"
	ChartPie  = "pie"
	ChartNone = ""
)

// Filter is a single predicate over a column. Value is raw JSON so
// dates, booleans, numbers, strings, and IN-lists all flow through.
type Filter struct {
	Column string          `json:"column"`
	Op     string          `json:"op"`
	Value  json.RawMessage `json:"value,omitempty"`
}

// Aggregation is a (op, column, alias) triple. Alias is the output
// column name that Result.Rows maps to.
type Aggregation struct {
	Op     string `json:"op"`
	Column string `json:"column"`
	Alias  string `json:"alias"`
}

// Sort is (column, direction) where direction is "asc" or "desc".
type Sort struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

// ChartSpec optionally hints the web layer about preferred chart
// rendering. The runner doesn't render charts — see apps/web.
type ChartSpec struct {
	Type    string `json:"type,omitempty"`
	XColumn string `json:"x_column,omitempty"`
	YColumn string `json:"y_column,omitempty"`
}

// PivotSpec turns the row set into a pivot: rows index by RowColumn,
// columns by ColumnColumn, and each cell holds the aggregated value.
type PivotSpec struct {
	RowColumn    string `json:"row_column,omitempty"`
	ColumnColumn string `json:"column_column,omitempty"`
	ValueColumn  string `json:"value_column,omitempty"`
}

// Join describes a tenant-scoped join from the primary source onto a
// secondary KType or ledger table. Only inner / left joins are
// supported in v1; the runner enforces tenant_id-on-both-sides at
// the SQL layer so RLS is defence-in-depth and the application
// filter is a hard ceiling.
//
// Alias is required; Columns referenced in the projection / filters
// / sort / group-by use `alias.column` notation when they target the
// joined source. The On predicate is restricted to a single equality
// predicate: `<left_column> = <alias>.<right_column>`. Composite keys
// can be modelled by additional Filter entries on either side.
type Join struct {
	Source      string `json:"source"`
	Alias       string `json:"alias"`
	Kind        string `json:"kind,omitempty"`
	LeftColumn  string `json:"left_column"`
	RightColumn string `json:"right_column"`
}

// JoinKind constants. inner is the default; left preserves
// unmatched rows on the primary source.
const (
	JoinInner = "inner"
	JoinLeft  = "left"
)

// MaxJoinsHardCeiling caps the absolute number of joins per query
// regardless of plan tier. The plan-level limit is enforced by the
// insights layer; the engine-level cap is a defence-in-depth fence
// so a misconfigured plan row can't unbound the query.
const MaxJoinsHardCeiling = 4

// Definition is the persisted report grammar. `Source` is required;
// everything else is optional. Columns + Aggregations together define
// the projection, GroupBy defines the bucketing, Filters the WHERE,
// Sort the ORDER BY, Limit caps the result.
type Definition struct {
	Source       string        `json:"source"`
	Columns      []string      `json:"columns,omitempty"`
	Filters      []Filter      `json:"filters,omitempty"`
	GroupBy      []string      `json:"group_by,omitempty"`
	Aggregations []Aggregation `json:"aggregations,omitempty"`
	Sort         []Sort        `json:"sort,omitempty"`
	Limit        int           `json:"limit,omitempty"`
	Pivot        *PivotSpec    `json:"pivot,omitempty"`
	Chart        *ChartSpec    `json:"chart,omitempty"`
	Joins        []Join        `json:"joins,omitempty"`
}

// Result is what Run returns. Columns is the ordered list of keys in
// Rows so the UI can render a table without inferring ordering.
type Result struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Pivot   *PivotResult     `json:"pivot,omitempty"`
	Chart   *ChartSpec       `json:"chart,omitempty"`
	Summary map[string]any   `json:"summary,omitempty"`
}

// PivotResult is the materialised pivot table. RowHeaders list every
// unique row value, ColumnHeaders every unique column value, and Cells
// is a map from row-value to (column-value → aggregated value).
type PivotResult struct {
	RowColumn     string                    `json:"row_column"`
	ColumnColumn  string                    `json:"column_column"`
	ValueColumn   string                    `json:"value_column"`
	RowHeaders    []string                  `json:"row_headers"`
	ColumnHeaders []string                  `json:"column_headers"`
	Cells         map[string]map[string]any `json:"cells"`
}

// Validate enforces the grammar constraints. Called by both persist
// and run so a bad definition is never stored and never executed.
func (d *Definition) Validate() error {
	if d == nil || d.Source == "" {
		return errors.New("reporting: source required")
	}
	if strings.HasPrefix(d.Source, SourceKTypePrefix) {
		if len(strings.TrimPrefix(d.Source, SourceKTypePrefix)) == 0 {
			return errors.New("reporting: ktype name required")
		}
	} else if strings.HasPrefix(d.Source, SourceExternalPrefix) {
		// external:<datasource_uuid>:<table>. The reporting layer
		// only validates the shape; execution is owned by insights.
		dsID, table, err := ParseExternalSource(d.Source)
		if err != nil {
			return err
		}
		if dsID == uuid.Nil {
			return errors.New("reporting: external data source id required")
		}
		if !isIdentifier(table) {
			return fmt.Errorf("reporting: invalid external table name %q", table)
		}
	} else if _, ok := allowedLedgerSources[d.Source]; !ok {
		return fmt.Errorf("reporting: unsupported source %q", d.Source)
	}
	// When the definition declares joins, columns may carry an
	// `<alias>.<col>` qualifier; otherwise the strict bare-identifier
	// rule applies.
	colCheck := isIdentifier
	if len(d.Joins) > 0 {
		colCheck = isColumnRef
	}
	for _, c := range d.Columns {
		if !colCheck(c) {
			return fmt.Errorf("reporting: invalid column identifier %q", c)
		}
	}
	for _, f := range d.Filters {
		if f.Column == "" {
			return errors.New("reporting: filter column required")
		}
		if _, ok := allowedFilterOps[strings.ToLower(f.Op)]; !ok {
			return fmt.Errorf("reporting: unsupported filter op %q", f.Op)
		}
		if !colCheck(f.Column) {
			return fmt.Errorf("reporting: invalid column identifier %q", f.Column)
		}
	}
	for _, g := range d.GroupBy {
		if !colCheck(g) {
			return fmt.Errorf("reporting: invalid group_by identifier %q", g)
		}
	}
	for _, a := range d.Aggregations {
		switch a.Op {
		case AggCount, AggSum, AggAvg, AggMin, AggMax:
		default:
			return fmt.Errorf("reporting: unsupported aggregation %q", a.Op)
		}
		if a.Column != "" && !colCheck(a.Column) {
			return fmt.Errorf("reporting: invalid aggregation column %q", a.Column)
		}
		if a.Alias != "" && !isIdentifier(a.Alias) {
			return fmt.Errorf("reporting: invalid aggregation alias %q", a.Alias)
		}
	}
	for _, s := range d.Sort {
		if !colCheck(s.Column) {
			return fmt.Errorf("reporting: invalid sort column %q", s.Column)
		}
		if s.Direction != "asc" && s.Direction != "desc" && s.Direction != "" {
			return fmt.Errorf("reporting: invalid sort direction %q", s.Direction)
		}
	}
	if d.Pivot != nil {
		if !isIdentifier(d.Pivot.RowColumn) || !isIdentifier(d.Pivot.ColumnColumn) || !isIdentifier(d.Pivot.ValueColumn) {
			return errors.New("reporting: pivot row/column/value required")
		}
	}
	if d.Chart != nil {
		switch d.Chart.Type {
		case ChartBar, ChartLine, ChartPie, ChartNone:
		default:
			return fmt.Errorf("reporting: unsupported chart type %q", d.Chart.Type)
		}
	}
	if d.Limit < 0 {
		return errors.New("reporting: limit must be non-negative")
	}
	if len(d.Joins) > MaxJoinsHardCeiling {
		return fmt.Errorf("reporting: too many joins (%d > %d)", len(d.Joins), MaxJoinsHardCeiling)
	}
	seenAliases := map[string]struct{}{}
	for _, j := range d.Joins {
		if !isIdentifier(j.Alias) {
			return fmt.Errorf("reporting: invalid join alias %q", j.Alias)
		}
		if _, dup := seenAliases[j.Alias]; dup {
			return fmt.Errorf("reporting: duplicate join alias %q", j.Alias)
		}
		seenAliases[j.Alias] = struct{}{}
		if !strings.HasPrefix(j.Source, SourceKTypePrefix) {
			if _, ok := allowedLedgerSources[j.Source]; !ok {
				return fmt.Errorf("reporting: unsupported join source %q", j.Source)
			}
		} else if len(strings.TrimPrefix(j.Source, SourceKTypePrefix)) == 0 {
			return errors.New("reporting: join ktype name required")
		}
		switch j.Kind {
		case "", JoinInner, JoinLeft:
		default:
			return fmt.Errorf("reporting: unsupported join kind %q", j.Kind)
		}
		if !isIdentifier(j.LeftColumn) {
			return fmt.Errorf("reporting: invalid join left column %q", j.LeftColumn)
		}
		if !isIdentifier(j.RightColumn) {
			return fmt.Errorf("reporting: invalid join right column %q", j.RightColumn)
		}
	}
	return nil
}

// Runner executes Definitions against the database. The runner never
// concatenates caller-supplied strings into SQL directly: identifiers
// go through isIdentifier + a whitelist and values flow as $N params.
type Runner struct {
	pool *pgxpool.Pool
}

// NewRunner wires a Runner from the shared pool.
func NewRunner(pool *pgxpool.Pool) *Runner {
	return &Runner{pool: pool}
}

// Run executes the definition for a tenant and returns a Result. The
// tenant_id is applied both as a WHERE condition and via the
// dbutil.WithTenantTx GUC so RLS backs up the application-level
// filter.
func (r *Runner) Run(ctx context.Context, tenantID uuid.UUID, def Definition) (*Result, error) {
	return r.RunWithStatementTimeout(ctx, tenantID, def, 0)
}

// RunWithStatementTimeout behaves like Run but applies SET LOCAL
// statement_timeout inside the same transaction as the reporting
// query so the database cancels a runaway scan even if the Go
// context cancellation is delayed (e.g. blocked in syscall, slow row
// streaming). A non-positive timeout disables the SET LOCAL, which
// keeps the helper a drop-in for callers that don't care about a
// DB-side fence.
func (r *Runner) RunWithStatementTimeout(ctx context.Context, tenantID uuid.UUID, def Definition, timeout time.Duration) (*Result, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("reporting: tenant id required")
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	if def.Limit == 0 || def.Limit > 5000 {
		def.Limit = 1000
	}

	rows := make([]map[string]any, 0)
	columns := []string{}
	err := dbutil.WithTenantTx(ctx, r.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if timeout > 0 {
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", timeout.Milliseconds())); err != nil {
				return fmt.Errorf("reporting: set statement_timeout: %w", err)
			}
		}
		query, args, cols, err := buildQuery(tenantID, def)
		if err != nil {
			return err
		}
		columns = cols
		pgxRows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("reporting: execute: %w", err)
		}
		defer pgxRows.Close()
		fieldDescs := pgxRows.FieldDescriptions()
		// When buildQuery emitted SELECT * as a fallback (no explicit
		// columns, no aggregations, no group-by), the placeholder "*"
		// in outColumns doesn't match any real row key. Rebuild columns
		// from the actual field descriptions so Result.Columns lines
		// up with the keys the UI looks up on each row.
		if len(columns) == 1 && columns[0] == "*" {
			columns = columns[:0]
			for _, fd := range fieldDescs {
				columns = append(columns, string(fd.Name))
			}
		}
		for pgxRows.Next() {
			vals, err := pgxRows.Values()
			if err != nil {
				return err
			}
			row := make(map[string]any, len(vals))
			for i, v := range vals {
				row[string(fieldDescs[i].Name)] = coerceValue(v)
			}
			rows = append(rows, row)
		}
		return pgxRows.Err()
	})
	if err != nil {
		return nil, err
	}

	out := &Result{
		Columns: columns,
		Rows:    rows,
		Chart:   def.Chart,
		Summary: summarise(rows, def),
	}
	if def.Pivot != nil {
		out.Pivot = pivot(rows, *def.Pivot)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Query assembly
// ---------------------------------------------------------------------------

// primaryAlias is the SQL alias the primary source carries when joins
// are present. Underscore-prefixed so it never collides with a user-
// supplied identifier (which must start with [a-zA-Z_] but in practice
// all UI-driven names are lowercase letters).
const primaryAlias = "_p"

// joinedSource is the resolved metadata for a join target. The runner
// builds a per-join lookup so column/filter expressions can route a
// `<alias>.<col>` reference to the right base table + jsonb column.
type joinedSource struct {
	alias     string
	base      string
	jsonbCol  string
	ktypeName string
	kind      string
	leftCol   string
	rightCol  string
}

func resolveJoins(def Definition) ([]joinedSource, error) {
	if len(def.Joins) == 0 {
		return nil, nil
	}
	out := make([]joinedSource, 0, len(def.Joins))
	for _, j := range def.Joins {
		js := joinedSource{alias: j.Alias, kind: j.Kind, leftCol: j.LeftColumn, rightCol: j.RightColumn}
		if js.kind == "" {
			js.kind = JoinInner
		}
		if strings.HasPrefix(j.Source, SourceKTypePrefix) {
			js.ktypeName = strings.TrimPrefix(j.Source, SourceKTypePrefix)
			js.base = "krecords"
			js.jsonbCol = "data"
		} else {
			js.base = strings.TrimPrefix(j.Source, "ledger.")
		}
		out = append(out, js)
	}
	return out, nil
}

// resolveColumn parses a (possibly alias-qualified) column reference
// and returns the SQL expression that reads it. Bare names target the
// primary source; `<alias>.<col>` targets the matching join.
func resolveColumn(name, primaryJSONBCol string, hasJoins bool, joins []joinedSource) (string, error) {
	if dot := strings.IndexByte(name, '.'); dot > 0 && hasJoins {
		alias := name[:dot]
		col := name[dot+1:]
		if !isIdentifier(alias) || !isIdentifier(col) {
			return "", fmt.Errorf("reporting: invalid alias-qualified column %q", name)
		}
		for _, j := range joins {
			if j.alias != alias {
				continue
			}
			return qualifiedColumnExpr(alias, col, j.jsonbCol), nil
		}
		return "", fmt.Errorf("reporting: unknown join alias %q", alias)
	}
	if !isIdentifier(name) {
		return "", fmt.Errorf("reporting: invalid column %q", name)
	}
	if hasJoins {
		return qualifiedColumnExpr(primaryAlias, name, primaryJSONBCol), nil
	}
	return columnExpr(name, primaryJSONBCol), nil
}

// resolveColumnForFilter is like resolveColumn but used by filter
// callers that already validated the column is alias-qualified.
func resolveColumnForFilter(f Filter, primaryJSONBCol string, hasJoins bool, joins []joinedSource) (string, error) {
	return resolveColumn(f.Column, primaryJSONBCol, hasJoins, joins)
}

func buildQuery(tenantID uuid.UUID, def Definition) (string, []any, []string, error) {
	var (
		base      string
		jsonbCol  string
		ktypeName string
	)
	if strings.HasPrefix(def.Source, SourceKTypePrefix) {
		ktypeName = strings.TrimPrefix(def.Source, SourceKTypePrefix)
		base = "krecords"
		jsonbCol = "data"
	} else if strings.HasPrefix(def.Source, SourceExternalPrefix) {
		// External execution is owned by the insights layer; the
		// reporting builder rejects external sources because the
		// runner here only knows how to talk to the local DB.
		return "", nil, nil, fmt.Errorf("reporting: external source %q must be executed via insights.Runner", def.Source)
	} else {
		base = strings.TrimPrefix(def.Source, "ledger.")
	}

	joins, err := resolveJoins(def)
	if err != nil {
		return "", nil, nil, err
	}
	hasJoins := len(joins) > 0

	args := []any{tenantID}
	nextArg := 2

	selectExprs := []string{}
	outColumns := []string{}
	// aggAliases tracks aggregation output aliases so ORDER BY can emit
	// them as bare identifiers instead of routing through columnExpr —
	// otherwise ktype-source sorts on an alias would become
	// `data->>'alias'` and match nothing.
	aggAliases := map[string]struct{}{}

	if len(def.Aggregations) == 0 && len(def.GroupBy) == 0 {
		for _, c := range def.Columns {
			expr, err := resolveColumn(c, jsonbCol, hasJoins, joins)
			if err != nil {
				return "", nil, nil, err
			}
			outAlias := outAliasFromColumn(c)
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", expr, outAlias))
			outColumns = append(outColumns, outAlias)
		}
		if len(selectExprs) == 0 {
			if hasJoins {
				selectExprs = append(selectExprs, primaryAlias+".*")
			} else {
				selectExprs = append(selectExprs, "*")
			}
			outColumns = append(outColumns, "*")
		}
	} else {
		for _, g := range def.GroupBy {
			expr, err := resolveColumn(g, jsonbCol, hasJoins, joins)
			if err != nil {
				return "", nil, nil, err
			}
			outAlias := outAliasFromColumn(g)
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", expr, outAlias))
			outColumns = append(outColumns, outAlias)
		}
		for _, a := range def.Aggregations {
			alias := a.Alias
			if alias == "" {
				alias = a.Op
				if a.Column != "" {
					alias = a.Op + "_" + outAliasFromColumn(a.Column)
				}
			}
			aggExpr, err := aggregationExprQualified(a, jsonbCol, hasJoins, joins)
			if err != nil {
				return "", nil, nil, err
			}
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", aggExpr, alias))
			outColumns = append(outColumns, alias)
			aggAliases[alias] = struct{}{}
		}
	}

	tenantCol := "tenant_id"
	if hasJoins {
		tenantCol = primaryAlias + ".tenant_id"
	}
	where := []string{tenantCol + " = $1"}
	if ktypeName != "" {
		args = append(args, ktypeName)
		ktCol := "ktype"
		delCol := "deleted_at"
		if hasJoins {
			ktCol = primaryAlias + ".ktype"
			delCol = primaryAlias + ".deleted_at"
		}
		where = append(where, fmt.Sprintf("%s = $%d", ktCol, nextArg))
		nextArg++
		// Exclude soft-deleted records by default so aggregate reports
		// (SUM of deal amounts, outstanding AR/AP, etc.) don't silently
		// include deleted rows. Authors who need deleted data can add
		// an explicit `deleted_at notnull` filter.
		where = append(where, delCol+" IS NULL")
	}
	for _, f := range def.Filters {
		colExpr, err := resolveColumnForFilter(f, jsonbCol, hasJoins, joins)
		if err != nil {
			return "", nil, nil, err
		}
		expr, params, err := filterExprWithColumn(f, colExpr, nextArg)
		if err != nil {
			return "", nil, nil, err
		}
		args = append(args, params...)
		nextArg += len(params)
		where = append(where, expr)
	}

	groupBy := []string{}
	for _, g := range def.GroupBy {
		expr, err := resolveColumn(g, jsonbCol, hasJoins, joins)
		if err != nil {
			return "", nil, nil, err
		}
		groupBy = append(groupBy, expr)
	}

	orderBy := []string{}
	for _, s := range def.Sort {
		dir := s.Direction
		if dir == "" {
			dir = "asc"
		}
		var expr string
		if _, ok := aggAliases[s.Column]; ok {
			// Aggregation output aliases (e.g. "sum_amount") are already
			// bare identifiers in the SELECT list — route them straight
			// through without JSONB extraction.
			expr = s.Column
		} else {
			resolved, err := resolveColumn(s.Column, jsonbCol, hasJoins, joins)
			if err != nil {
				return "", nil, nil, err
			}
			expr = resolved
		}
		orderBy = append(orderBy, fmt.Sprintf("%s %s", expr, strings.ToUpper(dir)))
	}

	fromClause := base
	if hasJoins {
		fromClause = fmt.Sprintf("%s AS %s", base, primaryAlias)
		for _, j := range joins {
			joinKind := "INNER JOIN"
			if j.kind == JoinLeft {
				joinKind = "LEFT JOIN"
			}
			joinedTenantPredicate := fmt.Sprintf("%s.tenant_id = $1", j.alias)
			joinedKtypePredicate := ""
			if j.ktypeName != "" {
				args = append(args, j.ktypeName)
				joinedKtypePredicate = fmt.Sprintf(" AND %s.ktype = $%d AND %s.deleted_at IS NULL", j.alias, nextArg, j.alias)
				nextArg++
			}
			leftQualified := qualifiedColumnExpr(primaryAlias, j.leftCol, jsonbCol)
			rightQualified := qualifiedColumnExpr(j.alias, j.rightCol, j.jsonbCol)
			fromClause += fmt.Sprintf(
				" %s %s AS %s ON %s = %s AND %s%s",
				joinKind, j.base, j.alias,
				leftQualified, rightQualified,
				joinedTenantPredicate, joinedKtypePredicate,
			)
		}
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s",
		strings.Join(selectExprs, ", "),
		fromClause,
		strings.Join(where, " AND "),
	)
	if len(groupBy) > 0 {
		query += " GROUP BY " + strings.Join(groupBy, ", ")
	}
	if len(orderBy) > 0 {
		query += " ORDER BY " + strings.Join(orderBy, ", ")
	}
	args = append(args, def.Limit)
	query += fmt.Sprintf(" LIMIT $%d", nextArg)

	return query, args, outColumns, nil
}

// columnExpr returns the SQL expression that reads a logical column.
// For KRecord sources, non-system columns are projected from the
// JSONB `data` column via `data->>'field'` so the caller can reference
// any KType field by name without ALTER TABLE.
func columnExpr(col, jsonbCol string) string {
	if jsonbCol == "" {
		return col
	}
	// Only true krecords columns are projected raw. `status` is deliberately
	// excluded because it's the lifecycle flag ('active'/'deleted'); every
	// KType's own business status lives in `data->>'status'`, which is what
	// report authors almost always want. Callers who really need the
	// lifecycle flag can reference `deleted_at IS NULL` via a filter.
	switch col {
	case "id", "tenant_id", "ktype", "ktype_version", "version",
		"created_at", "updated_at", "created_by", "updated_by", "deleted_at":
		return col
	default:
		return fmt.Sprintf("%s->>'%s'", jsonbCol, col)
	}
}

// qualifiedColumnExpr is columnExpr with an explicit table-alias
// prefix. Used when the FROM clause needs aliases (joins). Returns
// `<alias>.<col>` for typed columns and `<alias>.<jsonbCol>->>'<col>'`
// for JSONB-projected fields.
func qualifiedColumnExpr(alias, col, jsonbCol string) string {
	if jsonbCol == "" {
		return alias + "." + col
	}
	switch col {
	case "id", "tenant_id", "ktype", "ktype_version", "version",
		"created_at", "updated_at", "created_by", "updated_by", "deleted_at":
		return alias + "." + col
	default:
		return fmt.Sprintf("%s.%s->>'%s'", alias, jsonbCol, col)
	}
}

// outAliasFromColumn turns a (possibly alias-qualified) column
// reference into a SQL output alias. Bare `name` becomes `name`;
// `alias.name` becomes `alias_name` so the JSON map key the runner
// emits is a single-token identifier.
func outAliasFromColumn(name string) string {
	if dot := strings.IndexByte(name, '.'); dot > 0 {
		return name[:dot] + "_" + name[dot+1:]
	}
	return name
}

func aggregationExprQualified(a Aggregation, primaryJSONBCol string, hasJoins bool, joins []joinedSource) (string, error) {
	if a.Op == AggCount && a.Column == "" {
		return "COUNT(*)", nil
	}
	if a.Column == "" {
		return "", errors.New("reporting: aggregation column required")
	}
	colExpr, err := resolveColumn(a.Column, primaryJSONBCol, hasJoins, joins)
	if err != nil {
		return "", err
	}
	// JSONB-projected expressions need NULLIF + ::numeric for SUM/AVG/MIN/MAX;
	// typed columns use the expression as-is. Detect via the `->>` operator.
	numeric := colExpr
	if strings.Contains(colExpr, "->>") {
		numeric = fmt.Sprintf("NULLIF(%s, '')::numeric", colExpr)
	}
	switch a.Op {
	case AggCount:
		return fmt.Sprintf("COUNT(%s)", colExpr), nil
	case AggSum:
		return fmt.Sprintf("COALESCE(SUM(%s), 0)", numeric), nil
	case AggAvg:
		return fmt.Sprintf("AVG(%s)", numeric), nil
	case AggMin:
		return fmt.Sprintf("MIN(%s)", numeric), nil
	case AggMax:
		return fmt.Sprintf("MAX(%s)", numeric), nil
	default:
		return "", fmt.Errorf("reporting: unsupported aggregation %q", a.Op)
	}
}

func filterExprWithColumn(f Filter, colExpr string, startArg int) (string, []any, error) {
	op := strings.ToLower(f.Op)
	switch op {
	case "null":
		return fmt.Sprintf("%s IS NULL", colExpr), nil, nil
	case "notnull":
		return fmt.Sprintf("%s IS NOT NULL", colExpr), nil, nil
	case "in":
		var list []any
		if err := json.Unmarshal(f.Value, &list); err != nil {
			return "", nil, fmt.Errorf("reporting: in-filter value must be array: %w", err)
		}
		if len(list) == 0 {
			return "FALSE", nil, nil
		}
		placeholders := make([]string, len(list))
		for i := range list {
			placeholders[i] = fmt.Sprintf("$%d", startArg+i)
		}
		return fmt.Sprintf("%s IN (%s)", colExpr, strings.Join(placeholders, ", ")), list, nil
	default:
		var v any
		if len(f.Value) > 0 {
			if err := json.Unmarshal(f.Value, &v); err != nil {
				return "", nil, fmt.Errorf("reporting: filter value: %w", err)
			}
		}
		return fmt.Sprintf("%s %s $%d", colExpr, op, startArg), []any{v}, nil
	}
}

// coerceValue normalises pgx-returned values so JSON serialisation
// yields clean output: time.Time → RFC3339, numeric decimals kept as
// strings (via fmt), byte slices → string.
func coerceValue(v any) any {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case []byte:
		return string(t)
	default:
		return v
	}
}

func summarise(rows []map[string]any, def Definition) map[string]any {
	summary := map[string]any{
		"row_count": len(rows),
	}
	if len(def.Aggregations) == 0 {
		return summary
	}
	return summary
}

func pivot(rows []map[string]any, spec PivotSpec) *PivotResult {
	rowSet := map[string]struct{}{}
	colSet := map[string]struct{}{}
	cells := map[string]map[string]any{}
	for _, row := range rows {
		rowKey := fmt.Sprint(row[spec.RowColumn])
		colKey := fmt.Sprint(row[spec.ColumnColumn])
		rowSet[rowKey] = struct{}{}
		colSet[colKey] = struct{}{}
		if _, ok := cells[rowKey]; !ok {
			cells[rowKey] = map[string]any{}
		}
		cells[rowKey][colKey] = row[spec.ValueColumn]
	}
	rowHeaders := make([]string, 0, len(rowSet))
	for r := range rowSet {
		rowHeaders = append(rowHeaders, r)
	}
	colHeaders := make([]string, 0, len(colSet))
	for c := range colSet {
		colHeaders = append(colHeaders, c)
	}
	sort.Strings(rowHeaders)
	sort.Strings(colHeaders)
	return &PivotResult{
		RowColumn:     spec.RowColumn,
		ColumnColumn:  spec.ColumnColumn,
		ValueColumn:   spec.ValueColumn,
		RowHeaders:    rowHeaders,
		ColumnHeaders: colHeaders,
		Cells:         cells,
	}
}

// ParseExternalSource splits an external:<uuid>:<table> source string
// into its components. Returns (datasource_id, table_name, nil) on
// success or a tagged error suitable for the validator path.
func ParseExternalSource(source string) (uuid.UUID, string, error) {
	if !strings.HasPrefix(source, SourceExternalPrefix) {
		return uuid.Nil, "", fmt.Errorf("reporting: source %q is not external", source)
	}
	tail := strings.TrimPrefix(source, SourceExternalPrefix)
	parts := strings.SplitN(tail, ":", 2)
	if len(parts) != 2 {
		return uuid.Nil, "", errors.New("reporting: external source must be external:<datasource_uuid>:<table>")
	}
	id, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("reporting: external source datasource_id: %w", err)
	}
	if parts[1] == "" {
		return uuid.Nil, "", errors.New("reporting: external source table required")
	}
	return id, parts[1], nil
}

// isColumnRef accepts either a bare identifier or an alias-qualified
// `alias.column` reference. Used by Validate when Joins is non-empty
// so the engine can carry references like `t1.deal_id` from query
// definition through to resolveColumn during execution. Bare names
// fall through to the strict isIdentifier check.
func isColumnRef(s string) bool {
	if dot := strings.IndexByte(s, '.'); dot > 0 {
		return isIdentifier(s[:dot]) && isIdentifier(s[dot+1:])
	}
	return isIdentifier(s)
}

// isIdentifier ensures a caller-provided string is safe to interpolate
// as a SQL identifier. Only `[A-Za-z_][A-Za-z0-9_]*` is allowed so
// keywords, whitespace, quotes, and semicolons are rejected.
func isIdentifier(s string) bool {
	if s == "" {
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
