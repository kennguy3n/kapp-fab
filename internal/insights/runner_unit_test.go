package insights

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestRunRawSQLRejectsMultiStatement verifies the documented guard
// against semicolon-separated SQL bodies. pgx.Query silently
// executes only the first statement, so this guard turns a silent
// drop into a 400 the caller can act on.
//
// The validator parses the body, so "SELECT 1;" (single statement
// with trailing terminator) is NOT a multi-statement body — pg_query
// recognises a single statement. That used to be rejected by the
// textual `strings.Contains(rawSQL, ";")` check; the AST-based
// validator is more precise. See TestValidateRawSQLAcceptsTrailingSemicolon
// in sqlvalidate_test.go.
func TestRunRawSQLRejectsMultiStatement(t *testing.T) {
	r := &Runner{}
	cases := []string{
		"SELECT 1; SELECT 2",                            // two SELECTs
		"SELECT 1; DROP TABLE foo",                      // SELECT then DDL
		"SELECT * FROM employees; SELECT * FROM users;", // two real reads
	}
	for _, body := range cases {
		_, err := r.RunRawSQL(context.Background(), uuid.New(), body, nil)
		if err == nil {
			t.Errorf("RunRawSQL(%q) returned nil error; want validation failure", body)
			continue
		}
		if !errors.Is(err, ErrUnsafeSQL) {
			t.Errorf("RunRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err.Error())
			continue
		}
		if !strings.Contains(err.Error(), "multi-statement") {
			t.Errorf("RunRawSQL(%q) error = %q; want multi-statement message", body, err.Error())
		}
	}
}

// TestRunRawSQLRejectsNonSelect verifies the validator's SELECT-only
// rule fires before pgx ever sees the body. The
// `SET TRANSACTION READ ONLY` guard inside the per-tx callback is
// still in place as defense-in-depth, but we want a clean validator
// error rather than a Postgres runtime error so the API surfaces a
// 400 with a meaningful message.
func TestRunRawSQLRejectsNonSelect(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name string
		body string
	}{
		{"insert", "INSERT INTO employees(name) VALUES ('alice')"},
		{"update", "UPDATE employees SET name = 'bob' WHERE id = 1"},
		{"delete", "DELETE FROM employees WHERE id = 1"},
		{"create_table", "CREATE TABLE x (id int)"},
		{"drop_table", "DROP TABLE employees"},
		{"alter_table", "ALTER TABLE employees ADD COLUMN salary numeric"},
		{"begin", "BEGIN"},
		{"set", "SET TIME ZONE 'UTC'"},
		{"copy", "COPY employees TO STDOUT"},
		{"truncate", "TRUNCATE employees"},
		{"grant", "GRANT SELECT ON employees TO public"},
		{"vacuum", "VACUUM employees"},
		{"explain", "EXPLAIN SELECT * FROM krecords"},
		{"explain_analyze", "EXPLAIN ANALYZE SELECT * FROM krecords"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.RunRawSQL(context.Background(), uuid.New(), tc.body, nil)
			if err == nil {
				t.Fatalf("RunRawSQL(%q) returned nil error; want non-SELECT rejection", tc.body)
			}
			if !errors.Is(err, ErrUnsafeSQL) {
				t.Fatalf("RunRawSQL(%q) error = %q; want ErrUnsafeSQL", tc.body, err.Error())
			}
			if !strings.Contains(err.Error(), "only SELECT is permitted") {
				t.Fatalf("RunRawSQL(%q) error = %q; want only-SELECT message", tc.body, err.Error())
			}
		})
	}
}

// TestRunRawSQLRejectsDataModifyingCTE covers the CTE-bypass class
// of attack: WITH x AS (DELETE FROM tbl RETURNING *) SELECT … parses
// as a top-level SelectStmt with the DML hidden inside
// WithClause.Ctes[i].Ctequery. The previous (top-level only) check
// passed this through. The walker-based check now catches it
// because any nested Node whose oneof field ends in `_stmt` other
// than `select_stmt` is rejected.
func TestRunRawSQLRejectsDataModifyingCTE(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name string
		body string
	}{
		{"delete_cte", "WITH x AS (DELETE FROM krecords RETURNING *) SELECT * FROM x"},
		{"update_cte", "WITH x AS (UPDATE krecords SET status='archived' RETURNING *) SELECT count(*) FROM x"},
		{"insert_cte", "WITH x AS (INSERT INTO krecords(id) VALUES(gen_random_uuid()) RETURNING id) SELECT * FROM x"},
		{"nested_in_subquery", "SELECT * FROM (WITH y AS (DELETE FROM krecords RETURNING id) SELECT id FROM y) t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.RunRawSQL(context.Background(), uuid.New(), tc.body, nil)
			if err == nil {
				t.Fatalf("RunRawSQL(%q) returned nil error; want nested-statement rejection", tc.body)
			}
			if !errors.Is(err, ErrUnsafeSQL) {
				t.Fatalf("RunRawSQL(%q) error = %q; want ErrUnsafeSQL", tc.body, err.Error())
			}
			if !strings.Contains(err.Error(), "nested non-SELECT statement") {
				t.Fatalf("RunRawSQL(%q) error = %q; want nested-statement message", tc.body, err.Error())
			}
		})
	}
}

// TestRunRawSQLRejectsSystemFunction covers the function-call leg
// of the system-catalog rule: pg_read_file, pg_ls_dir, and other
// pg_-prefixed functions can leak server-side files / process info
// without touching a RangeVar, so the walker must inspect FuncCall
// nodes too.
func TestRunRawSQLRejectsSystemFunction(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name string
		body string
	}{
		{"unqualified_pg_read_file", "SELECT pg_read_file('/etc/passwd')"},
		{"unqualified_pg_ls_dir", "SELECT * FROM pg_ls_dir('/')"},
		{"qualified_pg_catalog", "SELECT pg_catalog.pg_read_file('/etc/passwd')"},
		{"qualified_information_schema", "SELECT information_schema._pg_truetypid(NULL, NULL)"},
		{"hidden_in_subquery", "SELECT 1 FROM (SELECT pg_read_file('/etc/passwd') AS x) t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.RunRawSQL(context.Background(), uuid.New(), tc.body, nil)
			if err == nil {
				t.Fatalf("RunRawSQL(%q) returned nil error; want system-function rejection", tc.body)
			}
			if !errors.Is(err, ErrUnsafeSQL) {
				t.Fatalf("RunRawSQL(%q) error = %q; want ErrUnsafeSQL", tc.body, err.Error())
			}
			if !strings.Contains(err.Error(), "system function") {
				t.Fatalf("RunRawSQL(%q) error = %q; want system-function message", tc.body, err.Error())
			}
		})
	}
}

// TestRunRawSQLRejectsSystemCatalog covers the third validator rule:
// no references to pg_catalog / information_schema / pg_-prefixed
// relations. The first two are explicitly scoped; the third covers
// the search_path resolution Postgres applies when the schema is
// omitted (so `pg_tables` is the same as `pg_catalog.pg_tables`).
func TestRunRawSQLRejectsSystemCatalog(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name string
		body string
	}{
		{"explicit_pg_catalog", "SELECT * FROM pg_catalog.pg_authid"},
		{"explicit_info_schema", "SELECT * FROM information_schema.tables"},
		{"unqualified_pg_tables", "SELECT * FROM pg_tables"},
		{"unqualified_pg_stat", "SELECT * FROM pg_stat_activity"},
		{"hidden_in_subquery", "SELECT 1 FROM (SELECT rolname FROM pg_authid) x"},
		{"hidden_in_cte", "WITH a AS (SELECT * FROM pg_roles) SELECT * FROM a"},
		{"hidden_in_union", "SELECT 1 FROM krecords UNION SELECT 1 FROM pg_authid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.RunRawSQL(context.Background(), uuid.New(), tc.body, nil)
			if err == nil {
				t.Fatalf("RunRawSQL(%q) returned nil error; want system-catalog rejection", tc.body)
			}
			if !errors.Is(err, ErrUnsafeSQL) {
				t.Fatalf("RunRawSQL(%q) error = %q; want ErrUnsafeSQL", tc.body, err.Error())
			}
			if !strings.Contains(err.Error(), "system catalog") {
				t.Fatalf("RunRawSQL(%q) error = %q; want system-catalog message", tc.body, err.Error())
			}
		})
	}
}

// TestRunRawSQLRejectsEmptyBody covers the existing "raw_sql body
// required" guard so the validationErr surface stays exercised.
func TestRunRawSQLRejectsEmptyBody(t *testing.T) {
	r := &Runner{}
	if _, err := r.RunRawSQL(context.Background(), uuid.New(), "", nil); err == nil {
		t.Fatal("RunRawSQL(\"\") returned nil error; want validation failure")
	}
}

// TestRunRawSQLRejectsZeroTenant covers the tenant-id guard.
func TestRunRawSQLRejectsZeroTenant(t *testing.T) {
	r := &Runner{}
	if _, err := r.RunRawSQL(context.Background(), uuid.Nil, "SELECT 1", nil); err == nil {
		t.Fatal("RunRawSQL(uuid.Nil) returned nil error; want validation failure")
	}
}

// TestRunRawSQLRejectsUnparseable verifies that gibberish gets
// surfaced as ErrUnsafeSQL rather than crashing into Postgres.
// pg_query reports a syntax error which we wrap as ErrUnsafeSQL.
func TestRunRawSQLRejectsUnparseable(t *testing.T) {
	r := &Runner{}
	body := "SELECT FROM WHERE BY"
	_, err := r.RunRawSQL(context.Background(), uuid.New(), body, nil)
	if err == nil {
		t.Fatalf("RunRawSQL(%q) returned nil error; want parse failure", body)
	}
	if !errors.Is(err, ErrUnsafeSQL) {
		t.Fatalf("RunRawSQL(%q) error = %q; want ErrUnsafeSQL", body, err.Error())
	}
}
