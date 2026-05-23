package insights

// Attack-vector suite for validateRawSQL.  This complements the
// shape-specific unit tests in sqlvalidate_test.go (which exercise
// each rule directly, including via raw AST construction) with a
// breadth-first table of real SQL payloads an attacker might send
// to the insights editor.  Every entry must either:
//
//   - parse, run through validateRawSQL, and be REJECTED with an
//     ErrUnsafeSQL-tagged error containing the expected substring,
//     OR
//   - parse, run through validateRawSQL, and be ACCEPTED.
//
// We do not assert exact error wording (so the validator can refine
// messages without churning the test) — only that the rejection
// reason names the rule that should have caught it.  This is the
// only test file that exercises the validator against full-text
// SQL strings end-to-end across every category, so adding a new
// rule to validateRawSQL should be accompanied by a new case here.
//
// Categories covered:
//
//   - Statement-shape violations (multi-statement, DML/DDL/utility
//     at the root, comment-hidden semicolons).
//   - System catalog access (pg_catalog.*, information_schema.*,
//     unqualified pg_*, three-part names, view-shadowed pg_*).
//   - System & dangerous-extension function calls (pg_read_file,
//     dblink, set_config, lo_import/lo_export, pg_stat_*).
//   - DML / DDL / utility hiding inside CTEs (INSERT/UPDATE/
//     DELETE/MERGE).
//   - Row locking (SELECT FOR UPDATE/SHARE/NO KEY UPDATE/KEY
//     SHARE) — root and nested.
//   - SELECT INTO at the root and inside CTE bodies.
//   - Transaction control / session GUC mutation at the root
//     (BEGIN/COMMIT/SET/RESET/SHOW).
//   - Procedure calls (CALL …) and PREPARE/EXECUTE/DEALLOCATE.
//   - Listen/notify, lock table, copy, vacuum, analyze,
//     reindex, security-label, grant/revoke.
//   - Positive-control SELECT shapes that MUST continue to be
//     accepted: simple SELECT, joins, set ops, CTEs over user
//     tables, lateral, subqueries, window functions, JSON
//     functions, EXPLAIN of a SELECT, casts to pg_catalog
//     types (benign — TypeName not RangeVar/FuncCall).

import (
	"errors"
	"strings"
	"testing"
)

// attackCase is a single row in the attack-vector table.
//
//   - name: t.Run subtest label (kebab-case, describes the attack).
//   - sql: payload submitted to validateRawSQL.
//   - wantReject: true => validateRawSQL must return an error;
//     false => validateRawSQL must return nil.
//   - wantSubstr: when wantReject is true, the returned error's
//     string MUST contain this substring (case-sensitive) so we
//     can confirm the RIGHT rule fired, not just any rule.  Empty
//     wantSubstr disables the substring check.
type attackCase struct {
	name       string
	sql        string
	wantReject bool
	wantSubstr string
}

// TestValidateRawSQLAttackVectors is the consolidated attack-vector
// suite.  Each entry exercises a distinct rule path through
// validateRawSQL, end-to-end via the parser.
func TestValidateRawSQLAttackVectors(t *testing.T) {
	cases := []attackCase{
		// -----------------------------------------------------
		// (A) Multi-statement / statement-shape violations.
		// -----------------------------------------------------
		{
			name:       "trailing-drop-after-semicolon",
			sql:        "SELECT 1; DROP TABLE krecords",
			wantReject: true,
			wantSubstr: "multi-statement",
		},
		{
			name:       "comment-hidden-drop",
			sql:        "SELECT 1 /* harmless */ ; DROP TABLE krecords",
			wantReject: true,
			wantSubstr: "multi-statement",
		},
		{
			name:       "line-comment-hidden-drop",
			sql:        "SELECT 1 -- innocent\n; DROP TABLE krecords",
			wantReject: true,
			wantSubstr: "multi-statement",
		},
		{
			name:       "three-statements",
			sql:        "SELECT 1; SELECT 2; SELECT 3",
			wantReject: true,
			wantSubstr: "multi-statement",
		},
		{
			name:       "blank-body",
			sql:        "   ",
			wantReject: true,
			wantSubstr: "raw_sql body required",
		},
		{
			name:       "parse-failure-junk",
			sql:        "SELECT FROM WHERE",
			wantReject: true,
			wantSubstr: "parse",
		},

		// -----------------------------------------------------
		// (B) Non-SELECT at root.
		//
		// EXPLAIN is also classified non-SELECT by the
		// validator's "only SELECT" rule even though `EXPLAIN
		// SELECT …` is read-only in practice — the validator
		// errs on the side of refusing utility statements so
		// EXPLAIN ANALYZE on a DML body cannot slip through.
		// -----------------------------------------------------
		{
			name:       "root-explain-select",
			sql:        "EXPLAIN SELECT * FROM krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-insert",
			sql:        "INSERT INTO krecords (id) VALUES ('00000000-0000-0000-0000-000000000000')",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-update",
			sql:        "UPDATE krecords SET id = id",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-delete",
			sql:        "DELETE FROM krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-merge",
			sql:        "MERGE INTO krecords k USING krecords s ON k.id = s.id WHEN MATCHED THEN DO NOTHING",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-truncate",
			sql:        "TRUNCATE krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-copy-from",
			sql:        "COPY krecords FROM '/tmp/evil.csv'",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-copy-to",
			sql:        "COPY (SELECT * FROM krecords) TO '/tmp/leak.csv'",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-create-table",
			sql:        "CREATE TABLE x (id int)",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-drop-table",
			sql:        "DROP TABLE krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-alter-table",
			sql:        "ALTER TABLE krecords ADD COLUMN evil text",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-create-role",
			sql:        "CREATE ROLE attacker LOGIN PASSWORD 'pwn'",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-grant",
			sql:        "GRANT ALL ON krecords TO public",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-revoke",
			sql:        "REVOKE ALL ON krecords FROM kapp_app",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-call-procedure",
			sql:        "CALL evil_proc()",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-set-session-guc",
			sql:        "SET row_security = off",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-set-local-guc",
			sql:        "SET LOCAL app.tenant_id = '00000000-0000-0000-0000-000000000000'",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-reset-all",
			sql:        "RESET ALL",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-show",
			sql:        "SHOW row_security",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-begin",
			sql:        "BEGIN",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-commit",
			sql:        "COMMIT",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-rollback",
			sql:        "ROLLBACK",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-prepare",
			sql:        "PREPARE evil AS SELECT 1",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-execute",
			sql:        "EXECUTE evil",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-deallocate",
			sql:        "DEALLOCATE evil",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-listen",
			sql:        "LISTEN tenants",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-notify",
			sql:        "NOTIFY tenants",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-lock-table",
			sql:        "LOCK TABLE krecords IN ACCESS EXCLUSIVE MODE",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-vacuum",
			sql:        "VACUUM krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-analyze",
			sql:        "ANALYZE krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-reindex",
			sql:        "REINDEX TABLE krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},
		{
			name:       "root-explain-analyze",
			sql:        "EXPLAIN ANALYZE DELETE FROM krecords",
			wantReject: true,
			wantSubstr: "only SELECT",
		},

		// -----------------------------------------------------
		// (C) System catalog access (RangeVar).
		// -----------------------------------------------------
		{
			name:       "unqualified-pg-authid",
			sql:        "SELECT * FROM pg_authid",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "pg-catalog-pg-authid",
			sql:        "SELECT * FROM pg_catalog.pg_authid",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "information-schema-tables",
			sql:        "SELECT * FROM information_schema.tables",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "three-part-cross-database",
			sql:        "SELECT * FROM template1.pg_catalog.pg_authid",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "user-view-shadow-of-pg-table",
			sql:        "SELECT * FROM public.pg_authid",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "pg-class-via-subquery",
			sql:        "SELECT count(*) FROM (SELECT 1 FROM pg_class) s",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "pg-stat-activity-cte",
			sql:        "WITH a AS (SELECT * FROM pg_stat_activity) SELECT * FROM a",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "pg-user-mappings-set-op",
			sql:        "SELECT 1 UNION ALL SELECT 1 FROM pg_user_mappings",
			wantReject: true,
			wantSubstr: "system catalog",
		},
		{
			name:       "lateral-pg-tables",
			sql:        "SELECT * FROM krecords k, LATERAL (SELECT * FROM pg_tables) p",
			wantReject: true,
			wantSubstr: "system catalog",
		},

		// -----------------------------------------------------
		// (D) System / dangerous extension function calls.
		// -----------------------------------------------------
		{
			name:       "pg-read-file",
			sql:        "SELECT pg_read_file('/etc/passwd')",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "pg-catalog-pg-read-file",
			sql:        "SELECT pg_catalog.pg_read_file('/etc/passwd')",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "pg-ls-dir",
			sql:        "SELECT pg_ls_dir('/')",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "pg-stat-get-activity",
			sql:        "SELECT pg_stat_get_activity(NULL)",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "dblink",
			sql:        "SELECT * FROM dblink('host=evil','SELECT * FROM pg_authid') AS t(rolname text)",
			wantReject: true,
			wantSubstr: "extension function",
		},
		{
			name:       "dblink-connect",
			sql:        "SELECT dblink_connect('myconn','host=evil')",
			wantReject: true,
			wantSubstr: "extension function",
		},
		{
			name:       "lo-import",
			sql:        "SELECT lo_import('/etc/passwd')",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "lo-export",
			sql:        "SELECT lo_export(0, '/tmp/leak')",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "set-config-tenant-swap",
			sql:        "SELECT set_config('app.tenant_id', '00000000-0000-0000-0000-000000000000', true)",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "pg-catalog-set-config",
			sql:        "SELECT pg_catalog.set_config('row_security','off',true)",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "set-config-hidden-in-cte",
			sql:        "WITH evil AS (SELECT set_config('app.tenant_id','x',true) AS x) SELECT * FROM evil",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "pg-read-file-in-target-list",
			sql:        "SELECT id, pg_read_file('/etc/passwd') FROM krecords",
			wantReject: true,
			wantSubstr: "system function",
		},
		{
			name:       "pg-read-file-in-where-subquery",
			sql:        "SELECT * FROM krecords WHERE id::text = (SELECT pg_read_file('/etc/passwd'))",
			wantReject: true,
			wantSubstr: "system function",
		},

		// -----------------------------------------------------
		// (E) DML/DDL hidden inside CTE bodies.
		// -----------------------------------------------------
		{
			name:       "cte-insert",
			sql:        "WITH x AS (INSERT INTO krecords (id) VALUES ('00000000-0000-0000-0000-000000000000') RETURNING *) SELECT * FROM x",
			wantReject: true,
			wantSubstr: "nested non-SELECT",
		},
		{
			name:       "cte-update",
			sql:        "WITH x AS (UPDATE krecords SET id = id RETURNING *) SELECT * FROM x",
			wantReject: true,
			wantSubstr: "nested non-SELECT",
		},
		{
			name:       "cte-delete",
			sql:        "WITH x AS (DELETE FROM krecords RETURNING *) SELECT * FROM x",
			wantReject: true,
			wantSubstr: "nested non-SELECT",
		},
		{
			// MERGE inside CTE: PG itself rejects the RETURNING
			// clause syntax (MERGE returning is post-15 and the
			// parser still trips on this shape).  Drop RETURNING
			// so the parse succeeds and the walker can classify
			// the nested MergeStmt.
			name:       "cte-merge",
			sql:        "WITH x AS (MERGE INTO krecords k USING krecords s ON k.id = s.id WHEN MATCHED THEN DO NOTHING) SELECT 1",
			wantReject: true,
			wantSubstr: "nested non-SELECT",
		},

		// -----------------------------------------------------
		// (F) Row locking (root & nested).
		// -----------------------------------------------------
		{
			name:       "root-select-for-update",
			sql:        "SELECT * FROM krecords FOR UPDATE",
			wantReject: true,
			wantSubstr: "row locking",
		},
		{
			name:       "root-select-for-share",
			sql:        "SELECT * FROM krecords FOR SHARE",
			wantReject: true,
			wantSubstr: "row locking",
		},
		{
			name:       "root-select-for-no-key-update",
			sql:        "SELECT * FROM krecords FOR NO KEY UPDATE",
			wantReject: true,
			wantSubstr: "row locking",
		},
		{
			name:       "nested-for-update-in-subquery",
			sql:        "SELECT * FROM (SELECT * FROM krecords FOR UPDATE) s",
			wantReject: true,
			wantSubstr: "row locking",
		},
		{
			name:       "cte-for-update",
			sql:        "WITH x AS (SELECT * FROM krecords FOR UPDATE) SELECT * FROM x",
			wantReject: true,
			wantSubstr: "row locking",
		},

		// -----------------------------------------------------
		// (G) SELECT INTO (root & nested).
		// -----------------------------------------------------
		{
			name:       "root-select-into",
			sql:        "SELECT * INTO new_tbl FROM krecords",
			wantReject: true,
			wantSubstr: "SELECT INTO",
		},
		{
			name:       "cte-select-into",
			sql:        "WITH x AS (SELECT * INTO new_tbl FROM krecords) SELECT 1",
			wantReject: true,
			wantSubstr: "SELECT INTO",
		},

		// -----------------------------------------------------
		// (H) Positive controls — MUST be accepted.
		// -----------------------------------------------------
		{name: "ok-select-literal", sql: "SELECT 1", wantReject: false},
		{name: "ok-select-user-table", sql: "SELECT id, name FROM krecords ORDER BY name LIMIT 10", wantReject: false},
		{name: "ok-trailing-semicolon", sql: "SELECT 1;", wantReject: false},
		{name: "ok-string-literal-semicolon", sql: "SELECT 'a;b' AS s", wantReject: false},
		{name: "ok-join-user-tables", sql: "SELECT k.id, kv.payload FROM krecords k JOIN krecord_versions kv ON kv.krecord_id = k.id", wantReject: false},
		{name: "ok-union", sql: "SELECT 1 AS x UNION ALL SELECT 2", wantReject: false},
		{name: "ok-cte-user-table", sql: "WITH x AS (SELECT id FROM krecords) SELECT count(*) FROM x", wantReject: false},
		{name: "ok-lateral-user-table", sql: "SELECT k.id FROM krecords k, LATERAL (SELECT 1) s", wantReject: false},
		{name: "ok-window-function", sql: "SELECT id, row_number() OVER (ORDER BY id) FROM krecords", wantReject: false},
		{name: "ok-json-function", sql: "SELECT jsonb_build_object('a', 1)", wantReject: false},
		{name: "ok-current-setting-read", sql: "SELECT current_setting('app.tenant_id', true)", wantReject: false},
		{name: "ok-pg-catalog-cast", sql: "SELECT '1'::pg_catalog.int4", wantReject: false},
		{name: "ok-line-comment-inside", sql: "SELECT 1 -- ok\nFROM krecords", wantReject: false},
		{name: "ok-block-comment-inside", sql: "SELECT 1 /* ok */ FROM krecords", wantReject: false},
		{name: "ok-alias-prefixed-pg", sql: "SELECT pg.id FROM krecords pg", wantReject: false},
		{name: "ok-values", sql: "VALUES (1), (2), (3)", wantReject: false},
		{name: "ok-recursive-cte", sql: "WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 5) SELECT n FROM r", wantReject: false},
	}

	if len(cases) < 50 {
		t.Fatalf("attack-vector suite must contain at least 50 cases, got %d (the suite is the documented contract for validateRawSQL coverage)", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateRawSQL(tc.sql)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("expected rejection for %q, got nil error", tc.sql)
				}
				if !errors.Is(err, ErrUnsafeSQL) && !errors.Is(err, ErrValidation) {
					t.Fatalf("expected ErrUnsafeSQL or ErrValidation, got %T: %v", err, err)
				}
				if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Fatalf("expected error to contain %q, got %q", tc.wantSubstr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("expected acceptance for %q, got error %v", tc.sql, err)
			}
		})
	}
}
