package captcha

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// solveChallenge brute-forces an answer string satisfying the
// difficulty constraint for the supplied challenge blob. Used in
// tests to round-trip an issued challenge through Verify. The
// search runs single-threaded with a small uint64 counter — at
// difficulty=8 (test default) we expect ~256 attempts so this is
// trivially fast.
func solveChallenge(t *testing.T, blob string, difficulty uint8) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	for i := uint64(0); i < 1<<24; i++ {
		ans := encodeUint(i)
		h := sha256.New()
		h.Write(raw)
		h.Write([]byte(ans))
		digest := h.Sum(nil)
		if hasLeadingZeroBits(digest, difficulty) {
			return ans
		}
	}
	t.Fatalf("could not solve challenge at difficulty %d within 2^24 attempts", difficulty)
	return ""
}

func encodeUint(n uint64) string {
	var b [16]byte
	for i := 0; i < len(b); i++ {
		b[i] = byte(n)
		n >>= 8
		if n == 0 {
			return base64.RawURLEncoding.EncodeToString(b[:i+1])
		}
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func TestPoW_RoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v := NewPoWVerifier(key, 8, time.Minute)
	blob, err := v.IssueChallenge()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	answer := solveChallenge(t, blob, 8)
	token := blob + "." + answer
	out, err := v.Verify(context.Background(), token, "")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if !out.Success {
		t.Fatalf("expected success, got %+v", out)
	}
	if out.Score != 1.0 {
		t.Errorf("expected score=1.0, got %v", out.Score)
	}
}

func TestPoW_ReplayRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v := NewPoWVerifier(key, 8, time.Minute)
	blob, _ := v.IssueChallenge()
	answer := solveChallenge(t, blob, 8)
	token := blob + "." + answer
	if _, err := v.Verify(context.Background(), token, ""); err != nil {
		t.Fatalf("first verify err: %v", err)
	}
	_, err := v.Verify(context.Background(), token, "")
	if !errors.Is(err, ErrTokenReplay) {
		t.Errorf("expected ErrTokenReplay on replay, got %v", err)
	}
}

func TestPoW_DifficultyTamperingRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v := NewPoWVerifier(key, 16, time.Minute)
	blob, _ := v.IssueChallenge()
	raw, _ := base64.RawURLEncoding.DecodeString(blob)
	// Mutate the difficulty byte (offset 16) to lower it and
	// then submit. HMAC over the original header should reject.
	raw[16] = 1
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	// Provide a trivially-easy answer that would solve at the
	// fake difficulty=1 but not the original 16.
	answer := solveChallenge(t, tampered, 1)
	token := tampered + "." + answer
	out, _ := v.Verify(context.Background(), token, "")
	if out.Success {
		t.Error("verify should reject tampered difficulty")
	}
	if !strings.Contains(strings.Join(out.ErrorCodes, ","), "bad-hmac") {
		t.Errorf("expected bad-hmac error code, got %v", out.ErrorCodes)
	}
}

func TestPoW_ExpiredChallengeRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v := NewPoWVerifier(key, 8, time.Minute)
	// Stub the clock so issuance happens at t=0 and verification
	// at t=2min — past the 1min expiry window without burning
	// wall-clock seconds in the test suite.
	clockAt := time.Unix(1_700_000_000, 0).UTC()
	v.SetClock(func() time.Time { return clockAt })
	blob, _ := v.IssueChallenge()
	answer := solveChallenge(t, blob, 8)
	clockAt = clockAt.Add(2 * time.Minute)
	token := blob + "." + answer
	_, err := v.Verify(context.Background(), token, "")
	if !errors.Is(err, ErrTokenStale) {
		t.Errorf("expected ErrTokenStale, got %v", err)
	}
}

func TestPoW_BadAnswerRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v := NewPoWVerifier(key, 16, time.Minute)
	blob, _ := v.IssueChallenge()
	// "wrong" answer that almost certainly does not produce 16
	// leading zero bits of SHA-256.
	token := blob + ".wrong"
	out, err := v.Verify(context.Background(), token, "")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if out.Success {
		t.Errorf("expected reject on wrong answer, got %+v", out)
	}
	if !strings.Contains(strings.Join(out.ErrorCodes, ","), "score-below-threshold") {
		t.Errorf("expected score-below-threshold error code, got %v", out.ErrorCodes)
	}
}

func TestPoW_MalformedTokenRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v := NewPoWVerifier(key, 16, time.Minute)
	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"no separator", "justachallenge"},
		{"too many parts", "a.b.c"},
		{"bad base64", "@@@.answer"},
		{"short challenge", base64.RawURLEncoding.EncodeToString([]byte("short")) + ".answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := v.Verify(context.Background(), tc.token, "")
			if err != nil {
				t.Fatalf("verify err: %v", err)
			}
			if out.Success {
				t.Errorf("expected reject for %q, got %+v", tc.name, out)
			}
		})
	}
}

func TestHasLeadingZeroBits(t *testing.T) {
	cases := []struct {
		bytes []byte
		bits  uint8
		want  bool
	}{
		{[]byte{0x00}, 8, true},
		{[]byte{0x00}, 9, false}, // ran off the end
		{[]byte{0x01}, 7, true},  // 0000 0001 → 7 leading zeros
		{[]byte{0x01}, 8, false},
		{[]byte{0x00, 0x7F}, 9, true},  // 0000 0000 0111 1111 → 9 zeros
		{[]byte{0x00, 0x7F}, 10, false},
		{[]byte{0xFF}, 0, true},     // zero-bit constraint always passes
		{[]byte{0xFF}, 1, false},
	}
	for _, tc := range cases {
		got := hasLeadingZeroBits(tc.bytes, tc.bits)
		if got != tc.want {
			t.Errorf("hasLeadingZeroBits(%x, %d) = %v want %v", tc.bytes, tc.bits, got, tc.want)
		}
	}
}

func TestPoW_NewFromConfigRejectsShortKey(t *testing.T) {
	_, err := NewFromConfig(Config{
		Provider:   "pow",
		PoWHMACKey: []byte("short"),
	})
	if err == nil {
		t.Error("expected error for short HMAC key")
	}
}

func TestPoW_SetClockConcurrentWithVerify(_ *testing.T) {
	// Regression test for Devin Review finding
	// ANALYSIS_pr-review-job-104ce38940214afeb0aedce5b15ff028_0005:
	// SetClock used to write v.now without synchronization,
	// racing with Verify reads on request goroutines. The atomic.
	// Pointer-backed nowFn must let SetClock and verifier reads
	// run concurrently without -race triggering. The test
	// completes in well under a second under -race because the
	// LRU cache lookup is in-memory.
	key := []byte("0123456789abcdef0123456789abcdef")
	v := NewPoWVerifier(key, 8, time.Minute)
	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				v.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
			}
		}
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = v.IssueChallenge()
			}
		}
	}()
	// Let the two goroutines race for ~5 ms; with -race the
	// runtime will flag any unsynchronized access.
	time.Sleep(5 * time.Millisecond)
	close(stop)
}

func TestFactory_UnknownProvider(t *testing.T) {
	_, err := NewFromConfig(Config{Provider: "magic"})
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestFactory_DisabledProviderAlwaysGrants(t *testing.T) {
	v, err := NewFromConfig(Config{Provider: "disabled"})
	if err != nil {
		t.Fatalf("factory err: %v", err)
	}
	out, err := v.Verify(context.Background(), "any", "")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if !out.Success {
		t.Error("disabled verifier should always grant")
	}
}
