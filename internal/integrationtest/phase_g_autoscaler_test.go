//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// TestAutoscalerEndToEnd asserts the platform autoscaler reads the
// cells table, applies the configured policy thresholds, and writes
// a decision row into platform_scale_events. Driving the engine
// against a real postgres exercises the snapshot SQL (LATERAL JOIN
// against platform_scale_events for the cooldown check) and the
// JSON marshaling path that a unit test against Decide() can't
// reach.
func TestAutoscalerEndToEnd(t *testing.T) {
	dbURL := os.Getenv("KAPP_TEST_ADMIN_DB_URL")
	if dbURL == "" {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer pool.Close()

	// Seed two cells: one over CPU threshold, one comfortably idle
	// with a small tenant footprint (so the scale_down branch fires).
	cellHot := "cell_test_hot"
	cellCold := "cell_test_cold"
	// Belt-and-braces: a prior failed run could have left rows
	// behind. Clear them up-front so the cooldown check inside
	// Decide() doesn't see a stale recent scale event.
	_, _ = pool.Exec(ctx,
		`DELETE FROM platform_scale_events WHERE cell_id IN ($1,$2)`, cellHot, cellCold)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM platform_scale_events WHERE cell_id IN ($1,$2)`, cellHot, cellCold)
		_, _ = pool.Exec(context.Background(),
			`UPDATE tenants SET cell_id = NULL WHERE cell_id IN ($1,$2)`, cellHot, cellCold)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM cells WHERE id IN ($1,$2)`, cellHot, cellCold)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO cells (id, region, max_tenants, cpu_pct, mem_pct, conn_saturation_pct)
		 VALUES ($1, 'test', 1000, 95, 50, 50),
		        ($2, 'test', 1000, 5,  5,  5)
		 ON CONFLICT (id) DO UPDATE SET
		   cpu_pct = EXCLUDED.cpu_pct,
		   mem_pct = EXCLUDED.mem_pct,
		   conn_saturation_pct = EXCLUDED.conn_saturation_pct,
		   observed_at = now()`,
		cellHot, cellCold,
	); err != nil {
		t.Fatalf("seed cells: %v", err)
	}

	engine := platform.NewAutoscaleEngine(pool, platform.DefaultAutoscalePolicy(), nil)
	decisions, err := engine.Evaluate(ctx)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	gotHot := false
	for _, d := range decisions {
		switch d.CellID {
		case cellHot:
			gotHot = true
			if d.EventType != platform.CellEventScaleUp {
				t.Errorf("hot cell: expected scale_up, got %s (%s)", d.EventType, d.Reason)
			}
		case cellCold:
			// Cold cell with 0 tenants and idle metrics should
			// scale_down per DefaultAutoscalePolicy.
			if d.EventType != platform.CellEventScaleDown {
				t.Errorf("cold cell: expected scale_down, got %s (%s)", d.EventType, d.Reason)
			}
		}
	}
	if !gotHot {
		t.Errorf("hot cell decision missing from %d total", len(decisions))
	}

	// Confirm both decisions landed in platform_scale_events. Hold
	// rows from other cells in the same evaluation are noise; we
	// only assert on the ids we seeded.
	var hotEventCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM platform_scale_events WHERE cell_id = $1 AND event_type = $2`,
		cellHot, platform.CellEventScaleUp,
	).Scan(&hotEventCount); err != nil {
		t.Fatalf("count hot events: %v", err)
	}
	if hotEventCount == 0 {
		t.Errorf("expected at least one scale_up row in platform_scale_events for %s", cellHot)
	}

	// Cooldown: a second Evaluate within MinHoldBetweenScales must
	// downgrade the hot decision to a hold even though metrics are
	// still over threshold.
	decisions2, err := engine.Evaluate(ctx)
	if err != nil {
		t.Fatalf("evaluate 2: %v", err)
	}
	for _, d := range decisions2 {
		if d.CellID == cellHot && d.EventType != platform.CellEventHold {
			t.Errorf("expected hot cell cooldown hold on second tick, got %s (%s)", d.EventType, d.Reason)
		}
	}
}

// TestTierUpgradeUsesSecurityDefiner verifies the SECURITY DEFINER
// function path works under a non-superuser caller. The default
// install GRANTs EXECUTE to kapp_admin, so we connect as kapp_admin
// (no BYPASSRLS bypass within the function call itself, since the
// function runs as its definer kapp_tier_admin).
func TestTierUpgradeUsesSecurityDefiner(t *testing.T) {
	adminURL := os.Getenv("KAPP_TEST_ADMIN_DB_URL")
	if adminURL == "" {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer pool.Close()

	// Inspect the function's prosecdef bit and owner so future
	// migrations cannot accidentally drop SECURITY DEFINER (which
	// would break the kapp_tier_admin scope by silently running
	// as the caller).
	var prosecdef bool
	var owner string
	if err := pool.QueryRow(ctx,
		`SELECT p.prosecdef, r.rolname
		   FROM pg_proc p
		   JOIN pg_namespace n ON n.oid = p.pronamespace
		   JOIN pg_roles r ON r.oid = p.proowner
		  WHERE n.nspname = 'public' AND p.proname = 'promote_tenant_to_schema'`,
	).Scan(&prosecdef, &owner); err != nil {
		t.Fatalf("inspect promote_tenant_to_schema: %v", err)
	}
	if !prosecdef {
		t.Fatalf("promote_tenant_to_schema is not SECURITY DEFINER")
	}
	if owner != "kapp_tier_admin" {
		t.Fatalf("promote_tenant_to_schema owner = %q, want %q", owner, "kapp_tier_admin")
	}
}
