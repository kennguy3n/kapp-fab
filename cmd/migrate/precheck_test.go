package main

import (
	"strings"
	"testing"
)

// findingRules returns the set of rule labels analyzeSQL flagged for a
// blob, so assertions can check exactly which rules fired.
func findingRules(sql string) []string {
	findings := analyzeSQL(1, "test", sql)
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.rule)
	}
	return out
}

func hasRule(rules []string, substr string) bool {
	for _, r := range rules {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func TestAnalyzeSQL_FlagsBackwardIncompatible(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string // substring of the expected rule label
	}{
		{"drop table", "DROP TABLE legacy_widgets;", "DROP TABLE"},
		{"drop column", "ALTER TABLE invoices DROP COLUMN old_total;", "DROP COLUMN"},
		{"table rename", "ALTER TABLE deals RENAME TO opportunities;", "table rename"},
		{"column rename", "ALTER TABLE deals RENAME COLUMN amt TO amount;", "column rename"},
		{"set not null", "ALTER TABLE users ALTER COLUMN email SET NOT NULL;", "SET NOT NULL"},
		{
			"add not null no default",
			"ALTER TABLE users ADD COLUMN region text NOT NULL;",
			"NOT NULL without DEFAULT",
		},
		{
			"lowercase still flagged",
			"alter table users drop column nickname;",
			"DROP COLUMN",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules := findingRules(tc.sql)
			if !hasRule(rules, tc.want) {
				t.Fatalf("sql %q: rules=%v, want one containing %q", tc.sql, rules, tc.want)
			}
		})
	}
}

func TestAnalyzeSQL_AllowsBackwardCompatible(t *testing.T) {
	safe := []string{
		"CREATE TABLE IF NOT EXISTS widgets (id bigserial PRIMARY KEY);",
		"ALTER TABLE users ADD COLUMN IF NOT EXISTS nickname text;",
		"ALTER TABLE users ADD COLUMN region text NOT NULL DEFAULT 'us';",
		"ALTER TABLE users ALTER COLUMN nickname DROP NOT NULL;",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_users_email ON users (email);",
		"ALTER TABLE users DROP CONSTRAINT users_email_key;",
		"DROP INDEX CONCURRENTLY IF EXISTS idx_old;",
		// GENERATED column may be NOT NULL without an explicit DEFAULT.
		"ALTER TABLE invoices ADD COLUMN total_cents bigint GENERATED ALWAYS AS (total * 100) STORED NOT NULL;",
	}
	for _, sql := range safe {
		if rules := findingRules(sql); len(rules) != 0 {
			t.Fatalf("sql %q unexpectedly flagged: %v", sql, rules)
		}
	}
}

// Keywords appearing only inside comments or string literals must not
// be mistaken for real DDL.
func TestAnalyzeSQL_IgnoresCommentsAndLiterals(t *testing.T) {
	cases := []string{
		"-- This migration does NOT drop column foo; it only adds one.\nALTER TABLE t ADD COLUMN IF NOT EXISTS foo text;",
		"/* historical note: we used to DROP TABLE t here */ CREATE TABLE IF NOT EXISTS t (id int);",
		"INSERT INTO audit_log (msg) VALUES ('previously this would DROP COLUMN x');",
	}
	for _, sql := range cases {
		if rules := findingRules(sql); len(rules) != 0 {
			t.Fatalf("sql %q should be clean, got: %v", sql, rules)
		}
	}
}

// A `;` inside a dollar-quoted function body must not split the
// statement, and DDL-looking text inside the body must not be flagged.
func TestSplitStatements_DollarQuotedBody(t *testing.T) {
	sql := `CREATE FUNCTION f() RETURNS void AS $$
BEGIN
  -- DROP COLUMN should be ignored inside this body
  PERFORM 1; PERFORM 2;
END;
$$ LANGUAGE plpgsql;
ALTER TABLE t ADD COLUMN IF NOT EXISTS c int;`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2: %#v", len(stmts), stmts)
	}
	if rules := findingRules(sql); len(rules) != 0 {
		t.Fatalf("dollar-quoted body should not flag: %v", rules)
	}
}

func TestAnalyzeSQL_MultipleStatementsAggregate(t *testing.T) {
	sql := `ALTER TABLE a DROP COLUMN x;
ALTER TABLE b RENAME TO c;
CREATE TABLE d (id int);`
	rules := findingRules(sql)
	if len(rules) != 2 {
		t.Fatalf("got %d findings, want 2: %v", len(rules), rules)
	}
	if !hasRule(rules, "DROP COLUMN") || !hasRule(rules, "table rename") {
		t.Fatalf("missing expected findings: %v", rules)
	}
}
