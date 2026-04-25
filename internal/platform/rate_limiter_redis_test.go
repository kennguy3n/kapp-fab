package platform

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
)

// TestRedisRateLimiter_BurstThenDeny exercises the sliding-window
// Lua script against an in-process miniredis. The first `burst`
// requests are allowed; the next one is denied. After the refill
// window the limiter recovers a single token and allows one more.
func TestRedisRateLimiter_BurstThenDeny(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	ctx := context.Background()
	lim, err := NewRedisRateLimiter(ctx, "redis://"+mr.Addr(), RateLimitConfig{
		RequestsPerMinute: 60, // 1 token / sec
		BurstSize:         3,
		IdleTimeout:       2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("new redis rate limiter: %v", err)
	}
	defer lim.Close()

	tenant := uuid.New()
	for i := 0; i < 3; i++ {
		if !lim.Allow(tenant, 60, 3) {
			t.Fatalf("expected request %d to be allowed in burst", i)
		}
	}
	if lim.Allow(tenant, 60, 3) {
		t.Fatalf("expected 4th request in same instant to be denied")
	}

	// Wait long enough for the bucket to refill exactly one
	// token (60 RPM = 1 tok/s). The Lua script reads now_ms from
	// real time on the Go side, not miniredis' simulated clock,
	// so an actual sleep is required.
	time.Sleep(1100 * time.Millisecond)
	if !lim.Allow(tenant, 60, 3) {
		t.Fatalf("expected request after 1s refill to be allowed")
	}
	if lim.Allow(tenant, 60, 3) {
		t.Fatalf("expected the next request after refill to be denied")
	}
}

// TestRedisRateLimiter_PerTenantIsolation guards the key format —
// two distinct tenants share the same Redis instance but do not
// share a bucket. A tenant burning their burst should not affect a
// neighbour.
func TestRedisRateLimiter_PerTenantIsolation(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	ctx := context.Background()
	lim, err := NewRedisRateLimiter(ctx, "redis://"+mr.Addr(), RateLimitConfig{
		RequestsPerMinute: 60,
		BurstSize:         2,
		IdleTimeout:       2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("new redis rate limiter: %v", err)
	}
	defer lim.Close()

	a, b := uuid.New(), uuid.New()
	for i := 0; i < 2; i++ {
		if !lim.Allow(a, 60, 2) {
			t.Fatalf("expected tenant A request %d to be allowed", i)
		}
	}
	if lim.Allow(a, 60, 2) {
		t.Fatalf("tenant A should be exhausted")
	}
	if !lim.Allow(b, 60, 2) {
		t.Fatalf("tenant B's first request must not be impacted by A")
	}
}

// TestRedisRateLimiter_FailsOpenOnError verifies that a Redis
// outage does not block traffic. We close the limiter (and the
// underlying client), then call Allow and expect a permissive
// result.
func TestRedisRateLimiter_FailsOpenOnError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	ctx := context.Background()
	lim, err := NewRedisRateLimiter(ctx, "redis://"+mr.Addr(), RateLimitConfig{
		RequestsPerMinute: 60,
		BurstSize:         1,
	})
	if err != nil {
		t.Fatalf("new redis rate limiter: %v", err)
	}
	mr.Close()
	if got := lim.Allow(uuid.New(), 60, 1); !got {
		t.Fatalf("expected fail-open behaviour after redis outage, got deny")
	}
}
