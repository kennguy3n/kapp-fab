//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestTenantContextSwitch1000Tenants asserts the sub-millisecond
// tenant-context-switching SLO from ARCHITECTURE.md §4 line 137 with
// 1000 tenant identities rotated in rapid succession. Unlike the
// 16-tenant BenchmarkTenantContextSwitch / TestTenantContextSwitchP99
// in bench_test.go, this variant widens the tenant fan-out so the GUC
// value actually rotates on every call and the shared connection pool
// cannot coast on a stable `app.tenant_id`.
//
// Each iteration runs `SET LOCAL app.tenant_id = $1; SELECT 1` inside
// dbutil.WithTenantTx, which is the exact path every Kapp API handler
// pays on entry. The assertion floor is 5ms (a safe CI ceiling on
// shared hardware); the architectural claim is sub-1ms on a warm
// local pool, which this test prints via `t.Logf` so regressions are
// visible even when the CI ceiling is not tripped.
func TestTenantContextSwitch1000Tenants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const (
		N          = 1000
		samples    = 5000
		p99Ceiling = 5 * time.Millisecond
	)

	tenants := make([]uuid.UUID, 0, N)
	seedStart := time.Now()
	for i := 0; i < N; i++ {
		tn, err := h.tenants.Create(ctx, tenant.CreateInput{
			Slug: uniqueSlug(fmt.Sprintf("switch-%04d", i)),
			Name: "CtxSwitch1k",
			Cell: "test",
			Plan: "free",
		})
		if err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
		tenants = append(tenants, tn.ID)
	}
	t.Logf("seeded %d tenants in %s", N, time.Since(seedStart))

	// Warm the pool by touching each tenant once so connection
	// handshakes are out of the measured window.
	for i := 0; i < N; i++ {
		if err := pingAs(ctx, h.pool, tenants[i]); err != nil {
			t.Fatalf("warm %d: %v", i, err)
		}
	}

	latencies := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		// Rotate linearly across tenants — guarantees the GUC value
		// changes on every call, which is the realistic multi-tenant
		// gateway pattern the SLO targets.
		tenantID := tenants[i%N]
		start := time.Now()
		if err := pingAs(ctx, h.pool, tenantID); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		latencies = append(latencies, time.Since(start))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	p50 := latencies[len(latencies)*50/100]
	p95 := latencies[len(latencies)*95/100]
	p99 := latencies[len(latencies)*99/100]
	t.Logf("tenant context switch (N=%d tenants, samples=%d): p50=%s p95=%s p99=%s",
		N, len(latencies), p50, p95, p99)

	if p99 > p99Ceiling {
		t.Fatalf("p99 tenant context switch = %s; want <= %s", p99, p99Ceiling)
	}
}

// BenchmarkTenantContextSwitch1000 is the benchmark counterpart to
// TestTenantContextSwitch1000Tenants. Run with:
//
//	KAPP_TEST_DB_URL=postgres://... go test -tags=integration \
//	  -bench=BenchmarkTenantContextSwitch1000 ./internal/integrationtest/...
func BenchmarkTenantContextSwitch1000(b *testing.B) {
	dbURL := os.Getenv("KAPP_TEST_DB_URL")
	if dbURL == "" {
		b.Skip("KAPP_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := platform.NewPool(ctx, dbURL)
	if err != nil {
		b.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	const N = 1000
	tenants := seedBenchTenants(ctx, b, pool, N)
	for i := 0; i < N; i++ {
		if err := pingAs(ctx, pool, tenants[i]); err != nil {
			b.Fatalf("warm %d: %v", i, err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := pingAs(ctx, pool, tenants[i%N]); err != nil {
			b.Fatalf("tx %d: %v", i, err)
		}
	}
}
