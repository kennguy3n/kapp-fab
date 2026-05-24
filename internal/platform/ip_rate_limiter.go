package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ipBucketTTL is how long an idle IP bucket lingers in memory or
// Redis before being evicted. 10 minutes is well past any reasonable
// burst window and lets honest clients survive a brief reconnect
// without paying for a cold-start refill.
const ipBucketTTL = 10 * time.Minute

// DefaultIPSweepInterval is how often the in-process limiter purges
// idle buckets when the background sweeper is running. 5 minutes is
// half the bucket TTL — short enough that a distributed bot attack
// with millions of unique source IPs cannot accumulate more than a
// few minutes worth of stale entries before they are dropped.
const DefaultIPSweepInterval = 5 * time.Minute

// IPRateLimiterBackend abstracts the storage layer for the
// IPRateLimitMiddleware. Production wiring constructs a
// RedisIPRateLimiter so every API replica enforces the same per-IP
// ceiling; tests and single-replica deployments fall back to
// InProcIPRateLimiter, which gives the correct semantics for a
// single pod but cannot enforce a global ceiling across replicas.
type IPRateLimiterBackend interface {
	// AllowCtx consumes one token from the bucket keyed by ip. rpm
	// is the steady-state refill rate (tokens per minute) and burst
	// is the bucket capacity. Returns true when the request is
	// allowed; false when the bucket is empty. Errors fail OPEN —
	// a transient Redis outage must not lock every public form out
	// of the site.
	AllowCtx(ctx context.Context, ip string, rpm, burst int) bool
}

// IPRateLimitMiddleware enforces a per-client-IP token bucket on the
// wrapped handler. It is intended for un-authenticated public routes
// (e.g. POST /api/v1/forms/{id}/submit, GET /api/v1/insights/embed/{token})
// where there is no tenant context to key the tenant-scoped
// RateLimitMiddleware off.
//
// keyPrefix scopes the bucket keyspace so multiple middleware
// instances sharing a backend (e.g. one for form submissions, one
// for public embeds) maintain independent buckets per IP. Without
// the prefix, two instances calling backend.AllowCtx(ip, ...) with
// different (rpm, burst) would overwrite each other's bucket state
// on every request because the in-process map and Redis HSET both
// key on the IP alone. Pass a stable, human-readable prefix
// ("form", "embed", etc.) — the value lands in the limiter key as
// "<prefix>:<ip>" and is never exposed back to the client.
//
// The IP is taken from r.RemoteAddr — chi.middleware.RealIP must run
// earlier in the chain so that value reflects the original client
// rather than the reverse proxy. Requests whose IP cannot be parsed
// (which should not happen in practice) are allowed through and
// counted as the unparsed string, so a malformed connection cannot
// take down the whole route.
//
// rpm is the steady-state refill rate (tokens per minute). burst is
// the bucket capacity, i.e. the maximum number of requests a single
// IP can fire in a single moment before queuing.
func IPRateLimitMiddleware(backend IPRateLimiterBackend, keyPrefix string, rpm, burst int) func(http.Handler) http.Handler {
	if rpm <= 0 {
		rpm = 10
	}
	if burst <= 0 {
		burst = rpm
	}
	if keyPrefix == "" {
		// Defensive default — an empty prefix collapses every
		// IPRateLimitMiddleware instance into a single shared
		// keyspace, which is the bug this parameter exists to
		// prevent. Use a sentinel rather than refusing the call so
		// a misconfigured caller still gets rate-limited (just on
		// the shared bucket) rather than crashing the binary.
		keyPrefix = "ip"
	}
	prefix := keyPrefix + ":"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := prefix + clientIP(r)
			if !backend.AllowCtx(r.Context(), key, rpm, burst) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RemoteIPFromRequest extracts the canonical client IP from
// r.RemoteAddr. When chi's RealIP middleware has run earlier it
// has already rewritten RemoteAddr to the originating client's
// address; otherwise it is whatever TCP saw, which is correct for
// a direct connection.
//
// Port stripping is best-effort — a synthesised RemoteAddr that
// lacks a port (some test harnesses) is returned verbatim so the
// limiter still keys on something stable.
//
// Exported so other middleware (captcha verifier, audit log) can
// reuse the same convention. The internal clientIP alias preserves
// existing call sites in this file.
func RemoteIPFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// clientIP keeps the original lower-case alias for in-package call
// sites. New code outside this package should use
// RemoteIPFromRequest.
func clientIP(r *http.Request) string {
	return RemoteIPFromRequest(r)
}

// InProcIPRateLimiter is the in-process fallback used when no Redis
// client is configured. It mirrors the token-bucket math used by the
// tenant-scoped RateLimiter (ratelimit.go) so the two backends agree
// on burst / refill semantics — only the keyspace differs.
//
// Idle buckets are evicted on two paths:
//
//  1. **Opportunistic, on AllowCtx**: every Allow call for the same
//     IP after ipBucketTTL of inactivity drops the old entry and
//     starts fresh. This covers honest traffic patterns.
//  2. **Periodic, via RunSweeper**: a background goroutine purges
//     buckets that have been idle past ipBucketTTL. This is what
//     stops a distributed bot attack — millions of unique source
//     IPs that each appear once and never return — from blowing
//     the map up unboundedly. Callers in long-running processes
//     MUST start the sweeper via RunSweeper; tests may use the
//     synchronous Sweep helper instead for deterministic state.
//
// now is the clock source for refill calculations and the sweeper
// horizon. Tests override it with a synthetic clock so refill
// behaviour and eviction can be asserted deterministically without
// real sleeps; production callers use the default time.Now via
// NewInProcIPRateLimiter.
type InProcIPRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	now     func() time.Time
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

// NewInProcIPRateLimiter returns an empty in-process bucket store
// bound to the real wall clock. Production callers should follow this
// with a `go limiter.RunSweeper(ctx, platform.DefaultIPSweepInterval)`
// so idle buckets do not accumulate under a DDoS-shaped traffic
// pattern; tests typically skip the sweeper and drive Sweep()
// directly off a synthetic clock.
func NewInProcIPRateLimiter() *InProcIPRateLimiter {
	return &InProcIPRateLimiter{
		buckets: make(map[string]*ipBucket),
		now:     time.Now,
	}
}

// AllowCtx implements IPRateLimiterBackend. ctx is accepted for
// signature compatibility with the Redis backend but is not consulted
// — the in-process variant cannot be cancelled mid-Allow.
func (l *InProcIPRateLimiter) AllowCtx(_ context.Context, ip string, rpm, burst int) bool {
	if rpm <= 0 {
		rpm = 10
	}
	if burst <= 0 {
		burst = rpm
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok || now.Sub(b.last) > ipBucketTTL {
		b = &ipBucket{tokens: float64(burst), last: now}
		l.buckets[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * float64(rpm) / 60.0
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Sweep removes every bucket whose last activity is older than
// ipBucketTTL. Safe to call from any goroutine. Returns the number of
// entries dropped so tests and operators can assert on the eviction
// path.
//
// Sweep is the synchronous counterpart to RunSweeper. Production code
// almost always wants RunSweeper (which calls Sweep on a ticker);
// Sweep itself is exported so tests can drive eviction deterministically
// against a synthetic clock.
func (l *InProcIPRateLimiter) Sweep() int {
	if l == nil {
		return 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	dropped := 0
	for ip, b := range l.buckets {
		if now.Sub(b.last) > ipBucketTTL {
			delete(l.buckets, ip)
			dropped++
		}
	}
	return dropped
}

// Size reports the current number of tracked IP buckets. Exposed for
// tests + operator visibility (e.g. an /admin/debug endpoint or a
// Prometheus gauge that wraps it).
func (l *InProcIPRateLimiter) Size() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// RunSweeper drives Sweep on a ticker until ctx is cancelled. It is
// designed to be launched as `go limiter.RunSweeper(ctx, interval)`
// from the API service's main goroutine; the supplied context is the
// same shutdown context that gates the HTTP server, so a clean
// shutdown stops the sweeper too.
//
// interval defaults to DefaultIPSweepInterval when non-positive.
// RunSweeper returns when ctx is cancelled; it does NOT call Sweep
// one final time on shutdown because the limiter's whole state is
// released along with the process.
func (l *InProcIPRateLimiter) RunSweeper(ctx context.Context, interval time.Duration) {
	if l == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultIPSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Sweep()
		}
	}
}

// ipSlidingWindowLua is the Redis Lua script that runs the same
// token-bucket math atomically. Keys[1] is the bucket key, ARGV
// supplies burst / rpm / now-millis / TTL-seconds.
//
// The script intentionally mirrors slidingWindowLua in
// rate_limiter_redis.go rather than reusing it — sharing the script
// across tenant and IP keyspaces would couple two unrelated cache
// invalidation lifetimes onto a single script SHA, and the IP
// variant uses a longer TTL because public-form abuse patterns are
// slower than authenticated tenant bursts.
const ipSlidingWindowLua = `
local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local rpm      = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])
local ttl      = tonumber(ARGV[4])

local bucket = redis.call("HMGET", key, "tokens", "last_ms")
local tokens = tonumber(bucket[1])
local last_ms = tonumber(bucket[2])
if tokens == nil then
  tokens = capacity
  last_ms = now_ms
end
local elapsed_ms = now_ms - last_ms
if elapsed_ms < 0 then elapsed_ms = 0 end
tokens = tokens + (elapsed_ms * rpm / 60000.0)
if tokens > capacity then tokens = capacity end
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call("HMSET", key, "tokens", tostring(tokens), "last_ms", tostring(now_ms))
redis.call("EXPIRE", key, ttl)
return allowed
`

// RedisIPRateLimiter is the multi-replica IP rate limiter. It shares
// keyspace via Redis so every API pod converges on the same per-IP
// ceiling, which is the whole point of running this behind a real
// limiter rather than the in-process fallback.
type RedisIPRateLimiter struct {
	client  *redis.Client
	sha     string
	ownsCli bool
}

// NewRedisIPRateLimiter constructs an IP limiter backed by Redis. It
// dials the supplied URL and loads the Lua script once at boot. The
// returned limiter owns the underlying client; callers must invoke
// Close on shutdown so the TCP pool drains.
func NewRedisIPRateLimiter(ctx context.Context, redisURL string) (*RedisIPRateLimiter, error) {
	if redisURL == "" {
		return nil, errors.New("platform: redis URL required")
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("platform: parse redis url: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("platform: redis ping: %w", err)
	}
	sha, err := client.ScriptLoad(ctx, ipSlidingWindowLua).Result()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("platform: ip script load: %w", err)
	}
	return &RedisIPRateLimiter{client: client, sha: sha, ownsCli: true}, nil
}

// Close releases the Redis connection pool. Safe to call on a nil
// receiver.
func (r *RedisIPRateLimiter) Close() error {
	if r == nil || r.client == nil || !r.ownsCli {
		return nil
	}
	return r.client.Close()
}

// AllowCtx implements IPRateLimiterBackend with the Lua script.
func (r *RedisIPRateLimiter) AllowCtx(ctx context.Context, ip string, rpm, burst int) bool {
	if rpm <= 0 {
		rpm = 10
	}
	if burst <= 0 {
		burst = rpm
	}
	nowMS := time.Now().UnixMilli()
	ttl := int64(ipBucketTTL.Seconds())
	key := "kapp:rl:ip:" + ip
	args := []any{burst, rpm, nowMS, ttl}
	res, err := r.client.EvalSha(ctx, r.sha, []string{key}, args...).Int64()
	if err != nil && isNoScript(err) {
		res, err = r.client.Eval(ctx, ipSlidingWindowLua, []string{key}, args...).Int64()
	}
	if err != nil {
		// Fail open. A Redis hiccup should not lock every public
		// form submitter out of the site; the reverse proxy is the
		// outer ceiling on abusive traffic regardless.
		return true
	}
	return res == 1
}
