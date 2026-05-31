package eventrouter

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Limiter is the shared per-`(tenant_id, extension_id)` rate
// budget consumed by BOTH the B4 event router (event_delivery
// kind in marketplace_dispatch_log) AND the B3 agent-tool
// dispatcher (tool_invoke kind). The budget is a property of the
// extension's webhook receiver — its ingress capacity — not of
// the dispatcher's call path, so both consumers share the same
// bucket.
//
// The implementation is a classic token bucket: each bucket
// holds at most `capacity` tokens, refills at `refillPerSecond`
// tokens / second, and Allow consumes one token if available
// (returning true) or refuses the call (returning false). Refills
// are computed lazily on each Allow so an idle bucket pays zero
// background cost.
//
// The bucket is keyed by `(tenant_id, extension_id)`. Buckets are
// created on first use and stay resident for the lifetime of the
// process; the steady-state memory cost is one bucket per
// installed extension per tenant. A B6-scale follow-up may add a
// time-based eviction pass — today we accept the worst-case
// memory cost (≈ 100 bytes per bucket × #installs) as bounded.
//
// Concurrency: Allow takes a write lock on the per-bucket mutex,
// so two goroutines invoking Allow on the same bucket serialise
// inside Allow. Two goroutines on different buckets do NOT
// serialise (each bucket has its own mutex). The Limiter's outer
// map is guarded by its own RWMutex so bucket creation and
// lookup race-free with concurrent dispatchers.
type Limiter struct {
	// mu guards the buckets map. Read-locked on the hot path
	// (every Allow looks up the bucket); write-locked only on
	// first-use bucket creation.
	mu      sync.RWMutex
	buckets map[bucketKey]*bucket

	// defaultRPM is the fallback rate when no per-extension
	// override is supplied via Allow's `rpm` parameter. Today
	// the only override path is the marketplace_extensions
	// .rate_limit_rpm column read by the caller; this struct
	// itself is rpm-agnostic so tests can drive it without a
	// DB.
	defaultRPM int

	// now is the wall-clock source. Tests override to a stub
	// for deterministic refill calculations.
	now func() time.Time
}

// NewLimiter constructs a Limiter with the supplied default
// rate (requests per minute) and clock. now=nil falls back to
// time.Now.
func NewLimiter(defaultRPM int, now func() time.Time) *Limiter {
	if defaultRPM < 1 {
		defaultRPM = 100
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		buckets:    make(map[bucketKey]*bucket),
		defaultRPM: defaultRPM,
		now:        now,
	}
}

// DefaultRPM exposes the limiter's fallback rate. Callers that
// resolve a per-extension override (from
// marketplace_extensions.rate_limit_rpm) pass the resolved value
// to Allow directly; this getter exists for tests + diagnostic
// logging.
func (l *Limiter) DefaultRPM() int { return l.defaultRPM }

// Allow consumes one token from the `(tenantID, extensionID)`
// bucket if available. rpm is the per-extension override (use
// l.DefaultRPM() to fall back to the limiter's default).
// Returns true iff a token was consumed.
//
// Tokens refill at rpm/60 tokens per second; capacity is rpm
// (i.e. one full minute's worth of burst). A bucket that has
// been idle for >= 60s observes a full bucket on next Allow.
func (l *Limiter) Allow(tenantID, extensionID uuid.UUID, rpm int) bool {
	if rpm < 1 {
		rpm = l.defaultRPM
	}
	key := bucketKey{Tenant: tenantID, Extension: extensionID}

	// Fast path: read-lock to look up an existing bucket.
	l.mu.RLock()
	b, ok := l.buckets[key]
	l.mu.RUnlock()

	if !ok {
		// Slow path: write-lock to create. Double-check
		// inside the lock to avoid the classic CAS race
		// where two goroutines both observe `!ok` under
		// the read lock and both try to create.
		l.mu.Lock()
		if existing, ok2 := l.buckets[key]; ok2 {
			b = existing
		} else {
			b = newBucket(rpm, l.now())
			l.buckets[key] = b
		}
		l.mu.Unlock()
	}

	return b.consume(rpm, l.now())
}

// bucketKey is the composite map key. The two-UUID composition
// (instead of a string) keeps the lookup allocation-free and
// avoids the cost of fmt.Sprintf("%s|%s", ...) on the hot path.
type bucketKey struct {
	Tenant    uuid.UUID
	Extension uuid.UUID
}

// bucket is a single token bucket. Each Allow takes the per-
// bucket mutex; two goroutines invoking Allow on different
// buckets do not contend.
type bucket struct {
	mu sync.Mutex
	// tokens is the current count of available tokens.
	// Float because refill is fractional (rpm/60 per second).
	tokens float64
	// lastRefill is the wall-clock time we last credited
	// refill. On each consume() we credit
	// (now - lastRefill).Seconds() * (rpm/60) tokens, cap at
	// capacity, and update lastRefill.
	lastRefill time.Time
}

// newBucket constructs a fresh, full bucket. capacity == rpm so
// a brand-new bucket can absorb one full minute's burst.
func newBucket(rpm int, now time.Time) *bucket {
	return &bucket{
		tokens:     float64(rpm),
		lastRefill: now,
	}
}

// consume credits any owed refill and then attempts to deduct
// one token. Returns true iff the deduction succeeded.
func (b *bucket) consume(rpm int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Credit refill. Fractional refill is fine — the next
	// consume sees the accumulated total.
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		refilled := elapsed * float64(rpm) / 60.0
		b.tokens += refilled
		if b.tokens > float64(rpm) {
			b.tokens = float64(rpm)
		}
		b.lastRefill = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
