package captcha

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// PoWVerifier issues and verifies Hashcash-style proof-of-work
// challenges. The provider exists for deployments that:
//
//   - Don't want a third-party captcha dependency (regulated
//     environments, on-prem deployments without outbound network
//     access to challenges.cloudflare.com).
//   - Want a captcha layer with zero per-solve cost (Turnstile is
//     free at the moment but Cloudflare reserves the right to
//     change that).
//   - Want a captcha that's invisible to the user in the common
//     case (a 16-bit-difficulty PoW takes ~50ms in JS — below the
//     perceptible latency floor — but costs an attacker ~$0.01 per
//     1000 solves on rented CPU, which on its own is sufficient
//     friction for most abuse classes).
//
// # Protocol
//
// 1. Client requests a challenge: server returns a base64-encoded
//    envelope (issued_at, expiry, difficulty, nonce, HMAC). The
//    HMAC is computed with a server-side key the client never
//    sees.
// 2. Client searches for an answer string `a` such that the
//    SHA-256 of `envelope || a` has at least `difficulty` leading
//    zero bits.
// 3. Client submits the envelope + answer back. Server
//    re-validates the HMAC (so a client can't fudge the
//    difficulty), checks expiry, recomputes the hash, and
//    confirms the leading-zero count.
//
// # Threat model
//
// - Replay: each envelope carries a unique nonce that's recorded
//   in the replay LRU on successful verification. The expiry
//   window is short (default 5 min) so the LRU stays bounded.
// - Difficulty tampering: HMAC-SHA256 over (issued_at, expiry,
//   difficulty, nonce) means a client can't lower the difficulty
//   without breaking the HMAC.
// - Cost asymmetry: at difficulty=16 the expected number of
//   hashes is 2^16 = 65536, which a modern CPU clears in <100ms.
//   The server-side verification is a single hash — orders of
//   magnitude cheaper than the client work. The asymmetry is the
//   whole point.
type PoWVerifier struct {
	hmacKey     []byte
	difficulty  uint8
	expiry      time.Duration
	replayCache *platform.LRUCache
	// nowFn is the clock source used for issuance and expiry
	// checks.  Production wires time.Now via the default
	// constructor; tests can swap in a stubbed clock via
	// SetClock to exercise expiry-driven branches deterministically.
	//
	// Stored behind an atomic.Pointer so SetClock is safe to call
	// concurrently with Verify / IssueChallenge — a plain function
	// field would race because Verify reads the field from request
	// goroutines while a test might rotate the clock from a
	// different goroutine.  See Devin Review finding
	// ANALYSIS_pr-review-job-104ce38940214afeb0aedce5b15ff028_0005.
	nowFn atomic.Pointer[func() time.Time]
}

// NewPoWVerifier returns a PoWVerifier signed with hmacKey. Empty
// hmacKey is rejected by NewFromConfig at boot; we don't reject
// here so tests can construct verifiers with deterministic short
// keys.
//
// difficulty is the number of leading zero bits required in the
// solution hash. Each additional bit doubles the expected number
// of hashes a client has to compute, so the right knob to turn:
//
//   - 14 bits: ~25ms on JS, ~$0.0025 per 1000 solves
//   - 16 bits: ~100ms on JS, ~$0.01 per 1000 solves (recommended)
//   - 18 bits: ~400ms on JS, ~$0.04 per 1000 solves (high-value endpoints)
//   - 20 bits: ~1.6s on JS, ~$0.16 per 1000 solves (login pages under attack)
//
// expiry is how long a challenge is valid for after issuance.
// Default 5 minutes; tune upward for slow networks, downward if
// abuse rates push you to want tighter replay windows.
func NewPoWVerifier(hmacKey []byte, difficulty uint8, expiry time.Duration) *PoWVerifier {
	if difficulty == 0 {
		difficulty = 16
	}
	if expiry == 0 {
		expiry = 5 * time.Minute
	}
	// Replay cache: each successful verification records the
	// envelope's nonce so the same solved challenge can't be
	// replayed. Bound matches typical anti-abuse needs without
	// pinning much memory.
	rc := platform.NewLRUCache(4096, expiry*2)
	v := &PoWVerifier{
		hmacKey:     hmacKey,
		difficulty:  difficulty,
		expiry:      expiry,
		replayCache: rc,
	}
	defaultNow := time.Now
	v.nowFn.Store(&defaultNow)
	return v
}

// now returns the current time via the (possibly stubbed) clock
// source. Safe for concurrent callers — the pointer load is atomic.
func (v *PoWVerifier) now() time.Time {
	if fn := v.nowFn.Load(); fn != nil {
		return (*fn)()
	}
	return time.Now()
}

// SetClock swaps the verifier's clock source.  Test-only seam:
// production callers should not change the clock after
// construction.  Passing nil restores time.Now. Safe to call
// concurrently with Verify and IssueChallenge — the underlying
// store is an atomic.Pointer so there is no data race on the
// function value.
func (v *PoWVerifier) SetClock(now func() time.Time) {
	if now == nil {
		defaultNow := time.Now
		v.nowFn.Store(&defaultNow)
		return
	}
	v.nowFn.Store(&now)
}

// Provider returns the canonical provider name ("pow").
func (v *PoWVerifier) Provider() string { return "pow" }

// Challenge is the on-wire shape of an issued challenge envelope.
// Marshalled into a base64-encoded blob for transport. Clients
// pass it back verbatim alongside their answer.
type Challenge struct {
	IssuedAt   int64
	Expiry     int64
	Difficulty uint8
	Nonce      [16]byte
	HMAC       [32]byte
}

// IssueChallenge returns a base64-encoded Challenge ready to send
// to the client. The HMAC is computed over the fixed-size header
// so clients can't trim or extend the envelope without invalidating
// the signature.
func (v *PoWVerifier) IssueChallenge() (string, error) {
	if len(v.hmacKey) == 0 {
		return "", errors.New("captcha: pow verifier has no hmac key")
	}
	now := v.now()
	c := Challenge{
		IssuedAt:   now.Unix(),
		Expiry:     now.Add(v.expiry).Unix(),
		Difficulty: v.difficulty,
	}
	if _, err := rand.Read(c.Nonce[:]); err != nil {
		return "", fmt.Errorf("captcha: pow nonce: %w", err)
	}
	header := encodeChallengeHeader(c)
	mac := hmac.New(sha256.New, v.hmacKey)
	mac.Write(header)
	copy(c.HMAC[:], mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString(append(header, c.HMAC[:]...)), nil
}

// Verify implements the Verifier interface. The token is the
// concatenation of the base64-encoded challenge and the answer
// string, separated by a "." (mimicking the JWT shape so frontend
// devs find the format familiar). Empty token denies; malformed
// token denies; expired challenge denies; replayed challenge
// denies; insufficient hash zeros deny.
func (v *PoWVerifier) Verify(_ context.Context, token, _ string) (Outcome, error) {
	if token == "" {
		return Outcome{Success: false, ErrorCodes: []string{"missing-input-response"}}, nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return Outcome{Success: false, ErrorCodes: []string{"malformed-token"}}, nil
	}
	challengeBlob, answer := parts[0], parts[1]
	raw, err := base64.RawURLEncoding.DecodeString(challengeBlob)
	if err != nil || len(raw) != challengeHeaderLen+32 {
		// Malformed base64 / wrong-length envelope is a client
		// bug or active tampering, not a verifier outage; the
		// captcha protocol soft-denies via Outcome.Success=false
		// so the HTTP layer surfaces 403 (not 5xx).  nilerr is a
		// false positive here: the decode err is observed via
		// Outcome.ErrorCodes, not lost.
		_ = err
		return Outcome{Success: false, ErrorCodes: []string{"malformed-challenge"}}, nil //nolint:nilerr // see comment above
	}
	header := raw[:challengeHeaderLen]
	gotHMAC := raw[challengeHeaderLen:]
	mac := hmac.New(sha256.New, v.hmacKey)
	mac.Write(header)
	wantHMAC := mac.Sum(nil)
	if !hmac.Equal(gotHMAC, wantHMAC) {
		// HMAC mismatch means the client tampered with the
		// envelope (most likely lowering the difficulty). Treat
		// as a hard fail with a distinct error code so the
		// audit log can flag attempted tampering.
		return Outcome{Success: false, ErrorCodes: []string{"bad-hmac"}}, nil
	}
	ch := decodeChallengeHeader(header)
	now := v.now().Unix()
	if now > ch.Expiry {
		return Outcome{Success: false, ErrorCodes: []string{"timeout-or-duplicate"}}, ErrTokenStale
	}
	// Replay guard: a successfully-solved challenge is recorded
	// by nonce. Clients can't re-use the same envelope twice.
	nonceKey := base64.RawURLEncoding.EncodeToString(ch.Nonce[:])
	if _, hit := v.replayCache.Get(nonceKey); hit {
		return Outcome{Success: false, ErrorCodes: []string{"timeout-or-duplicate"}}, ErrTokenReplay
	}
	// Verify the proof: SHA-256 of (challenge || answer) must
	// have at least Difficulty leading zero bits.
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte(answer))
	digest := h.Sum(nil)
	if !hasLeadingZeroBits(digest, ch.Difficulty) {
		return Outcome{Success: false, ErrorCodes: []string{"score-below-threshold"}}, nil
	}
	// All checks passed — mark the nonce consumed and return.
	v.replayCache.Set(nonceKey, v.now())
	return Outcome{
		Success: true,
		// Synthetic score: 1.0 means "passed at exactly the
		// configured difficulty"; clients that solve harder than
		// required will have proportionally higher hash budget
		// but we don't expose finer-grained scoring (the
		// captcha is binary at the protocol level).
		Score: 1.0,
		// ChallengeTS reflects when the envelope was issued,
		// matching the field semantics for upstream providers.
		ChallengeTS: time.Unix(ch.IssuedAt, 0),
	}, nil
}

// challengeHeaderLen is the byte length of an encoded challenge
// header (issued_at:8 + expiry:8 + difficulty:1 + nonce:16 = 33).
const challengeHeaderLen = 8 + 8 + 1 + 16

func encodeChallengeHeader(c Challenge) []byte {
	out := make([]byte, challengeHeaderLen)
	// G115 false positive: IssuedAt and Expiry are Unix
	// timestamps (always positive in our use); the BigEndian
	// codec round-trips the bit pattern verbatim so the
	// int64↔uint64 reinterpretation here and in
	// decodeChallengeHeader is byte-identical.
	binary.BigEndian.PutUint64(out[0:8], uint64(c.IssuedAt)) //nolint:gosec // G115 round-trip
	binary.BigEndian.PutUint64(out[8:16], uint64(c.Expiry)) //nolint:gosec // G115 round-trip
	out[16] = c.Difficulty
	copy(out[17:33], c.Nonce[:])
	return out
}

func decodeChallengeHeader(b []byte) Challenge {
	var c Challenge
	c.IssuedAt = int64(binary.BigEndian.Uint64(b[0:8])) //nolint:gosec // G115 round-trip
	c.Expiry = int64(binary.BigEndian.Uint64(b[8:16]))  //nolint:gosec // G115 round-trip
	c.Difficulty = b[16]
	copy(c.Nonce[:], b[17:33])
	return c
}

// hasLeadingZeroBits reports whether the first `bits` bits of `b`
// are all zero. We do the comparison byte-by-byte rather than
// converting to a big.Int because the typical case (most bytes are
// zero) short-circuits cheaply.
func hasLeadingZeroBits(b []byte, bits uint8) bool {
	full := bits / 8
	partial := bits % 8
	if int(full) > len(b) {
		return false
	}
	for i := uint8(0); i < full; i++ {
		if b[i] != 0 {
			return false
		}
	}
	if partial == 0 {
		return true
	}
	if int(full) >= len(b) {
		return false
	}
	mask := byte(0xFF) << (8 - partial)
	return b[full]&mask == 0
}
