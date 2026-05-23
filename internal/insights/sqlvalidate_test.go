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
func TestValidateRawSQLRejectsBlank(t *testing.T) {
	cases := []string{"", "   ", "\n\n", "\t  \n\t"}
	for _, body := range cases {
		err := validateRawSQL(body)
		if err == nil {
			t.Errorf("validateRawSQL(%q) returned nil; want blank rejection", body)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
