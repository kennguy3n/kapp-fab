// Command kapp-backup is the per-tenant export / restore tool.
//
// Kapp runs thousands of tenants inside a single PostgreSQL cluster
// behind Row-Level Security; a useful backup tool therefore has to
// scope every SELECT to `WHERE tenant_id = $1` and a useful restore
// tool has to accept an optional tenant-id remap (so an operator can
// restore tenant A's data into a freshly-provisioned tenant B without
// touching anyone else). Both flows live in this one binary.
//
//	kapp-backup extract --tenant <id> --out <file.jsonl>
//	kapp-backup restore --in <file.jsonl> [--remap <src>:<dst>]
//
// The export format is line-delimited JSON: one JSON object per row
// with a `_table` key so the restore side does not need to preserve
// ordering. A manifest record is emitted first so consumers can tell
// the schema version of the dump.
//
// Table coverage is defined by the TenantScopedTables table below;
// adding a tenant-scoped table to the schema requires adding it here
// as well. Keep the list in the order dependents must be inserted
// after their parents — both restore and extract walk the list in
// order.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantScopedTables is the authoritative list of tables the backup
// tool walks. Parents first; children (anything with a FK back to a
// row in the same dump) last. Partitioned tables use the parent name
// so PostgreSQL can route to the right partition on restore.
// Note: `ktypes` is intentionally NOT in this list. KTypes are the
// metadata schema and are re-registered at boot from Go code (see
// services/api/main.go), not per tenant. The table also has no
// `tenant_id` column, which would make the extract query fail with
// `column "tenant_id" does not exist` the moment we walked it.
var TenantScopedTables = []string{
	// Identity — required for a restored tenant to have any users or
	// custom roles. Listed first so FKs from the rest of the dump
	// resolve cleanly on insert.
	"user_tenants",
	"roles",
	"permissions",
	"sessions",
	// Platform
	"idempotency_keys",
	"saved_views",
	"notifications",
	// Metadata
	"krecords",
	"workflows",
	"workflow_runs",
	"approvals",
	"audit_log",
	"events",
	// Finance
	"accounts",
	"journal_entries",
	"journal_lines",
	"fiscal_periods",
	"tax_codes",
	"cost_centers",
	"bank_accounts",
	"bank_transactions",
	// Inventory
	"inventory_warehouses",
	"inventory_items",
	"inventory_batches",
	"inventory_moves",
	// HR / LMS
	"leave_ledger",
	"lesson_progress",
	// Files / Base / Docs / Forms / Imports
	"files",
	"base_tables",
	"base_rows",
	"docs_documents",
	"docs_document_versions",
	"forms",
	"import_jobs",
	"import_staging",
	// Phase I
	"exchange_rates",
	"sla_policies",
	"ticket_sla_log",
	"saved_reports",
	"scheduled_actions",
	"tenant_features",
	"tenant_usage",
	// Phase J
	"webhooks",
	"webhook_deliveries",
	"print_templates",
	"portal_users",
	// Phase J/K
	"tenant_support_domains",
	"data_retention_policies",
	"report_schedules",
	"export_jobs",
	// Phase L — Insights
	"insights_queries",
	"insights_dashboards",
	"insights_dashboard_widgets",
	"insights_query_cache",
	"insights_shares",
}

// manifest is the first record in every dump file.
type manifest struct {
	Type      string    `json:"_type"`
	Version   int       `json:"version"`
	TenantID  uuid.UUID `json:"tenant_id"`
	CreatedAt time.Time `json:"created_at"`
	Tables    []string  `json:"tables"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kapp-backup:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("subcommand required: extract | restore")
	}
	switch os.Args[1] {
	case "extract":
		return cmdExtract(os.Args[2:])
	case "restore":
		return cmdRestore(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kapp-backup extract --tenant <id> --out <file.jsonl>")
	fmt.Fprintln(os.Stderr, "       kapp-backup restore --in <file.jsonl> [--remap src:dst]")
	fmt.Fprintln(os.Stderr, "  $DATABASE_URL is read from the environment.")
}

func cmdExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	tenantRaw := fs.String("tenant", "", "tenant UUID to export (required)")
	out := fs.String("out", "-", "output path, '-' for stdout")
	_ = fs.Parse(args)
	if *tenantRaw == "" {
		return errors.New("--tenant required")
	}
	tenantID, err := uuid.Parse(*tenantRaw)
	if err != nil {
		return fmt.Errorf("invalid --tenant: %w", err)
	}
	ctx := context.Background()
	pool, err := poolFromEnv(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	w, close, err := openWriter(*out)
	if err != nil {
		return err
	}
	defer close()
	return extractTenant(ctx, pool, tenantID, w)
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	in := fs.String("in", "-", "input path, '-' for stdin")
	remapRaw := fs.String("remap", "", "optional src-uuid:dst-uuid tenant remap")
	_ = fs.Parse(args)
	var remap map[uuid.UUID]uuid.UUID
	if *remapRaw != "" {
		parts := strings.SplitN(*remapRaw, ":", 2)
		if len(parts) != 2 {
			return errors.New("--remap must be src-uuid:dst-uuid")
		}
		src, err := uuid.Parse(parts[0])
		if err != nil {
			return fmt.Errorf("invalid remap src: %w", err)
		}
		dst, err := uuid.Parse(parts[1])
		if err != nil {
			return fmt.Errorf("invalid remap dst: %w", err)
		}
		remap = map[uuid.UUID]uuid.UUID{src: dst}
	}
	ctx := context.Background()
	pool, err := poolFromEnv(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	r, close, err := openReader(*in)
	if err != nil {
		return err
	}
	defer close()
	return restoreTenant(ctx, pool, r, remap)
}

// extractTenant walks TenantScopedTables in order and writes one JSON
// object per row. Each row is augmented with `_table: <name>` so the
// restore path does not rely on insertion order. Text columns with
// JSONB content round-trip as raw JSON because pgx decodes JSONB into
// `[]byte`/`map[string]any` and json.Encoder handles both.
func extractTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, w io.Writer) error {
	enc := json.NewEncoder(w)
	if err := enc.Encode(manifest{
		Type:      "manifest",
		Version:   1,
		TenantID:  tenantID,
		CreatedAt: time.Now().UTC(),
		Tables:    TenantScopedTables,
	}); err != nil {
		return err
	}
	for _, table := range TenantScopedTables {
		count, err := exportTable(ctx, pool, tenantID, table, enc)
		if err != nil {
			return fmt.Errorf("export %s: %w", table, err)
		}
		fmt.Fprintf(os.Stderr, "kapp-backup: %s: %d rows\n", table, count)
	}
	return nil
}

func exportTable(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, table string, enc *json.Encoder) (int, error) {
	// We rely on row_to_json on the server so column lists don't need
	// to be hardcoded on the client — adding a column to the schema
	// surfaces automatically in the dump.
	rows, err := pool.Query(ctx,
		fmt.Sprintf(`SELECT row_to_json(t) FROM %s AS t WHERE tenant_id = $1`, quoteIdent(table)),
		tenantID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return count, err
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return count, err
		}
		obj["_table"] = table
		if err := enc.Encode(obj); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

// restoreTenant inserts every row back into its source table. When a
// remap is supplied the tenant_id column is rewritten before insert;
// IDs already in the DB hit the ON CONFLICT branch which replaces the
// existing row. The insert is all-or-nothing: a single transaction
// wraps every INSERT so a partial restore is never visible.
func restoreTenant(ctx context.Context, pool *pgxpool.Pool, r io.Reader, remap map[uuid.UUID]uuid.UUID) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if m.Type != "manifest" {
		return errors.New("missing manifest record")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := restoreRows(ctx, tx, dec, remap); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func restoreRows(ctx context.Context, tx pgx.Tx, dec *json.Decoder, remap map[uuid.UUID]uuid.UUID) error {
	for {
		obj := map[string]any{}
		err := dec.Decode(&obj)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode row: %w", err)
		}
		tableRaw, ok := obj["_table"]
		if !ok {
			return errors.New("row missing _table")
		}
		table, ok := tableRaw.(string)
		if !ok || !isKnownTable(table) {
			return fmt.Errorf("unknown _table %v", tableRaw)
		}
		delete(obj, "_table")
		// Apply remap before insert so the INSERT enforces RLS.
		if tenantRaw, ok := obj["tenant_id"].(string); ok {
			src, err := uuid.Parse(tenantRaw)
			if err != nil {
				return fmt.Errorf("row tenant_id not a uuid: %w", err)
			}
			if dst, ok := remap[src]; ok {
				obj["tenant_id"] = dst.String()
			}
		}
		if err := insertRow(ctx, tx, table, obj); err != nil {
			return fmt.Errorf("insert into %s: %w", table, err)
		}
	}
}

// tableConflictKeys maps tenant-scoped tables whose primary key is
// NOT the standard `(tenant_id, id)` to their actual PK column list.
// Tables not listed here fall back to `(tenant_id, id)` when the
// decoded row carries an `id` column, and to `ON CONFLICT DO NOTHING`
// otherwise. Adding a new tenant-scoped table with a non-standard PK
// requires a new entry here.
var tableConflictKeys = map[string][]string{
	"user_tenants":           {"user_id", "tenant_id"},
	"roles":                  {"tenant_id", "name"},
	"accounts":               {"tenant_id", "code"},
	"idempotency_keys":       {"tenant_id", "key"},
	"workflows":              {"tenant_id", "name", "version"},
	"fiscal_periods":         {"tenant_id", "period_start"},
	"tax_codes":              {"tenant_id", "code"},
	"cost_centers":           {"tenant_id", "code"},
	"docs_document_versions": {"tenant_id", "document_id", "version"},
	"lesson_progress":        {"tenant_id", "enrollment_id", "lesson_id"},
	"exchange_rates":         {"tenant_id", "from_currency", "to_currency", "rate_date"},
	"saved_reports":          {"tenant_id", "id"},
	"tenant_features":        {"tenant_id", "feature_key"},
	"tenant_usage":           {"tenant_id", "period_start", "metric"},
	// Phase J/K — tenant_support_domains uses (tenant_id, domain_lower)
	// via UNIQUE INDEX so the PK seen in the dump is a non-standard
	// natural key. data_retention_policies's PK is (tenant_id, category).
	"tenant_support_domains":  {"tenant_id", "domain"},
	"data_retention_policies": {"tenant_id", "category"},
	// insights_query_cache PK is (tenant_id, query_hash, filter_hash) and
	// insights_shares enforces a (tenant_id, resource_type, resource_id,
	// grantee_type, grantee) UNIQUE on top of the (tenant_id, id) PK.
	"insights_query_cache": {"tenant_id", "query_hash", "filter_hash"},
}

// insertRow issues a parameterised INSERT that lists the columns from
// the decoded row map. The conflict clause is picked per-table:
//
//   - tables in tableConflictKeys upsert on the declared PK;
//   - tables with an `id` column upsert on (tenant_id, id);
//   - anything else falls back to ON CONFLICT DO NOTHING.
//
// The SET list in the DO UPDATE branch covers every column supplied
// in the dump except the conflict keys themselves, so a second
// restore of a corrected dump overwrites the existing row rather
// than silently skipping it.
func insertRow(ctx context.Context, tx pgx.Tx, table string, obj map[string]any) error {
	cols := make([]string, 0, len(obj))
	for k := range obj {
		cols = append(cols, k)
	}
	// Sort so the generated statement is deterministic — easier on
	// tests and on logs that grep for the raw SQL.
	strSort(cols)
	placeholders := make([]string, len(cols))
	values := make([]any, len(cols))
	for i, c := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		values[i] = obj[c]
	}
	stmt := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) %s",
		quoteIdent(table),
		strings.Join(quoteIdents(cols), ", "),
		strings.Join(placeholders, ", "),
		conflictClause(table, cols),
	)
	_, err := tx.Exec(ctx, stmt, values...)
	return err
}

// conflictClause returns the ON CONFLICT fragment appended to the
// INSERT statement produced by insertRow. It is separated out so it
// can be unit-tested against the static PK map without touching a
// real database.
func conflictClause(table string, cols []string) string {
	key := resolveConflictKey(table, cols)
	if len(key) == 0 {
		return "ON CONFLICT DO NOTHING"
	}
	// Build the SET list: every supplied column that is not part of
	// the conflict key gets overwritten with the EXCLUDED value.
	setCols := make([]string, 0, len(cols))
	keySet := make(map[string]struct{}, len(key))
	for _, k := range key {
		keySet[k] = struct{}{}
	}
	for _, c := range cols {
		if _, isKey := keySet[c]; isKey {
			continue
		}
		setCols = append(setCols, fmt.Sprintf("%s = EXCLUDED.%s", quoteIdent(c), quoteIdent(c)))
	}
	if len(setCols) == 0 {
		// Every column is part of the PK — nothing to update.
		return fmt.Sprintf("ON CONFLICT (%s) DO NOTHING", strings.Join(quoteIdents(key), ", "))
	}
	return fmt.Sprintf(
		"ON CONFLICT (%s) DO UPDATE SET %s",
		strings.Join(quoteIdents(key), ", "),
		strings.Join(setCols, ", "),
	)
}

// resolveConflictKey picks the PK column list to use in the ON
// CONFLICT clause. Explicit entries in tableConflictKeys win; the
// fallback is `(tenant_id, id)` when both columns are present in the
// dump. Returns nil if no workable key is available — the caller
// uses the bare `ON CONFLICT DO NOTHING` form in that case.
func resolveConflictKey(table string, cols []string) []string {
	if key, ok := tableConflictKeys[table]; ok {
		return key
	}
	hasCol := func(name string) bool {
		for _, c := range cols {
			if c == name {
				return true
			}
		}
		return false
	}
	if hasCol("tenant_id") && hasCol("id") {
		return []string{"tenant_id", "id"}
	}
	return nil
}

// strSort is pulled out so callers don't need to import sort just
// for the one call site.
func strSort(s []string) { sort.Strings(s) }

// isKnownTable is a defence-in-depth check so a malicious dump cannot
// name an arbitrary table and trick restore into writing into it.
func isKnownTable(name string) bool {
	for _, t := range TenantScopedTables {
		if t == name {
			return true
		}
	}
	return false
}

func poolFromEnv(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	return pgxpool.New(ctx, dsn)
}

func openWriter(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

func openReader(path string) (io.Reader, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// quoteIdent wraps an identifier in double quotes after escaping any
// embedded double quotes. On the extract side the identifier list is
// trusted (TenantScopedTables + keys produced by row_to_json), but
// the restore path reads column names from an arbitrary JSON dump
// provided by the operator and the table list comes from the same
// dump's `_table` field. A crafted key like `id"); DROP TABLE …; --`
// must not escape the quoting context, so we always double-up any
// embedded quotes (the standard PostgreSQL identifier escape).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteIdents(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = quoteIdent(v)
	}
	return out
}
