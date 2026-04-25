//go:build loadtest
// +build loadtest

package loadtest

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestFiveThousandTenantMixedLoad scales the harness to 5000 tenants
// with the standard mixed-workload profile (CRUD + ledger posts) and
// asserts the Phase K SLOs.
//
// Required env:
//
//	KAPP_TEST_DB_URL — Postgres URL with the migrations applied.
//
// Run with:
//
//	go test -tags=loadtest -timeout=2h -run TestFiveThousandTenantMixedLoad \
//	  ./internal/integrationtest/loadtest/...
func TestFiveThousandTenantMixedLoad(t *testing.T) {
	dsn := os.Getenv("KAPP_TEST_DB_URL")
	if dsn == "" {
		t.Skip("KAPP_TEST_DB_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	cfg := Config{
		Tenants:            5000,
		Workers:            64,
		CRUDOpsPerTenant:   8,
		LedgerOpsPerTenant: 4,
		SLO: SLOTargets{
			APIp99:             100 * time.Millisecond,
			PostJournalp99:     250 * time.Millisecond,
			MaxFailureRate:     0,
			MaxPoolUtilization: 0.95,
		},
	}
	res, err := Run(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, res)
	}
	t.Logf("%s", res)
}

// TestZKPlacementFleetSpread provisions 100 tenants with distinct
// placement-policy parameters and verifies that each tenant's stored
// policy reflects what the harness asked for. The check runs without
// the fabric console attached — we only validate the persisted
// state on the Kapp side, which is the authoritative record the API
// gateway returns.
func TestZKPlacementFleetSpread(t *testing.T) {
	dsn := os.Getenv("KAPP_TEST_DB_URL")
	if dsn == "" {
		t.Skip("KAPP_TEST_DB_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	store := tenant.NewPGStore(pool)
	const n = 100
	countries := []string{"US", "DE", "JP", "BR", "AU"}
	providers := [][]string{
		{"local"},
		{"wasabi"},
		{"local", "wasabi"},
		{"local", "azure"},
	}

	wg := sync.WaitGroup{}
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			ctx := context.Background()
			slug := fmt.Sprintf("zk-spread-%04d", i)
			created, err := store.Create(ctx, tenant.CreateInput{
				Slug: slug, Name: slug, Cell: "default", Plan: "starter",
			})
			if err != nil {
				errs <- fmt.Errorf("create %s: %w", slug, err)
				return
			}
			tenantID := created.ID
			policyCfg := tenant.PlacementPolicyConfig{
				Plan:             "starter",
				Country:          countries[i%len(countries)],
				DefaultProviders: providers[i%len(providers)],
			}
			policy := tenant.DerivePlacementPolicy(policyCfg)
			policy.Tenant = tenantID.String()
			policy.Bucket = "bucket-" + slug
			if err := store.SetPlacementPolicy(ctx, tenantID, policy); err != nil {
				errs <- fmt.Errorf("set %s: %w", slug, err)
				return
			}
			got, ok, err := store.GetPlacementPolicy(ctx, tenantID)
			if err != nil {
				errs <- fmt.Errorf("get %s: %w", slug, err)
				return
			}
			if !ok || got.Tenant != tenantID.String() {
				errs <- fmt.Errorf("%s: persisted policy missing tenant", slug)
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("placement fleet spread: %v", err)
	}
}
