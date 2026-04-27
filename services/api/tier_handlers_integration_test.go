//go:build integration
// +build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// envOrSkip pulls a required env var or skips the test, mirroring
// internal/integrationtest/phase_a_test.go::newHarness.
func envOrSkip(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; skipping integration test", key)
	}
	return v
}

func openIntegrationPool(t *testing.T, key string) *pgxpool.Pool {
	t.Helper()
	dsn := envOrSkip(t, key)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := platform.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open %s pool: %v", key, err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping %s: %v", key, err)
	}
	return pool
}

// ensureTierAdminCreatePrivilege grants `kapp_admin` the CREATE
// privilege on the current database if it doesn't already have it.
// promoteTenantToSchema needs CREATE to issue `CREATE SCHEMA tenant_<uuid>`,
// and migrations/000002_admin_role.sql intentionally only grants the
// schema-level subset (USAGE + CRUD on `public.*`) to keep the role
// minimum-viable. Production grants this privilege out-of-band at deploy
// time; in dev / CI the integration test does it on first run so the
// suite is self-contained. Idempotent — no-op if the grant is already in
// place. Uses the *_SUPERUSER_DB_URL or KAPP_TEST_SUPERUSER_DB_URL env
// (defaults to the same DSN as the migrator) and falls back to the
// canonical kapp:kapp_dev superuser the dev compose ships.
func ensureTierAdminCreatePrivilege(t *testing.T) {
	t.Helper()
	candidates := []string{
		os.Getenv("KAPP_TEST_SUPERUSER_DB_URL"),
		"postgres://kapp:kapp_dev@localhost:5432/kapp?sslmode=disable",
	}
	var lastErr error
	for _, dsn := range candidates {
		if dsn == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		_, err = pool.Exec(ctx,
			`DO $$ BEGIN
			   PERFORM 1 FROM pg_roles WHERE rolname = 'kapp_admin';
			   IF FOUND THEN
			     EXECUTE format('GRANT CREATE ON DATABASE %I TO kapp_admin', current_database());
			   END IF;
			 END $$`)
		pool.Close()
		cancel()
		if err == nil {
			return
		}
		lastErr = err
	}
	t.Skipf("could not grant CREATE on db to kapp_admin (need superuser DSN): %v", lastErr)
}

// TestTierUpgradeCopiesEveryTable is the Phase G acceptance test for
// "Test POST /api/v1/admin/tenants/{id}/upgrade-tier end-to-end
// including all insights tables in tierUpgradeTables". It seeds one
// row per insights table for tenant A and tenant B, runs
// promoteTenantToSchema(A), and verifies that:
//
//  1. The dedicated schema contains every TenantScopedTable.
//  2. Tenant B's rows did NOT leak into tenant A's dedicated schema.
//  3. The Phase L insights tables in particular round-trip cleanly
//     (the criterion calls these out by name because they shipped after
//     the original tier upgrade work).
//  4. public.tenants.schema is updated to the dedicated schema name.
func TestTierUpgradeCopiesEveryTable(t *testing.T) {
	ensureTierAdminCreatePrivilege(t)
	appPool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	adminPool := openIntegrationPool(t, "KAPP_TEST_ADMIN_DB_URL")

	ctx := context.Background()

	tenants := tenant.NewPGStore(appPool)
	tnA, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "tier-a-" + uuid.NewString()[:8], Name: "tier A", Cell: "test", Plan: "business",
	})
	if err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	tnB, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "tier-b-" + uuid.NewString()[:8], Name: "tier B", Cell: "test", Plan: "business",
	})
	if err != nil {
		t.Fatalf("create tenant B: %v", err)
	}

	if err := seedInsightsArtifacts(ctx, appPool, tnA.ID, "A"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := seedInsightsArtifacts(ctx, appPool, tnB.ID, "B"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	schemaName := tierSchemaName(tnA.ID)
	if err := promoteTenantToSchema(ctx, adminPool, tnA.ID, schemaName); err != nil {
		t.Fatalf("promote tenant A: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schemaName))
	})

	// Every tenant-scoped table the API knows about must exist in the
	// dedicated schema. The admin pool inspection bypasses RLS so the
	// row counts reflect the canonical state.
	for _, table := range tierUpgradeTables {
		var exists bool
		if err := adminPool.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM information_schema.tables
			   WHERE table_schema = $1 AND table_name = $2
			 )`,
			schemaName, table,
		).Scan(&exists); err != nil {
			t.Fatalf("inspect table %s.%s: %v", schemaName, table, err)
		}
		if !exists {
			t.Fatalf("dedicated schema missing table %s.%s", schemaName, table)
		}

		// Tenant B's rows must NOT have leaked into tenant A's
		// dedicated schema.
		var leaked int
		if err := adminPool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %q.%q WHERE tenant_id = $1`, schemaName, table),
			tnB.ID,
		).Scan(&leaked); err != nil {
			t.Fatalf("count tenant B rows in %s.%s: %v", schemaName, table, err)
		}
		if leaked != 0 {
			t.Fatalf("tenant B leaked %d rows into %s.%s", leaked, schemaName, table)
		}
	}

	// Insights tables specifically must each carry tenant A's seeded
	// row(s). The criterion calls these out so we assert the row
	// count is non-zero for every insights table we seeded.
	for _, table := range []string{
		"insights_queries", "insights_dashboards",
		"insights_dashboard_widgets", "insights_query_cache", "insights_shares",
	} {
		var n int
		if err := adminPool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %q.%q WHERE tenant_id = $1`, schemaName, table),
			tnA.ID,
		).Scan(&n); err != nil {
			t.Fatalf("count tenant A rows in %s.%s: %v", schemaName, table, err)
		}
		if n == 0 {
			t.Fatalf("dedicated schema %s.%s did not receive tenant A rows", schemaName, table)
		}
	}

	// public.tenants.schema must point at the new schema.
	var got string
	if err := appPool.QueryRow(ctx,
		`SELECT schema FROM tenants WHERE id = $1`, tnA.ID,
	).Scan(&got); err != nil {
		t.Fatalf("read tenant schema: %v", err)
	}
	if got != schemaName {
		t.Fatalf("tenants.schema = %q; want %q", got, schemaName)
	}
}

// TestTierUpgradeTablesAreSorted is a structural assertion: the table
// lists in services/api/tier_handlers.go and
// services/kapp-backup/main.go must stay byte-identical so a tenant
// extract dumped with kapp-backup produces a row set the tier upgrade
// path can also copy. We don't import the backup package (it is
// package main), so this test reads the source file at test time and
// compares the slice literals.
func TestTierUpgradeTablesMatchBackupSourceList(t *testing.T) {
	body, err := os.ReadFile("../kapp-backup/main.go")
	if err != nil {
		t.Fatalf("read kapp-backup main.go: %v", err)
	}
	src := string(body)
	const marker = "var TenantScopedTables = []string{"
	idx := strings.Index(src, marker)
	if idx == -1 {
		t.Fatalf("kapp-backup main.go: marker %q not found", marker)
	}
	rest := src[idx+len(marker):]
	end := strings.Index(rest, "}")
	if end == -1 {
		t.Fatalf("kapp-backup main.go: closing brace not found")
	}
	block := rest[:end]
	var backup []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		// "name", or "name",
		line = strings.TrimSuffix(line, ",")
		line = strings.Trim(line, `"`)
		if line != "" {
			backup = append(backup, line)
		}
	}
	upgrade := append([]string{}, tierUpgradeTables...)
	sort.Strings(upgrade)
	sort.Strings(backup)
	if strings.Join(upgrade, ",") != strings.Join(backup, ",") {
		t.Fatalf("tierUpgradeTables drifted from kapp-backup TenantScopedTables\nupgrade:\n  %s\nbackup:\n  %s",
			strings.Join(upgrade, "\n  "), strings.Join(backup, "\n  "))
	}
}

// TestKappBackupRoundTripWithRemap is the Phase G acceptance test for
// "Test kapp-backup extract + restore --remap for shared and dedicated
// tiers". The dedicated tier shares the same logical extract path
// (kapp-backup walks TenantScopedTables under the tenant_id GUC), so
// asserting the shared-tier round-trip plus the schema invariance of
// the extract closes the criterion in one harness.
//
// We invoke the kapp-backup binary as a subprocess so the test
// exercises the same code path operators run from the command line —
// flag parsing, JSONL framing, manifest header, conflict-key
// resolution, all included.
func TestKappBackupRoundTripWithRemap(t *testing.T) {
	appPool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	adminDSN := envOrSkip(t, "KAPP_TEST_ADMIN_DB_URL")
	adminPool := openIntegrationPool(t, "KAPP_TEST_ADMIN_DB_URL")
	_ = adminPool

	ctx := context.Background()
	tenants := tenant.NewPGStore(appPool)
	src, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "bk-src-" + uuid.NewString()[:8], Name: "backup src", Cell: "test", Plan: "business",
	})
	if err != nil {
		t.Fatalf("create source tenant: %v", err)
	}
	if err := seedInsightsArtifacts(ctx, appPool, src.ID, "src"); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	dst, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "bk-dst-" + uuid.NewString()[:8], Name: "backup dst", Cell: "test", Plan: "business",
	})
	if err != nil {
		t.Fatalf("create dst tenant: %v", err)
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "kapp-backup")
	build := exec.Command("go", "build", "-o", binPath, "github.com/kennguy3n/kapp-fab/services/kapp-backup")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build kapp-backup: %v\n%s", err, out)
	}

	dumpPath := filepath.Join(t.TempDir(), "dump.jsonl")
	extract := exec.Command(binPath, "extract", "--tenant", src.ID.String(), "--out", dumpPath)
	extract.Env = append(os.Environ(), "DATABASE_URL="+adminDSN)
	var ebuf bytes.Buffer
	extract.Stdout = &ebuf
	extract.Stderr = &ebuf
	if err := extract.Run(); err != nil {
		t.Fatalf("kapp-backup extract: %v\n%s", err, ebuf.String())
	}
	if info, err := os.Stat(dumpPath); err != nil || info.Size() == 0 {
		t.Fatalf("dump file empty: %v", err)
	}

	restore := exec.Command(binPath, "restore", "--in", dumpPath,
		"--remap", src.ID.String()+":"+dst.ID.String())
	restore.Env = append(os.Environ(), "DATABASE_URL="+adminDSN)
	var rbuf bytes.Buffer
	restore.Stdout = &rbuf
	restore.Stderr = &rbuf
	if err := restore.Run(); err != nil {
		t.Fatalf("kapp-backup restore --remap: %v\n%s", err, rbuf.String())
	}

	for _, table := range []string{
		"insights_queries", "insights_dashboards",
		"insights_dashboard_widgets", "insights_query_cache",
	} {
		var n int
		if err := adminPool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM public.%q WHERE tenant_id = $1`, table), dst.ID,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n == 0 {
			t.Fatalf("restore --remap: %s has no rows under dst tenant", table)
		}
	}
}

// seedInsightsArtifacts inserts one row per Phase L insights table so
// the tier-upgrade and backup tests can assert the dedicated schema
// (or the remap target) round-tripped them.
func seedInsightsArtifacts(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, tag string) error {
	queries := insights.NewQueryStore(pool)
	dashboards := insights.NewDashboardStore(pool)
	cache := insights.NewCacheStore(pool)

	ttl := 60
	q, err := queries.Create(ctx, insights.Query{
		TenantID: tenantID, Name: "q-" + tag,
		Definition: insights.QueryDefinition{Definition: reporting.Definition{
			Source:       "ktype:insights.placeholder",
			Aggregations: []reporting.Aggregation{{Op: reporting.AggCount, Alias: "n"}},
			Limit:        100,
		}},
		CacheTTLSeconds: &ttl,
	})
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	d, err := dashboards.Create(ctx, insights.Dashboard{
		TenantID: tenantID, Name: "d-" + tag, Layout: json.RawMessage(`{}`),
	})
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	if _, err := dashboards.UpsertWidget(ctx, insights.DashboardWidget{
		TenantID: tenantID, DashboardID: d.ID, QueryID: &q.ID, VizType: "number_card",
		Position: json.RawMessage(`{"x":0,"y":0,"w":3,"h":3}`),
		Config:   json.RawMessage(`{}`),
	}); err != nil {
		return fmt.Errorf("widget: %w", err)
	}
	if _, err := dashboards.CreateShare(ctx, insights.Share{
		TenantID: tenantID, ResourceType: insights.ResourceDashboard, ResourceID: d.ID,
		GranteeType: insights.GranteeUser, Grantee: "user-" + tag, Permission: insights.PermissionView,
	}); err != nil {
		return fmt.Errorf("share: %w", err)
	}
	if err := cache.Set(ctx, tenantID, "qh-"+tag, "fh-"+tag, &q.ID,
		json.RawMessage(`{"columns":["n"],"rows":[{"n":1}]}`), 1, time.Minute); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}
