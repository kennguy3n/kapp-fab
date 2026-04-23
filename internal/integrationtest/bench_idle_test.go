//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestIdleTenantsEvictAfterTimeout validates the zero-idle-cost
// architectural invariant from ARCHITECTURE.md §1 lines 16-17 at a
// scale that approximates a real cell: 100 tenants warm their caches
// with a single request each, then sit idle past the configured
// IdleTimeout. Afterwards the per-tenant RateLimiter bucket map and
// the shared LRU metadata cache must both be empty for those idle
// tenants. This proves that hitting N tenants once does not leave N
// long-lived allocations behind.
//
// The test intentionally does NOT provision DB rows or exercise the
// record store — those are covered by load_test.go. The point here is
// to pin down the invariant that a tenant that has gone idle costs
// nothing in memory, which is the constraint the cell-density target
// depends on.
func TestIdleTenantsEvictAfterTimeout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const (
		numTenants  = 100
		idleWindow  = 100 * time.Millisecond
		cacheWindow = 100 * time.Millisecond
		cacheMax    = 1024
	)

	tenants := make([]uuid.UUID, 0, numTenants)
	for i := 0; i < numTenants; i++ {
		tn, err := h.tenants.Create(ctx, tenant.CreateInput{
			Slug: uniqueSlug(fmt.Sprintf("idle-%03d", i)),
			Name: "Idle Tenant",
			Cell: "test",
			Plan: "free",
		})
		if err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
		tenants = append(tenants, tn.ID)
	}

	limiter := platform.NewRateLimiter(platform.RateLimitConfig{
		RequestsPerMinute: 6000,
		BurstSize:         1000,
		IdleTimeout:       idleWindow,
	})
	cache := platform.NewLRUCache(cacheMax, cacheWindow)

	// Each tenant hits the limiter and warms a metadata entry once.
	// The bucket + cache key are both tenant-scoped so the invariant
	// we care about is: after the idle window, nothing keyed by any
	// of these tenants remains.
	for i, tid := range tenants {
		if !limiter.Allow(tid, 0, 0) {
			t.Fatalf("tenant %d unexpectedly rate-limited on warm-up", i)
		}
		cache.Set(fmt.Sprintf("%s:ktype:demo.note", tid), map[string]any{"warm": true})
	}

	if got := limiter.Len(); got != numTenants {
		t.Fatalf("warm-up: limiter buckets = %d; want %d", got, numTenants)
	}

	// Wait past the idle window and trigger one sweep per structure.
	time.Sleep(idleWindow + 50*time.Millisecond)
	_ = limiter.Allow(uuid.New(), 0, 0) // evict-on-access sweep
	// The LRU cache evicts on Get when the entry's TTL has expired.
	for _, tid := range tenants {
		if _, ok := cache.Get(fmt.Sprintf("%s:ktype:demo.note", tid)); ok {
			t.Fatalf("tenant %s cache entry survived TTL", tid)
		}
	}

	// The limiter now holds only the sweeper we just poked; none of
	// the original idle tenants should remain.
	for _, tid := range tenants {
		if limiter.Has(tid) {
			t.Fatalf("idle tenant %s still has a rate-limit bucket after timeout", tid)
		}
	}
}
