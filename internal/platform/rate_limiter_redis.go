package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RateOutcome is the three-way result of a rate-limit check. The bool
// Allow/AllowCtx API collapses it to allowed/denied; the middleware
// uses the richer form so it can distinguish "over the limit" (429)
// from "limiter backend unavailable under fail-closed" (503).
type RateOutcome int

const (
	// RateAllowed means the request is within the tenant's budget (or
	// the backend errored while the limiter is in fail-open mode).
	RateAllowed RateOutcome = iota
	// RateDenied means the tenant is over its configured limit → 429.
	RateDenied
	// RateUnavailable means the Redis backend is unreachable AND the
	// limiter is configured fail-closed (production) → 503.
	RateUnavailable
)

// slidingWindowLua is an atomic sliding-window token bucket. The
// script takes:
//
//	KEYS[1] = bucket key
//	ARGV[1] = capacity (burst)
//	ARGV[2] = refill rate in tokens per minute (the original RPM)
//	ARGV[3] = now (unix millis)
//	ARGV[4] = TTL in seconds
//
// Stored state (HASH):
//
//	tokens  = current float token count, stored as string
//	last_ms = unix millis of last update
//
// Returns 1 if allowed, 0 otherwise.
//
// Refill math: tokens added over elapsed_ms = elapsed_ms * RPM /
// 60000. We deliberately stay in milliseconds throughout so a 1ms
// gap does not refill a meaningful fraction of a token at sane
// RPMs. Keeping the script in redis side-steps the round-trip
// latency and keeps the check-and-decrement atomic across
// replicas.
const slidingWindowLua = `
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

// RedisRateLimiter is a Redis-backed drop-in for RateLimiter that
// shares state across multiple API + agent-tools replicas. Keys auto-
// expire after the idle TTL so the zero-idle-cost invariant from the
// in-process limiter carries over: a tenant that stops sending
// traffic stops paying for storage in Redis too.
type RedisRateLimiter struct {
	client *redis.Client
	cfg    RateLimitConfig
	sha    string // EVALSHA of slidingWindowLua; falls back to EVAL on NOSCRIPT
	// failClosed selects the behaviour when Redis is unreachable.
	// false (default): fail OPEN — a transient outage allows traffic
	// through so a Redis blip does not take the whole API down. true
	// (production, set via WithFailClosed): fail CLOSED — an
	// unreachable backend returns 503 so an attacker cannot bypass
	// the limiter by knocking Redis over.
	failClosed bool
}

// NewRedisRateLimiter constructs a Redis-backed limiter. redisURL
// accepts the full redis://user:pass@host:port/db form; a zero
// cfg.RequestsPerMinute is replaced by DefaultRateLimitConfig.
func NewRedisRateLimiter(ctx context.Context, redisURL string, cfg RateLimitConfig) (*RedisRateLimiter, error) {
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
	if cfg.RequestsPerMinute <= 0 {
		cfg = DefaultRateLimitConfig()
	}
	if cfg.BurstSize <= 0 {
		cfg.BurstSize = cfg.RequestsPerMinute
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	sha, err := client.ScriptLoad(ctx, slidingWindowLua).Result()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("platform: redis script load: %w", err)
	}
	return &RedisRateLimiter{client: client, cfg: cfg, sha: sha}, nil
}

// WithFailClosed sets the unreachable-backend behaviour and returns
// the receiver for chaining at wiring time. Production wiring calls
// WithFailClosed(true) so a Redis outage returns 503 instead of
// silently allowing every request through; development leaves it at
// the fail-open default. Safe on a nil receiver so callers can chain
// it off a constructor that may have returned nil.
func (r *RedisRateLimiter) WithFailClosed(failClosed bool) *RedisRateLimiter {
	if r == nil {
		return nil
	}
	r.failClosed = failClosed
	return r
}

// Close releases the Redis connection pool. Safe to call on a nil
// receiver.
func (r *RedisRateLimiter) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

// Allow mirrors RateLimiter.Allow. Context is bound to the enclosing
// request via AllowCtx; this shim keeps the signature compatible so
// existing callers that do not thread a context do not change.
func (r *RedisRateLimiter) Allow(tenantID uuid.UUID, rpm, burst int) bool {
	return r.AllowCtx(context.Background(), tenantID, rpm, burst)
}

// AllowCtx is the context-aware variant. It preserves the historical
// bool contract: a request is "allowed" unless it is over the limit,
// or — only in fail-closed mode — the backend is unreachable. Callers
// that need to distinguish the 429 and 503 cases use Check instead.
func (r *RedisRateLimiter) AllowCtx(ctx context.Context, tenantID uuid.UUID, rpm, burst int) bool {
	return r.Check(ctx, tenantID, rpm, burst) == RateAllowed
}

// Check runs the limiter and returns the three-way outcome. On a
// backend error it folds the fail-open/fail-closed policy in one
// place: fail-open returns RateAllowed (a transient Redis blip should
// not take the API down), fail-closed returns RateUnavailable so the
// middleware can answer 503. Either way the error is logged at WARN so
// operators can correlate against Redis health.
func (r *RedisRateLimiter) Check(ctx context.Context, tenantID uuid.UUID, rpm, burst int) RateOutcome {
	wantRPM := chooseInt(rpm, r.cfg.RequestsPerMinute)
	wantBurst := chooseInt(burst, r.cfg.BurstSize)
	nowMS := time.Now().UnixMilli()
	ttl := int64(r.cfg.IdleTimeout.Seconds())
	if ttl <= 0 {
		ttl = 600
	}
	key := fmt.Sprintf("kapp:rl:%s", tenantID.String())
	args := []any{wantBurst, wantRPM, nowMS, ttl}
	// Try EVALSHA first; on NOSCRIPT (Redis restarted, script cache
	// dropped), fall back to EVAL which also repopulates the cache.
	res, err := r.client.EvalSha(ctx, r.sha, []string{key}, args...).Int64()
	if err != nil {
		if isNoScript(err) {
			res, err = r.client.Eval(ctx, slidingWindowLua, []string{key}, args...).Int64()
		}
	}
	if err != nil {
		if r.failClosed {
			// Fail CLOSED (production): an unreachable backend must
			// not become a rate-limit bypass. The middleware turns
			// this into a 503 so abusive traffic cannot ride a Redis
			// outage past the limiter.
			slog.Default().Warn("redis rate limiter unreachable, failing closed",
				slog.String("tenant_id", tenantID.String()),
				slog.String("err", err.Error()),
			)
			return RateUnavailable
		}
		// Fail OPEN (development): a transient Redis outage should
		// not block every request. The reverse proxy is still the
		// outer ceiling on abusive traffic.
		slog.Default().Warn("redis rate limiter unreachable, failing open",
			slog.String("tenant_id", tenantID.String()),
			slog.String("err", err.Error()),
		)
		return RateAllowed
	}
	if res == 1 {
		return RateAllowed
	}
	return RateDenied
}

// isNoScript matches redis NOSCRIPT errors without binding to a
// specific go-redis error type, so a go-redis upgrade that renames
// the sentinel does not silently break the fallback path.
func isNoScript(err error) bool {
	return err != nil && len(err.Error()) >= 8 && err.Error()[:8] == "NOSCRIPT"
}

// RedisRateLimitMiddleware mirrors RateLimitMiddleware but backs
// onto a RedisRateLimiter. Kept as a separate function so callers
// can switch backends with a single wiring-time decision rather
// than forcing every handler to indirect through an interface.
func RedisRateLimitMiddleware(limiter *RedisRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := TenantFromContext(r.Context())
			if t == nil {
				http.Error(w, "tenant context missing", http.StatusInternalServerError)
				return
			}
			rpm, burst := extractRateLimitOverrides(t.Quota)
			switch limiter.Check(r.Context(), t.ID, rpm, burst) {
			case RateDenied:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			case RateUnavailable:
				// Fail-closed (production): the limiter backend is
				// unreachable, so we refuse the request rather than
				// let it bypass the limiter. 503 + Retry-After tells
				// well-behaved clients to back off and retry.
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limiter temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
