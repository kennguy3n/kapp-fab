//go:build loadtest_acceptance
// +build loadtest_acceptance

package loadtest

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPhaseGAcceptanceLoad is the operator-runnable harness used to
// produce the SLO numbers documented in docs/PHASE_G_ACCEPTANCE.md.
//
// LT_TENANTS controls the fleet size (default 5000). Override with a
// smaller value to run a quick smoke locally; the canonical Phase G
// acceptance run uses 5000.
//
//	go test -tags=loadtest_acceptance -timeout=4h \
//	  -run TestPhaseGAcceptanceLoad \
//	  ./internal/integrationtest/loadtest/...
func TestPhaseGAcceptanceLoad(t *testing.T) {
	dsn := os.Getenv("KAPP_TEST_DB_URL")
	if dsn == "" {
		t.Skip("KAPP_TEST_DB_URL not set")
	}
	tenants := 5000
	if v := os.Getenv("LT_TENANTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("LT_TENANTS invalid: %q", v)
		}
		tenants = n
	}
	// Default pgxpool max_conns is min(4*cpus, server max). The
	// 5k-tenant target needs more headroom so the SLO assertion on
	// pool utilisation reflects DB capacity rather than the
	// driver's default ceiling. LT_MAX_CONNS overrides; default is
	// 96 which is below the 200-conn server cap shipped with the
	// dev compose stack.
	maxConns := 96
	if v := os.Getenv("LT_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("LT_MAX_CONNS invalid: %q", v)
		}
		maxConns = n
	}
	cfg2, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg2.MaxConns = int32(maxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg2)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	cfg := Config{
		Tenants:            tenants,
		Workers:            64,
		CRUDOpsPerTenant:   8,
		LedgerOpsPerTenant: 4,
		SLO: SLOTargets{
			APIp99:             100 * time.Millisecond,
			PostJournalp99:     250 * time.Millisecond,
			MaxFailureRate:     0.001,
			MaxPoolUtilization: 0.95,
		},
	}
	start := time.Now()
	res, err := Run(context.Background(), pool, cfg)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("run (%s): %v\n%s", dur, err, res)
	}
	if res == nil {
		t.Fatalf("nil result")
	}
	t.Logf("phase-g acceptance: tenants=%d duration=%s\n%s", tenants, dur, res)
	// Persist result text to a stable path so the operator can copy
	// into docs/PHASE_G_ACCEPTANCE.md without scraping `go test` output.
	if path := os.Getenv("LT_REPORT_PATH"); path != "" {
		body := fmt.Sprintf("# Phase G acceptance load run\n\nTenants: %d\nDuration: %s\n\n```\n%s\n```\n", tenants, dur, res)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write report: %v", err)
		}
	}
}
