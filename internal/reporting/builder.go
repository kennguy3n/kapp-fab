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
// SQL tables.
const (
	SourceKTypePrefix = "ktype:"
	SourceLedger      = "ledger."
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
	"=":    {},
	"!=":   {},
	">":    {},
	">=":   {},
	"<":    {},
	"<=":   {},
	"like": {},
	"in":   {},
	"null": {},
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
}

// Result is what Run returns. Columns is the ordered list of keys in
// Rows so the UI can render a table without inferring ordering.
type Result struct {
	Columns []string                   `json:"columns"`
	Rows    []map[string]any           `json:"rows"`
	Pivot   *PivotResult               `json:"pivot,omitempty"`
	Chart   *ChartSpec                 `json:"chart,omitempty"`
	Summary map[string]any             `json:"summary,omitempty"`
}

// PivotResult is the materialised pivot table. RowHeaders list every
// unique row value, ColumnHeaders every unique column value, and Cells
// is a map from row-value to (column-value → aggregated value).
type PivotResult struct {
	RowColumn     string                       `json:"row_column"`
	ColumnColumn  string                       `json:"column_column"`
	ValueColumn   string                       `json:"value_column"`
	RowHeaders    []string                     `json:"row_headers"`
	ColumnHeaders []string                     `json:"column_headers"`
	Cells         map[string]map[string]any    `json:"cells"`
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
	} else if _, ok := allowedLedgerSources[d.Source]; !ok {
		return fmt.Errorf("reporting: unsupported source %q", d.Source)
	}
	for _, c := range d.Columns {
		if !isIdentifier(c) {
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
		if !isIdentifier(f.Column) {
			return fmt.Errorf("reporting: invalid column identifier %q", f.Column)
		}
	}
	for _, g := range d.GroupBy {
		if !isIdentifier(g) {
			return fmt.Errorf("reporting: invalid group_by identifier %q", g)
		}
	}
	for _, a := range d.Aggregations {
		switch a.Op {
		case AggCount, AggSum, AggAvg, AggMin, AggMax:
		default:
			return fmt.Errorf("reporting: unsupported aggregation %q", a.Op)
		}
		if a.Column != "" && !isIdentifier(a.Column) {
			return fmt.Errorf("reporting: invalid aggregation column %q", a.Column)
		}
		if a.Alias != "" && !isIdentifier(a.Alias) {
			return fmt.Errorf("reporting: invalid aggregation alias %q", a.Alias)
		}
	}
	for _, s := range d.Sort {
		if !isIdentifier(s.Column) {
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

func buildQuery(tenantID uuid.UUID, def Definition) (string, []any, []string, error) {
	var (
		base       string
		jsonbCol   string
		ktypeName  string
	)
	if strings.HasPrefix(def.Source, SourceKTypePrefix) {
		ktypeName = strings.TrimPrefix(def.Source, SourceKTypePrefix)
		base = "krecords"
		jsonbCol = "data"
	} else {
		base = strings.TrimPrefix(def.Source, "ledger.")
	}

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
			if !isIdentifier(c) {
				return "", nil, nil, fmt.Errorf("reporting: invalid column %q", c)
			}
			expr := columnExpr(c, jsonbCol)
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", expr, c))
			outColumns = append(outColumns, c)
		}
		if len(selectExprs) == 0 {
			selectExprs = append(selectExprs, "*")
			outColumns = append(outColumns, "*")
		}
	} else {
		for _, g := range def.GroupBy {
			expr := columnExpr(g, jsonbCol)
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", expr, g))
			outColumns = append(outColumns, g)
		}
		for _, a := range def.Aggregations {
			alias := a.Alias
			if alias == "" {
				alias = a.Op
				if a.Column != "" {
					alias = a.Op + "_" + a.Column
				}
			}
			aggExpr, err := aggregationExpr(a, jsonbCol)
			if err != nil {
				return "", nil, nil, err
			}
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS %s", aggExpr, alias))
			outColumns = append(outColumns, alias)
			aggAliases[alias] = struct{}{}
		}
	}

	where := []string{"tenant_id = $1"}
	if ktypeName != "" {
		args = append(args, ktypeName)
		where = append(where, fmt.Sprintf("ktype = $%d", nextArg))
		nextArg++
		// Exclude soft-deleted records by default so aggregate reports
		// (SUM of deal amounts, outstanding AR/AP, etc.) don't silently
		// include deleted rows. Authors who need deleted data can add
		// an explicit `deleted_at notnull` filter.
		where = append(where, "deleted_at IS NULL")
	}
	for _, f := range def.Filters {
		expr, params, err := filterExpr(f, jsonbCol, nextArg)
		if err != nil {
			return "", nil, nil, err
		}
		args = append(args, params...)
		nextArg += len(params)
		where = append(where, expr)
	}

	groupBy := []string{}
	for _, g := range def.GroupBy {
		groupBy = append(groupBy, columnExpr(g, jsonbCol))
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
			expr = columnExpr(s.Column, jsonbCol)
		}
		orderBy = append(orderBy, fmt.Sprintf("%s %s", expr, strings.ToUpper(dir)))
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s",
		strings.Join(selectExprs, ", "),
		base,
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

func aggregationExpr(a Aggregation, jsonbCol string) (string, error) {
	if a.Op == AggCount && a.Column == "" {
		return "COUNT(*)", nil
	}
	if a.Column == "" {
		return "", errors.New("reporting: aggregation column required")
	}
	col := columnExpr(a.Column, jsonbCol)
	// KType sources extract from JSONB as text and need NULLIF + ::numeric;
	// ledger sources are already typed columns and must not be cast through ''.
	numeric := col
	if jsonbCol != "" {
		numeric = fmt.Sprintf("NULLIF(%s, '')::numeric", col)
	}
	switch a.Op {
	case AggCount:
		return fmt.Sprintf("COUNT(%s)", col), nil
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

func filterExpr(f Filter, jsonbCol string, startArg int) (string, []any, error) {
	col := columnExpr(f.Column, jsonbCol)
	op := strings.ToLower(f.Op)
	switch op {
	case "null":
		return fmt.Sprintf("%s IS NULL", col), nil, nil
	case "notnull":
		return fmt.Sprintf("%s IS NOT NULL", col), nil, nil
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
		return fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ", ")), list, nil
	default:
		var v any
		if len(f.Value) > 0 {
			if err := json.Unmarshal(f.Value, &v); err != nil {
				return "", nil, fmt.Errorf("reporting: filter value: %w", err)
			}
		}
		return fmt.Sprintf("%s %s $%d", col, op, startArg), []any{v}, nil
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
