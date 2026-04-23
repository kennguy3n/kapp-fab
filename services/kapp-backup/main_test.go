package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestConflictClauseStandardPK exercises the default `(tenant_id, id)`
// path so a second restore of the same dump overwrites existing rows
// instead of silently dropping updates.
func TestConflictClauseStandardPK(t *testing.T) {
	got := conflictClause("krecords", []string{"tenant_id", "id", "ktype", "data", "version"})
	want := `ON CONFLICT ("tenant_id", "id") DO UPDATE SET "ktype" = EXCLUDED."ktype", "data" = EXCLUDED."data", "version" = EXCLUDED."version"`
	if got != want {
		t.Fatalf("conflictClause mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestConflictClauseCompositePK locks in the PK map for the two
// identity tables added to TenantScopedTables: user_tenants and
// roles. A future refactor that drops them from the map would
// silently regress to ON CONFLICT DO NOTHING, which is the bug the
// original Devin Review finding flagged.
func TestConflictClauseCompositePK(t *testing.T) {
	for _, tc := range []struct {
		table string
		cols  []string
		want  string
	}{
		{
			table: "user_tenants",
			cols:  []string{"user_id", "tenant_id", "role", "status"},
			want:  `ON CONFLICT ("user_id", "tenant_id") DO UPDATE SET "role" = EXCLUDED."role", "status" = EXCLUDED."status"`,
		},
		{
			table: "roles",
			cols:  []string{"tenant_id", "name", "permissions"},
			want:  `ON CONFLICT ("tenant_id", "name") DO UPDATE SET "permissions" = EXCLUDED."permissions"`,
		},
		{
			table: "lesson_progress",
			cols:  []string{"tenant_id", "enrollment_id", "lesson_id", "status", "score"},
			want:  `ON CONFLICT ("tenant_id", "enrollment_id", "lesson_id") DO UPDATE SET "status" = EXCLUDED."status", "score" = EXCLUDED."score"`,
		},
		{
			// accounts has no `id` column — PK is (tenant_id, code).
			// Without an explicit entry the fallback path would emit
			// ON CONFLICT DO NOTHING and silently drop re-restored
			// rows, so we pin the composite PK here.
			table: "accounts",
			cols:  []string{"tenant_id", "code", "name", "type"},
			want:  `ON CONFLICT ("tenant_id", "code") DO UPDATE SET "name" = EXCLUDED."name", "type" = EXCLUDED."type"`,
		},
		{
			// idempotency_keys has no `id` column — PK is (tenant_id, key).
			table: "idempotency_keys",
			cols:  []string{"tenant_id", "key", "response_code", "response_body"},
			want:  `ON CONFLICT ("tenant_id", "key") DO UPDATE SET "response_code" = EXCLUDED."response_code", "response_body" = EXCLUDED."response_body"`,
		},
	} {
		t.Run(tc.table, func(t *testing.T) {
			got := conflictClause(tc.table, tc.cols)
			if got != tc.want {
				t.Fatalf("\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestConflictClauseFallback covers the no-`id`, no-map case: the
// row has nothing we can safely use as a conflict target, so the
// statement falls back to ON CONFLICT DO NOTHING. An audit_log entry
// with a surrogate id is the positive case — it must upsert.
func TestConflictClauseFallback(t *testing.T) {
	got := conflictClause("unknown_table", []string{"foo", "bar"})
	want := "ON CONFLICT DO NOTHING"
	if got != want {
		t.Fatalf("unknown table: got %q want %q", got, want)
	}

	got = conflictClause("audit_log", []string{"tenant_id", "id", "action", "actor"})
	want = `ON CONFLICT ("tenant_id", "id") DO UPDATE SET "action" = EXCLUDED."action", "actor" = EXCLUDED."actor"`
	if got != want {
		t.Fatalf("audit_log: got %q want %q", got, want)
	}
}

// TestConflictClauseAllKeyColumns guards a subtle branch: if the
// dump carries only the PK columns and nothing else, the SET list
// would be empty. Emitting an empty SET is a SQL error, so we
// degrade to ON CONFLICT (...) DO NOTHING in that case.
func TestConflictClauseAllKeyColumns(t *testing.T) {
	got := conflictClause("roles", []string{"tenant_id", "name"})
	want := `ON CONFLICT ("tenant_id", "name") DO NOTHING`
	if got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// TestTenantScopedTablesCoversMigrations is a schema-driven
// regression test: scan every migration file for CREATE TABLE blocks
// whose column list includes `tenant_id UUID NOT NULL`, and fail if
// the discovered table is missing from TenantScopedTables. This is
// how the missing `user_tenants` / `roles` entries flagged in Devin
// Review slipped in — future additions will now break the build
// instead of silently producing broken restores.
func TestTenantScopedTablesCoversMigrations(t *testing.T) {
	migrationsDir := locateMigrationsDir(t)
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no migrations found under %s", migrationsDir)
	}
	discovered := map[string]struct{}{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, table := range extractTenantScopedTables(string(raw)) {
			discovered[table] = struct{}{}
		}
	}
	if len(discovered) == 0 {
		t.Fatalf("parser found zero tenant-scoped tables in %s \u2014 regex broken", migrationsDir)
	}
	listed := map[string]struct{}{}
	for _, t := range TenantScopedTables {
		listed[t] = struct{}{}
	}
	var missing []string
	for name := range discovered {
		if _, ok := listed[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("TenantScopedTables is missing %d schema-declared tenant-scoped tables: %v.\nAdd them to services/kapp-backup/main.go and scripts/upgrade_tier.sh, and update tableConflictKeys if their PK is not (tenant_id, id).", len(missing), missing)
	}
}

// extractTenantScopedTables parses SQL to find every `CREATE TABLE`
// block that declares a `tenant_id` column. We deliberately keep the
// parser dumb \u2014 we match the CREATE TABLE header and then scan until
// the first `);` for a line that starts with `tenant_id`. That's good
// enough for the hand-written migrations in this repo and avoids
// pulling in a real SQL parser.
func extractTenantScopedTables(sql string) []string {
	headerRe := regexp.MustCompile(`(?i)CREATE TABLE(?:\s+IF NOT EXISTS)?\s+([a-z_][a-z0-9_]*)\s*\(`)
	tenantColRe := regexp.MustCompile(`(?im)^\s*tenant_id\b`)
	var out []string
	for _, match := range headerRe.FindAllStringSubmatchIndex(sql, -1) {
		// match[2:4] are the capture group (table name) indices.
		name := sql[match[2]:match[3]]
		// Skip PARTITION OF tables \u2014 they piggyback on the parent.
		if strings.Contains(strings.ToLower(sql[match[0]:match[1]]), "partition of") {
			continue
		}
		// Find the matching closing paren for the column list.
		rest := sql[match[1]:]
		end := strings.Index(rest, "\n);")
		if end < 0 {
			continue
		}
		body := rest[:end]
		if tenantColRe.MatchString(body) {
			out = append(out, name)
		}
	}
	return out
}

// locateMigrationsDir walks up from the test binary's working
// directory until it finds a sibling `migrations/` folder. Tests run
// from the package directory, so the repo root sits two levels up;
// we don't hardcode that so `go test ./...` from anywhere still works.
func locateMigrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := wd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "migrations")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	t.Fatalf("migrations/ not found above %s", wd)
	return ""
}
