package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// TestTruncateBodyValidUTF8 ensures the error-body truncation never
// splits a multi-byte UTF-8 character, so the result is always valid
// UTF-8 even when the 512-byte cut falls mid-rune.
func TestTruncateBodyValidUTF8(t *testing.T) {
	// "é" is two bytes (0xC3 0xA9). A run of 511 ASCII bytes followed by
	// "é" puts the 512-byte boundary in the middle of the rune.
	body := strings.Repeat("a", 511) + "é" + strings.Repeat("b", 100)
	got := truncateBody([]byte(body))
	if !utf8.ValidString(got) {
		t.Fatalf("truncateBody produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got[len(got)-4:])
	}
}

// TestTruncateBodyShort leaves short bodies untouched (aside from
// trimming surrounding whitespace).
func TestTruncateBodyShort(t *testing.T) {
	if got := truncateBody([]byte("  hello  ")); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestReadCappedBody rejects a body that exceeds the in-memory cap
// rather than buffering it whole.
func TestReadCappedBody(t *testing.T) {
	if _, err := readCappedBody(strings.NewReader(strings.Repeat("x", maxResponseBytes+10))); err == nil {
		t.Error("expected error for oversized body")
	}
	if got, err := readCappedBody(strings.NewReader("small")); err != nil || string(got) != "small" {
		t.Errorf("readCappedBody small: got %q err %v", got, err)
	}
}

// TestOAuthTokenCacheSingleflight verifies that many concurrent resolves
// for the same connection collapse into exactly one refresh-token grant.
// Without deduplication each goroutine would observe a cache miss and run
// its own grant, which fails on rotating providers (QuickBooks/Sage)
// because the first exchange invalidates the shared refresh token.
func TestOAuthTokenCacheSingleflight(t *testing.T) {
	var grants int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&grants, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","refresh_token":"rotated","expires_in":3600}`))
	}))
	defer srv.Close()

	cache := &oauthTokenCache{}
	cfg := oauth2Config{TokenURL: srv.URL, ClientID: "cid", ClientSecret: "secret", RefreshToken: "refresh-abc"}

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	tokens := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			tokens[idx], _, errs[idx] = cache.resolve(context.Background(), srv.Client(), cfg)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: resolve error: %v", i, err)
		}
		if tokens[i] != "tok-1" {
			t.Fatalf("worker %d: got token %q, want tok-1", i, tokens[i])
		}
	}
	if got := atomic.LoadInt32(&grants); got != 1 {
		t.Fatalf("expected exactly 1 token grant under concurrency, got %d", got)
	}
}

// TestOAuthCacheTTL checks the cached lifetime keeps a positive safety
// margin for short-lived tokens and falls back when no lifetime is given.
func TestOAuthCacheTTL(t *testing.T) {
	cases := []struct {
		expiresIn int
		want      time.Duration
	}{
		{expiresIn: 0, want: oauthBridgeTTL},
		{expiresIn: 3600, want: 3600*time.Second - oauthTokenExpirySkew},
		{expiresIn: 10, want: 10*time.Second - 5*time.Second},
		{expiresIn: 1, want: 1*time.Second - 500*time.Millisecond},
	}
	for _, c := range cases {
		got := oauthCacheTTL(oauth2Token{ExpiresIn: c.expiresIn})
		if got != c.want {
			t.Errorf("oauthCacheTTL(expires_in=%d) = %v, want %v", c.expiresIn, got, c.want)
		}
		if got <= 0 {
			t.Errorf("oauthCacheTTL(expires_in=%d) must be positive, got %v", c.expiresIn, got)
		}
	}
}
