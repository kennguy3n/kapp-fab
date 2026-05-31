package eventrouter

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLimiter_AllowConsumesUntilEmpty(t *testing.T) {
	// rpm=3 → bucket capacity 3, refill 0.05 tok/sec.
	// At a frozen clock we should be able to consume 3 in
	// a row and then get refused on the 4th.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lim := NewLimiter(3, func() time.Time { return now })
	tenant := uuid.New()
	ext := uuid.New()

	for i := 0; i < 3; i++ {
		if !lim.Allow(tenant, ext, 3) {
			t.Fatalf("attempt %d: expected Allow=true, got false (bucket should still have tokens)", i+1)
		}
	}
	if lim.Allow(tenant, ext, 3) {
		t.Fatalf("4th attempt: expected Allow=false (bucket empty), got true")
	}
}

func TestLimiter_RefillsOverTime(t *testing.T) {
	// rpm=60 → 1 tok/sec. Drain to 0, advance clock 1s,
	// expect one new token.
	current := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lim := NewLimiter(60, func() time.Time { return current })
	tenant := uuid.New()
	ext := uuid.New()

	// Drain the bucket.
	for i := 0; i < 60; i++ {
		if !lim.Allow(tenant, ext, 60) {
			t.Fatalf("drain attempt %d unexpectedly refused", i+1)
		}
	}
	if lim.Allow(tenant, ext, 60) {
		t.Fatalf("post-drain Allow returned true; bucket should be empty")
	}

	// Advance 1 second — should refill 1 token.
	current = current.Add(1 * time.Second)
	if !lim.Allow(tenant, ext, 60) {
		t.Fatalf("after 1s wait, expected Allow=true (refilled), got false")
	}
	// 1 token consumed; next call refused.
	if lim.Allow(tenant, ext, 60) {
		t.Fatalf("immediate follow-up call should be refused")
	}
}

func TestLimiter_FullRefillCappedAtCapacity(t *testing.T) {
	// rpm=10, drain 1 token, idle for an hour — refill
	// should cap at rpm (10), not balloon to 10*60.
	current := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lim := NewLimiter(10, func() time.Time { return current })
	tenant := uuid.New()
	ext := uuid.New()

	if !lim.Allow(tenant, ext, 10) {
		t.Fatalf("first Allow refused on fresh bucket")
	}

	// Idle 1 hour.
	current = current.Add(1 * time.Hour)
	// Should now allow exactly 10 calls (capacity cap).
	for i := 0; i < 10; i++ {
		if !lim.Allow(tenant, ext, 10) {
			t.Fatalf("post-hour drain attempt %d refused; bucket should be full again", i+1)
		}
	}
	if lim.Allow(tenant, ext, 10) {
		t.Fatalf("11th attempt accepted; capacity cap not enforced")
	}
}

func TestLimiter_BucketsAreIndependentPerTenantExtension(t *testing.T) {
	// Two distinct (tenant, ext) pairs share no token state.
	current := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lim := NewLimiter(1, func() time.Time { return current })
	tenantA := uuid.New()
	tenantB := uuid.New()
	extA := uuid.New()
	extB := uuid.New()

	// Drain tenantA/extA.
	if !lim.Allow(tenantA, extA, 1) {
		t.Fatalf("A/A first allow refused")
	}
	if lim.Allow(tenantA, extA, 1) {
		t.Fatalf("A/A second allow accepted; should be drained")
	}

	// tenantB/extA still has a token (different tenant).
	if !lim.Allow(tenantB, extA, 1) {
		t.Fatalf("B/A first allow refused; bucket should be independent of A/A")
	}
	// tenantA/extB still has a token (different ext).
	if !lim.Allow(tenantA, extB, 1) {
		t.Fatalf("A/B first allow refused; bucket should be independent of A/A")
	}
}

func TestLimiter_NewLimiterDefaultsRPMOnZero(t *testing.T) {
	lim := NewLimiter(0, nil)
	if lim.DefaultRPM() != 100 {
		t.Fatalf("zero default rpm should fall through to 100; got %d", lim.DefaultRPM())
	}
}

func TestLimiter_AllowWithRPMBelowOneUsesDefault(t *testing.T) {
	// rpm=0 on the per-call argument should fall back to
	// the limiter's default rather than refuse outright.
	current := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lim := NewLimiter(2, func() time.Time { return current })
	tenant := uuid.New()
	ext := uuid.New()
	if !lim.Allow(tenant, ext, 0) {
		t.Fatalf("Allow with rpm=0 should fall back to default (2) and succeed on fresh bucket")
	}
}
