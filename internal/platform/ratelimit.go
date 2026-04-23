package platform

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RateLimitConfig controls the default token bucket shape. Individual tenants
// can override `RequestsPerMinute` and `BurstSize` via their `quota` JSONB
// column (keys: api_calls_per_minute, api_calls_burst).
type RateLimitConfig struct {
	RequestsPerMinute int
	BurstSize         int
	IdleTimeout       time.Duration
}

// DefaultRateLimitConfig returns sensible defaults (60 RPM, burst 30,
// 10-minute idle timeout).
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		RequestsPerMinute: 60,
		BurstSize:         30,
		IdleTimeout:       10 * time.Minute,
	}
}

// RateLimiter is a per-tenant token bucket. Buckets that have been idle for
// IdleTimeout are evicted on the next access so inactive tenants do not pay
// the memory cost of a live bucket — the zero-idle-cost invariant from
// ARCHITECTURE.md §1.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[uuid.UUID]*tokenBucket
	config  RateLimitConfig
	now     func() time.Time
}

// ErrRateLimitExceeded is returned when the caller has no tokens left.
var ErrRateLimitExceeded = errors.New("platform: rate limit exceeded")

// NewRateLimiter constructs a limiter using the provided config. A zero
// config is replaced by defaults.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.RequestsPerMinute <= 0 {
		cfg = DefaultRateLimitConfig()
	}
	if cfg.BurstSize <= 0 {
		cfg.BurstSize = cfg.RequestsPerMinute
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	return &RateLimiter{
		buckets: map[uuid.UUID]*tokenBucket{},
		config:  cfg,
		now:     time.Now,
	}
}

// Allow consumes a token from the tenant's bucket and returns true if one
// was available. If the bucket does not yet exist it is created lazily with
// the caller's per-tenant overrides (rpm/burst).
func (r *RateLimiter) Allow(tenantID uuid.UUID, rpm, burst int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.evictIdle(now)

	wantRPM := chooseInt(rpm, r.config.RequestsPerMinute)
	wantBurst := chooseInt(burst, r.config.BurstSize)
	b, ok := r.buckets[tenantID]
	if !ok {
		b = newTokenBucket(wantRPM, wantBurst, now)
		r.buckets[tenantID] = b
	} else {
		// Pick up per-tenant quota changes (plan upgrade/downgrade) for an
		// already-active tenant without waiting for idle eviction.
		b.reshape(wantRPM, wantBurst)
	}
	return b.take(now)
}

// Len returns the current number of live token buckets (i.e. tenants
// seen within the idle window). Primarily a test / metrics hook used to
// assert the zero-idle-cost invariant: after an idle sweep, buckets for
// silent tenants are evicted.
func (r *RateLimiter) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

// Has reports whether the limiter currently tracks a bucket for the
// given tenant. An idle eviction sweep is NOT performed, so callers
// measuring idle cost should pair this with an explicit Allow on any
// other tenant (which triggers evictIdle) immediately before.
func (r *RateLimiter) Has(tenantID uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.buckets[tenantID]
	return ok
}

func (r *RateLimiter) evictIdle(now time.Time) {
	cutoff := now.Add(-r.config.IdleTimeout)
	for k, b := range r.buckets {
		if b.lastAccess.Before(cutoff) {
			delete(r.buckets, k)
		}
	}
}

func chooseInt(requested, fallback int) int {
	if requested > 0 {
		return requested
	}
	return fallback
}

type tokenBucket struct {
	capacity   int
	tokens     float64
	refillPerS float64
	last       time.Time
	lastAccess time.Time
}

func newTokenBucket(rpm, burst int, now time.Time) *tokenBucket {
	return &tokenBucket{
		capacity:   burst,
		tokens:     float64(burst),
		refillPerS: float64(rpm) / 60.0,
		last:       now,
		lastAccess: now,
	}
}

// reshape adjusts the bucket's capacity and refill rate in-place when the
// tenant's plan changes. Tokens already granted are preserved but clamped to
// the new capacity so a downgrade cannot be used to exceed the new burst.
func (b *tokenBucket) reshape(rpm, burst int) {
	newRefill := float64(rpm) / 60.0
	if b.capacity == burst && b.refillPerS == newRefill {
		return
	}
	b.capacity = burst
	b.refillPerS = newRefill
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
}

func (b *tokenBucket) take(now time.Time) bool {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillPerS
		if b.tokens > float64(b.capacity) {
			b.tokens = float64(b.capacity)
		}
		b.last = now
	}
	b.lastAccess = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RateLimitMiddleware enforces per-tenant budgets. It reads tenant overrides
// (api_calls_per_minute, api_calls_burst) from the tenant's Quota.
func RateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := TenantFromContext(r.Context())
			if t == nil {
				http.Error(w, "tenant context missing", http.StatusInternalServerError)
				return
			}
			rpm, burst := extractRateLimitOverrides(t.Quota)
			if !limiter.Allow(t.ID, rpm, burst) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
