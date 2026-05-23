package insights

import (
	"errors"
	"strings"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// makeRangeVar builds a *pg_query.RangeVar with the supplied
// catalog/schema/relation names. Used by the isSystemCatalog unit
// tests to exercise the classifier directly without going through
// the parser.
func makeRangeVar(catalog, schema, rel string) *pg_query.RangeVar {
	return &pg_query.RangeVar{
		Catalogname: catalog,
		Schemaname:  schema,
		Relname:     rel,
	}
}

// TestValidateRawSQLAcceptsValidSelect covers the happy path: a
// single SELECT against a user table is accepted. This is the
// fundamental editor use case, so the validator must not
// over-reject.
func TestValidateRawSQLAcceptsValidSelect(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"SELECT * FROM krecords",
		"SELECT id, ktype FROM krecords WHERE status = 'active' LIMIT 10",
		"SELECT count(*) AS n FROM journal_entries",
		"SELECT k.id, j.amount FROM krecords k JOIN journal_lines j ON j.record_id = k.id",
		"WITH active AS (SELECT * FROM krecords WHERE status = 'active') SELECT count(*) FROM active",
		"SELECT 1 UNION ALL SELECT 2",
		"VALUES (1), (2), (3)",
	}
	for _, body := range cases {
		t.Run(strings.SplitN(body, " ", 2)[0]+"_"+truncate(body, 32), func(t *testing.T) {
			if err := validateRawSQL(body); err != nil {
				t.Fatalf("validateRawSQL(%q) returned error %q; want nil", body, err)
			}
		})
	}
}

// TestValidateRawSQLAcceptsTrailingSemicolon documents the
// improvement over the previous textual check: a single statement
// with a trailing semicolon parses as one statement and is
// accepted. The old `strings.Contains(rawSQL, ";")` rejected this
// case for no good reason.
func TestValidateRawSQLAcceptsTrailingSemicolon(t *testing.T) {
	cases := []string{
		"SELECT 1;",
		"SELECT * FROM krecords;",
		"SELECT 1\n;\n",
		"SELECT 1 ; ", // trailing whitespace + semicolon
	}
	for _, body := range cases {
		if err := validateRawSQL(body); err != nil {
			t.Errorf("validateRawSQL(%q) returned error %q; want nil (trailing-semicolon should be accepted)", body, err)
		}
	}
}

// TestValidateRawSQLAcceptsSemicolonInStringLiteral is the
// signature improvement from moving to AST validation: a semicolon
// inside a single-quoted literal is part of the data, not a
// statement separator. The old textual check rejected this; the
// AST validator accepts it because pg_query reports one statement.
func TestValidateRawSQLAcceptsSemicolonInStringLiteral(t *testing.T) {
	cases := []string{
		"SELECT 'a;b'",
		"SELECT * FROM krecords WHERE data::text LIKE '%;%'",
		"SELECT E'first;second' AS s",
		"SELECT '/* not a comment ; */ ok' AS s",
	}
	for _, body := range cases {
		if err := validateRawSQL(body); err != nil {
			t.Errorf("validateRawSQL(%q) returned error %q; want nil (semicolon in literal should be accepted)", body, err)
		}
	}
}

// TestValidateRawSQLRejectsCommentHiddenInjection covers the loose
// side of the old textual check: a comment between the legitimate
// SELECT and a piggybacked DROP would slip past any naïve regex
// that strips C-style comments before checking for `;`. The AST
// validator counts statements after the parser has already
// resolved comments, so it catches this.
func TestValidateRawSQLRejectsCommentHiddenInjection(t *testing.T) {
	cases := []string{
		"SELECT 1 /* comment */ ; DROP TABLE krecords",
		"SELECT 1 -- inline\n; DROP TABLE krecords",
		"SELECT 1 /* a */ /* b */ ; DELETE FROM krecords",
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want multi-statement rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
	}
}

// TestValidateRawSQLErrorIsValidation ensures the wrapper preserves
// both sentinels. The HTTP layer maps ErrValidation to 400, and
// future call-sites might branch on ErrUnsafeSQL specifically; both
// must be reachable via errors.Is on the returned error.
func TestValidateRawSQLErrorIsValidation(t *testing.T) {
	err := validateRawSQL("DELETE FROM krecords")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("validateRawSQL error not Is ErrValidation: %v", err)
	}
	if !errors.Is(err, ErrUnsafeSQL) {
		t.Errorf("validateRawSQL error not Is ErrUnsafeSQL: %v", err)
	}
}

// TestValidateRawSQLRejectsParseFailure verifies pure garbage and
// syntactically-malformed SQL is surfaced as ErrUnsafeSQL with a
// "sql parse failed" prefix, rather than crashing or returning nil.
func TestValidateRawSQLRejectsParseFailure(t *testing.T) {
	cases := []string{
		"SELECT FROM",
		"FROM krecords",
		"SELECT * krecords",
		"))(",
		"SELECT 1 UNION", // missing right side
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want parse failure", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
	}
}

// TestValidateRawSQLRejectsBlank covers the leading-whitespace +
// empty cases — the validator does TrimSpace before parsing and
// returns the "raw_sql body required" message for all of them.
//
// The empty-body case is intentionally classified as "missing
// input" (ErrValidation only), NOT "unsafe SQL" (ErrUnsafeSQL).
// Tagging an empty body as ErrUnsafeSQL would conflate validation
// typos with attempted security-boundary violations and skew any
// monitoring that branches on errors.Is(err, ErrUnsafeSQL). The
// HTTP layer maps both sentinels to 400, so this is a semantic
// distinction with no behavioral impact on the API surface.
func TestValidateRawSQLRejectsBlank(t *testing.T) {
	cases := []string{"", "   ", "\n\n", "\t  \n\t"}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want blank rejection", body)
			continue
		}
		if !errors.Is(err, ErrValidation) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrValidation", body, err)
		}
		// Empty body is missing input, not unsafe SQL — the
		// ErrUnsafeSQL sentinel must NOT match.
		if errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; should NOT match ErrUnsafeSQL (empty body is missing input, not unsafe SQL)", body, err)
		}
		if !strings.Contains(err.Error(), "raw_sql body required") {
			t.Errorf("validateRawSQL(%q) error = %q; want 'raw_sql body required'", body, err)
		}
	}
}

// TestValidateRawSQLAcceptsAliasMatchingPgPrefix verifies the
// validator's prefix check is scoped to *table references*, not to
// column or alias names. A user can name a column or alias `pg_X`
// without tripping the system-catalog guard.
func TestValidateRawSQLAcceptsAliasMatchingPgPrefix(t *testing.T) {
	cases := []string{
		"SELECT id AS pg_id FROM krecords",
		"SELECT data->>'pg_name' AS pg_name FROM krecords",
		"SELECT 1 AS pg_constant",
	}
	for _, body := range cases {
		if err := validateRawSQL(body); err != nil {
			t.Errorf("validateRawSQL(%q) returned error %q; want nil (pg_ alias should be accepted)", body, err)
		}
	}
}

// TestIsSystemCatalogPositive covers the unit-level rule table for
// the schema/relname classifier. Documents the exact set of
// references the validator rejects so future maintainers can
// see the contract without re-reading the AST walker.
func TestIsSystemCatalogPositive(t *testing.T) {
	cases := []struct {
		catalog, schema, rel string
	}{
		{"", "pg_catalog", "pg_authid"},
		{"", "pg_catalog", "anything"}, // any rel under pg_catalog
		{"", "information_schema", "tables"},
		{"", "INFORMATION_SCHEMA", "tables"}, // case-insensitive
		{"", "PG_CATALOG", "pg_class"},
		{"", "", "pg_tables"},        // unqualified pg_-prefixed
		{"", "", "pg_stat_activity"}, // ditto
		{"db1", "public", "table"},   // catalog name set
	}
	for _, tc := range cases {
		rv := makeRangeVar(tc.catalog, tc.schema, tc.rel)
		if !isSystemCatalog(rv) {
			t.Errorf("isSystemCatalog(catalog=%q schema=%q rel=%q) = false; want true", tc.catalog, tc.schema, tc.rel)
		}
	}
}

// TestIsSystemCatalogNegative covers the references that must NOT
// trigger the system-catalog guard: ordinary tenant tables, plain
// names without pg_ prefix, and `public.foo` style references.
func TestIsSystemCatalogNegative(t *testing.T) {
	cases := []struct {
		catalog, schema, rel string
	}{
		{"", "", "krecords"},
		{"", "", "journal_entries"},
		{"", "public", "krecords"},
		{"", "app", "users"},
		{"", "", "payment_gateway"}, // name does not start with pg_
	}
	for _, tc := range cases {
		rv := makeRangeVar(tc.catalog, tc.schema, tc.rel)
		if isSystemCatalog(rv) {
			t.Errorf("isSystemCatalog(catalog=%q schema=%q rel=%q) = true; want false", tc.catalog, tc.schema, tc.rel)
		}
	}
}

// TestValidateRawSQLRejectsDataModifyingCTE pins the CTE-bypass
// rule (rule 5b in validateRawSQL). PostgreSQL allows
// INSERT/UPDATE/DELETE/MERGE inside `WITH` clauses; the validator
// rejects every one of them because the runner is meant to be a
// SELECT-only sandbox. The check is at the *walker* level — any
// nested *Node whose oneof field ends in `_stmt` other than
// `select_stmt` is rejected — so future statement nodes are
// caught without code changes.
func TestValidateRawSQLRejectsDataModifyingCTE(t *testing.T) {
	cases := []string{
		"WITH x AS (DELETE FROM krecords RETURNING *) SELECT * FROM x",
		"WITH x AS (UPDATE krecords SET status='archived' RETURNING *) SELECT count(*) FROM x",
		"WITH x AS (INSERT INTO krecords(id) VALUES (gen_random_uuid()) RETURNING id) SELECT * FROM x",
		"SELECT * FROM (WITH y AS (DELETE FROM krecords RETURNING id) SELECT id FROM y) t",
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want CTE-DML rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "nested non-SELECT statement") {
			t.Errorf("validateRawSQL(%q) error = %q; want nested-statement message", body, err)
		}
	}
}

// TestValidateRawSQLRejectsSystemFunction pins rule 5c. Function
// calls into pg_catalog or any pg_-prefixed name are blocked
// regardless of qualification, schema casing, or whether the call
// appears in the SELECT list, WHERE clause, or FROM-as-function.
func TestValidateRawSQLRejectsSystemFunction(t *testing.T) {
	cases := []string{
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT pg_read_binary_file('/etc/passwd')",
		"SELECT pg_ls_dir('/')",
		"SELECT * FROM pg_ls_dir('/')",
		"SELECT pg_catalog.pg_read_file('/etc/passwd')",
		"SELECT 1 WHERE pg_backend_pid() > 0",
		"SELECT 1 FROM (SELECT pg_read_file('/etc/passwd') AS x) t",
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want system-function rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "system function") {
			t.Errorf("validateRawSQL(%q) error = %q; want system-function message", body, err)
		}
	}
}

// TestValidateRawSQLRejectsSelectInto pins the SELECT INTO leg of
// rule 4. `SELECT … INTO newtable FROM …` parses as a SelectStmt
// with a non-nil IntoClause and is functionally DDL (creates a
// table from the result set). The READ ONLY tx is the backstop,
// but rule 4 must reject this at the AST layer so the validator's
// docstring promise of "only SELECT" matches reality.
func TestValidateRawSQLRejectsSelectInto(t *testing.T) {
	cases := []string{
		"SELECT * INTO newtable FROM krecords",
		"SELECT id, name INTO TEMP scratch FROM krecords",
		"SELECT id INTO UNLOGGED bulk FROM krecords WHERE status = 'active'",
		"SELECT 1 INTO newtable",
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want SELECT-INTO rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "SELECT INTO is not allowed") {
			t.Errorf("validateRawSQL(%q) error = %q; want SELECT-INTO message", body, err)
		}
	}
}

// TestValidateRawSQLRejectsSetConfig pins the set_config leg of
// the dangerous-functions denylist. set_config(name, value,
// is_local) can change `app.tenant_id` (which the RLS policy
// reads), so it would break tenant isolation if allowed. RLS qual
// evaluation order is plan-dependent; fail closed at the AST
// layer regardless.
//
// set_config is a built-in PostgreSQL function (it lives in
// pg_catalog, just without the `pg_` prefix), so the user-facing
// message uses the "system function" wording. Both the bare
// `set_config(…)` form (matched via the denylist) and the
// `pg_catalog.set_config(…)` form (matched via the schema check)
// produce the same classification, so callers see a consistent
// error category regardless of how they qualified the call.
func TestValidateRawSQLRejectsSetConfig(t *testing.T) {
	cases := []string{
		"SELECT set_config('app.tenant_id', '00000000-0000-0000-0000-000000000000', true)",
		"SELECT set_config('app.tenant_id', 'victim', true), * FROM krecords",
		"SELECT SET_CONFIG('app.tenant_id', 'x', true)",
		"SELECT public.set_config('app.tenant_id', 'x', true)",
		// pg_catalog-qualified form must produce the same
		// system-function classification as the bare form.
		"SELECT pg_catalog.set_config('app.tenant_id', 'x', true)",
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want set_config rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "system function") {
			t.Errorf("validateRawSQL(%q) error = %q; want system-function message", body, err)
		}
	}
}

// TestValidateRawSQLRejectsSchemaQualifiedPg pins the fail-closed
// defense-in-depth for 2-part `<schema>.pg_*` function references.
// A DBA-defined wrapper `public.pg_read_file(text)` (e.g. a
// SECURITY DEFINER stub for admin tooling) would otherwise be
// callable from the editor; treating any `pg_`-prefixed leaf as
// system regardless of schema closes that gap with no false
// positives on tenant code (the convention `pg_*` is reserved for
// Postgres-built-ins by policy).
func TestValidateRawSQLRejectsSchemaQualifiedPg(t *testing.T) {
	cases := []string{
		"SELECT public.pg_read_file('/etc/passwd')",
		"SELECT public.pg_ls_dir('/')",
		"SELECT app.pg_backend_pid()",
		"SELECT PUBLIC.PG_READ_FILE('/x')", // case-insensitive
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want schema-qualified-pg rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "system function") {
			t.Errorf("validateRawSQL(%q) error = %q; want system-function message", body, err)
		}
	}
}

// TestValidateRawSQLRejectsDangerousExtensionFunction pins rule 5c
// for the extension-function leg of the function denylist. dblink
// opens a new connection that bypasses RLS / READ ONLY /
// statement_timeout from the outer tx — it requires CREATE
// EXTENSION dblink to install, so it is genuinely an extension
// function and the user-facing message says so ("disallowed
// extension function"). Large-object I/O (lo_import / lo_export)
// is covered by TestValidateRawSQLRejectsBuiltinDenylist below
// since those are built-in functions, not extension functions.
func TestValidateRawSQLRejectsDangerousExtensionFunction(t *testing.T) {
	cases := []string{
		"SELECT dblink('dbname=other', 'SELECT * FROM krecords')",
		"SELECT dblink_exec('dbname=other', 'DELETE FROM krecords')",
		"SELECT dblink_connect('dbname=other')",
		"SELECT 1 FROM (SELECT dblink('dbname=other', 'x') AS y) t",
		"SELECT public.dblink('dbname=other', 'SELECT 1')",
		"SELECT DBLINK('dbname=other', 'SELECT 1')",
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want extension-function rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "disallowed extension function") {
			t.Errorf("validateRawSQL(%q) error = %q; want disallowed-extension-function message", body, err)
		}
	}
}

// TestValidateRawSQLRejectsBuiltinDenylist pins the built-in
// (non-pg_-prefixed) leg of the function denylist. lo_import and
// lo_export are large-object I/O functions that ship with the
// PostgreSQL core distribution (they live in pg_catalog despite
// the lo_ prefix). Their leaf names happen not to start with
// `pg_`, so the prefix check doesn't catch them — the explicit
// denylist must reject them, and the error message must use the
// "system function" wording to accurately reflect that they are
// built-ins rather than from an extension. Compare with
// TestValidateRawSQLRejectsDangerousExtensionFunction (dblink) for
// the extension-classification counterpart.
func TestValidateRawSQLRejectsBuiltinDenylist(t *testing.T) {
	cases := []string{
		"SELECT lo_import('/etc/passwd')",
		"SELECT lo_export(1, '/tmp/leak')",
		"SELECT LO_IMPORT('/x')",          // case-insensitive
		"SELECT public.lo_import('/x')",   // 2-part qualified leaf
		"SELECT pg_catalog.lo_import('/x')", // pg_catalog-qualified
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want built-in denylist rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "system function") {
			t.Errorf("validateRawSQL(%q) error = %q; want system-function message (lo_* are built-ins, not extensions)", body, err)
		}
	}
}

// TestValidateRawSQLRejectsSchemaQualifiedPgTable pins the
// fail-closed defense-in-depth for 2-part `<schema>.pg_*` table
// references. A DBA-created view `public.pg_authid` that wraps
// `pg_catalog.pg_authid` would otherwise bypass the validator,
// since `public` is not in the explicit system-schema list. Same
// fail-closed posture as the function-call path's
// pg_-prefixed-leaf check (TestValidateRawSQLRejectsSchemaQualifiedPg).
func TestValidateRawSQLRejectsSchemaQualifiedPgTable(t *testing.T) {
	cases := []string{
		"SELECT * FROM public.pg_authid",
		"SELECT * FROM public.pg_stat_activity",
		"SELECT * FROM myschema.pg_stat_user_tables",
		"SELECT * FROM PUBLIC.PG_AUTHID", // case-insensitive
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want schema-qualified-pg-table rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "system catalog") {
			t.Errorf("validateRawSQL(%q) error = %q; want system-catalog message", body, err)
		}
	}
}

// TestValidateRawSQLRejectsForUpdate pins the row-locking-clause
// rejection. SELECT … FOR UPDATE / SHARE / NO KEY UPDATE / KEY
// SHARE all parse as SelectStmt with a non-nil LockingClause and
// would otherwise reach the per-tenant tx, where Postgres would
// surface a less-friendly "cannot execute SELECT FOR UPDATE in a
// read-only transaction" runtime error. Surfacing the rejection at
// the AST layer keeps the validator as the single source of truth
// for the editor surface's accepted shapes, with READ ONLY tx as
// defense in depth.
func TestValidateRawSQLRejectsForUpdate(t *testing.T) {
	cases := []string{
		"SELECT * FROM krecords FOR UPDATE",
		"SELECT * FROM krecords FOR SHARE",
		"SELECT * FROM krecords FOR NO KEY UPDATE",
		"SELECT * FROM krecords FOR KEY SHARE",
		"SELECT * FROM krecords FOR UPDATE NOWAIT",
		"SELECT * FROM krecords FOR UPDATE SKIP LOCKED",
		"SELECT id FROM krecords WHERE tenant_id = $1 FOR UPDATE",
	}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want FOR UPDATE rejection", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("validateRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err)
		}
		if !strings.Contains(err.Error(), "FOR UPDATE") {
			t.Errorf("validateRawSQL(%q) error = %q; want FOR-UPDATE message", body, err)
		}
	}
}

// TestValidateRawSQLAcceptsUserFunction documents that the
// function-call rule is scoped to pg_-prefixed / pg_catalog /
// information_schema names. Normal user-callable SQL functions
// like count, length, lower, jsonb_extract_path_text, and even
// `now()` (no pg_ prefix) must be accepted.
func TestValidateRawSQLAcceptsUserFunction(t *testing.T) {
	cases := []string{
		"SELECT count(*) FROM krecords",
		"SELECT length(name) FROM krecords",
		"SELECT lower(ktype) FROM krecords",
		"SELECT now()",
		"SELECT current_timestamp",
		"SELECT jsonb_extract_path_text(data, 'name') FROM krecords",
		"SELECT public.my_user_function(id) FROM krecords",
	}
	for _, body := range cases {
		if err := validateRawSQL(body); err != nil {
			t.Errorf("validateRawSQL(%q) returned error %q; want nil (user function should be accepted)", body, err)
		}
	}
}

// TestIsSystemFunctionPositive covers the unit-level rule table for
// the function-name classifier. Mirrors TestIsSystemCatalogPositive
// and pins both the (name, kind) return tuple and the system/extension
// categorisation that the runner uses to format the user-facing error.
func TestIsSystemFunctionPositive(t *testing.T) {
	cases := []struct {
		parts []string
		kind  funcKind
	}{
		{[]string{"pg_read_file"}, funcKindSystem},
		{[]string{"pg_ls_dir"}, funcKindSystem},
		{[]string{"PG_BACKEND_PID"}, funcKindSystem}, // case-insensitive on the prefix check
		{[]string{"pg_catalog", "pg_read_file"}, funcKindSystem},
		{[]string{"PG_CATALOG", "pg_read_file"}, funcKindSystem},
		{[]string{"information_schema", "_pg_truetypid"}, funcKindSystem},
		{[]string{"db1", "public", "fn"}, funcKindSystem}, // 3-part = cross-database, rejected outright
		// 2-part with non-system schema but pg_-prefixed leaf:
		// fail-closed against hostile `public.pg_read_file` wrapper.
		{[]string{"public", "pg_read_file"}, funcKindSystem},
		{[]string{"PUBLIC", "PG_READ_FILE"}, funcKindSystem},
		{[]string{"app", "pg_backend_pid"}, funcKindSystem},
		// set_config is a built-in PostgreSQL function (lives in
		// pg_catalog, just without the `pg_` prefix). Both the
		// bare form and the public-schema form must produce
		// funcKindSystem so the user-facing message is consistent
		// regardless of qualification — same as how the
		// pg_catalog.set_config form is classified via the schema
		// check at the top of isSystemFunction.
		{[]string{"set_config"}, funcKindSystem},
		{[]string{"SET_CONFIG"}, funcKindSystem},
		{[]string{"public", "set_config"}, funcKindSystem},
		// Large-object I/O is also built-in (not from an extension)
		// despite the lo_ prefix that suggests otherwise.
		// Classification follows the dangerousFunctions map.
		{[]string{"lo_import"}, funcKindSystem},
		{[]string{"lo_export"}, funcKindSystem},
		{[]string{"public", "lo_import"}, funcKindSystem},
		// dblink_* genuinely is an extension (requires CREATE
		// EXTENSION dblink to install), so funcKindExtension is
		// the correct classification and the runner produces the
		// "disallowed extension function" error message.
		{[]string{"dblink"}, funcKindExtension},
		{[]string{"DBLINK"}, funcKindExtension}, // case-insensitive leaf
		{[]string{"dblink_exec"}, funcKindExtension},
		{[]string{"dblink_send_query"}, funcKindExtension},
		{[]string{"public", "dblink"}, funcKindExtension},
	}
	for _, tc := range cases {
		fc := makeFuncCall(tc.parts...)
		ref, kind, ok := isSystemFunction(fc)
		if !ok {
			t.Errorf("isSystemFunction(%v) = (%q, %v, false); want true", tc.parts, ref, kind)
			continue
		}
		if kind != tc.kind {
			t.Errorf("isSystemFunction(%v) kind = %v; want %v (ref=%q)", tc.parts, kind, tc.kind, ref)
		}
	}
}

// TestIsSystemFunctionNegative documents the references that must
// NOT trigger the function guard: any unqualified non-`pg_` name
// outside the dangerous-extension denylist, or a schema-qualified
// name in `public` / any other tenant schema whose leaf isn't on
// the denylist.
func TestIsSystemFunctionNegative(t *testing.T) {
	cases := [][]string{
		{"count"},
		{"length"},
		{"now"},
		{"public", "my_function"},
		{"app", "compute_total"},
		// Names that share a prefix with a denylisted name but are
		// not themselves on the denylist must be accepted: only exact
		// leaf-name matches block.
		{"dblink_helper"}, // not a real dblink function, allow
		{"lower"},         // shares no chars with lo_import beyond `l`
	}
	for _, parts := range cases {
		fc := makeFuncCall(parts...)
		if ref, kind, ok := isSystemFunction(fc); ok {
			t.Errorf("isSystemFunction(%v) = (%q, %v, true); want false", parts, ref, kind)
		}
	}
}

// makeFuncCall builds a *pg_query.FuncCall with the supplied dotted
// name parts, wrapped in String nodes the same way the parser
// emits them. Used by the isSystemFunction unit tests to exercise
// the classifier directly without going through the parser.
func makeFuncCall(parts ...string) *pg_query.FuncCall {
	nodes := make([]*pg_query.Node, 0, len(parts))
	for _, p := range parts {
		nodes = append(nodes, &pg_query.Node{
			Node: &pg_query.Node_String_{
				String_: &pg_query.String{Sval: p},
			},
		})
	}
	return &pg_query.FuncCall{Funcname: nodes}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
