package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInProcIPRateLimiter_BurstAndRefill(t *testing.T) {
	l := NewInProcIPRateLimiter()
	clock := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return clock }
	ctx := context.Background()
	ip := "192.0.2.10"
	rpm := 60 // 1 token per second
	burst := 3

	// Burst should allow exactly `burst` requests in the first window.
	for i := 0; i < burst; i++ {
		if !l.AllowCtx(ctx, ip, rpm, burst) {
			t.Fatalf("burst %d: expected allow, got deny", i)
		}
	}
	if l.AllowCtx(ctx, ip, rpm, burst) {
		t.Fatal("expected deny once bucket is empty")
	}

	// Advance 999ms — still under one token, must remain denied.
	clock = clock.Add(999 * time.Millisecond)
	if l.AllowCtx(ctx, ip, rpm, burst) {
		t.Fatal("expected deny at +999ms (less than 1 token refilled)")
	}

	// At +1.001s the bucket has refilled exactly one token.
	clock = clock.Add(2 * time.Millisecond)
	if !l.AllowCtx(ctx, ip, rpm, burst) {
		t.Fatal("expected allow at +1s (one token refilled)")
	}
	if l.AllowCtx(ctx, ip, rpm, burst) {
		t.Fatal("expected deny immediately after spending refilled token")
	}

	// Idling for 5s refills to burst (capacity), not 5 tokens.
	clock = clock.Add(5 * time.Second)
	for i := 0; i < burst; i++ {
		if !l.AllowCtx(ctx, ip, rpm, burst) {
			t.Fatalf("post-idle burst %d: expected allow, got deny", i)
		}
	}
	if l.AllowCtx(ctx, ip, rpm, burst) {
		t.Fatal("expected deny once post-idle burst exhausted")
	}
}

func TestInProcIPRateLimiter_PerIPIsolation(t *testing.T) {
	l := NewInProcIPRateLimiter()
	ctx := context.Background()
	if !l.AllowCtx(ctx, "203.0.113.1", 10, 1) {
		t.Fatal("first IP should be allowed once")
	}
	if l.AllowCtx(ctx, "203.0.113.1", 10, 1) {
		t.Fatal("first IP should be denied second time (burst=1)")
	}
	// Different IP must have its own bucket.
	if !l.AllowCtx(ctx, "203.0.113.2", 10, 1) {
		t.Fatal("second IP should be allowed (separate bucket)")
	}
}

func TestInProcIPRateLimiter_IdleEviction(t *testing.T) {
	l := NewInProcIPRateLimiter()
	clock := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return clock }
	ctx := context.Background()
	ip := "203.0.113.5"
	// Drain the bucket.
	if !l.AllowCtx(ctx, ip, 1, 1) {
		t.Fatal("initial allow failed")
	}
	if l.AllowCtx(ctx, ip, 1, 1) {
		t.Fatal("second call should be denied")
	}
	// Advance past the idle TTL — the next Allow should reset the
	// bucket to burst capacity even though only 1 token would
	// otherwise have refilled.
	clock = clock.Add(2 * ipBucketTTL)
	if !l.AllowCtx(ctx, ip, 1, 1) {
		t.Fatal("after idle eviction the bucket should refill to burst")
	}
}

func TestIPRateLimitMiddleware_Allows(t *testing.T) {
	l := NewInProcIPRateLimiter()
	mw := IPRateLimitMiddleware(l, 60, 5)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest(http.MethodPost, "/submit", nil)
	req.RemoteAddr = "198.51.100.7:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("handler not invoked")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
}

func TestIPRateLimitMiddleware_RejectsAfterBurst(t *testing.T) {
	l := NewInProcIPRateLimiter()
	mw := IPRateLimitMiddleware(l, 60, 2)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ip := "198.51.100.20:1111"
	hit := func() int {
		req := httptest.NewRequest(http.MethodPost, "/submit", nil)
		req.RemoteAddr = ip
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := hit(); got != http.StatusOK {
		t.Fatalf("burst 1 status = %d, want 200", got)
	}
	if got := hit(); got != http.StatusOK {
		t.Fatalf("burst 2 status = %d, want 200", got)
	}
	if got := hit(); got != http.StatusTooManyRequests {
		t.Fatalf("burst 3 status = %d, want 429", got)
	}
}

func TestClientIP_SplitsHostPort(t *testing.T) {
	tests := []struct {
		remote string
		want   string
	}{
		{"10.0.0.1:1234", "10.0.0.1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"unparsed", "unparsed"},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tc.remote
		if got := clientIP(req); got != tc.want {
			t.Fatalf("clientIP(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}
