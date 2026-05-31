package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SignatureHeaderName is the header the dispatcher sets on every
// outbound request and the receiver MUST verify. The "sha256="
// prefix on the value (added by FormatSignatureHeader) lets the
// receiver distinguish future algorithms — when we eventually add
// HMAC-SHA512 the prefix becomes "sha512=" and old receivers can
// reject what they don't recognise instead of silently mis-
// verifying.
const SignatureHeaderName = "X-Kapp-Signature"

// TimestampHeaderName carries the dispatcher's wall-clock timestamp
// in RFC 3339 nano format. The receiver re-runs the HMAC using the
// supplied timestamp; if the supplied timestamp drifts more than
// MaxClockSkew from the receiver's wall clock, the receiver MUST
// reject the request (replay protection).
const TimestampHeaderName = "X-Kapp-Timestamp"

// RequestIDHeaderName carries the dispatcher's per-attempt request
// ID. The receiver should treat this as the de-duplication key for
// at-least-once delivery semantics — if the receiver has already
// processed a request with this ID, it should return the prior
// response without re-executing side effects.
const RequestIDHeaderName = "X-Kapp-Request-Id"

// MaxClockSkew is the receiver's allowable timestamp drift. A 5
// minute window covers cross-region NTP skew + reasonable client
// clock drift without giving an attacker enough time to capture
// and replay a request before the window closes. Mirrors the
// AWS Signature V4 default for the same reason.
const MaxClockSkew = 5 * time.Minute

// CanonicalRequest serialises a DispatchRequest into the exact
// byte sequence the HMAC is computed over. Every field that
// affects the request's semantics MUST appear here so a man-in-
// the-middle that alters any byte invalidates the signature.
//
// Fields, newline-separated:
//
//	timestamp_rfc3339_nano  ← e.g. "2024-01-15T12:30:00.123456789Z"
//	request_id_uuid         ← canonical hyphenated UUIDv4
//	http_method             ← always "POST" in B3
//	url_path_with_query     ← URL.Path + "?" + URL.RawQuery if any
//	body_sha256_lowerhex    ← lower-hex of SHA-256(body)
//
// The trailing newline after body_sha256 is deliberate: the
// canonical form is line-terminated, NOT line-separated. This
// matches Stripe's webhook signing convention (see Stripe Webhooks
// API docs §"Verifying signatures manually") and makes the
// receiver's parsing more forgiving — a buggy receiver that trims
// trailing whitespace still computes the same HMAC.
func CanonicalRequest(timestamp time.Time, requestID uuid.UUID, method, targetURL string, body []byte) (string, error) {
	if method == "" {
		return "", errors.New("runtime: canonical: empty method")
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return "", fmt.Errorf("runtime: canonical: parse url: %w", err)
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	bodyHash := sha256.Sum256(body)
	var b strings.Builder
	b.WriteString(timestamp.UTC().Format(time.RFC3339Nano))
	b.WriteByte('\n')
	b.WriteString(requestID.String())
	b.WriteByte('\n')
	b.WriteString(strings.ToUpper(method))
	b.WriteByte('\n')
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(bodyHash[:]))
	b.WriteByte('\n')
	return b.String(), nil
}

// BodyHashHex returns the lower-hex SHA-256 of the body. Exported
// for the dispatch log (the column stores the hash, not the body).
// Identical to the value embedded inside CanonicalRequest.
func BodyHashHex(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// SignCanonical computes HMAC-SHA256 of canonical using secret and
// returns the lower-hex digest. This is the "sha256" half of the
// X-Kapp-Signature header value.
func SignCanonical(canonical string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// FormatSignatureHeader joins the algorithm tag and digest into the
// header value the receiver will see.
//
//	X-Kapp-Signature: sha256=<hex>
//
// Receivers MUST split on "=" and reject if the prefix is anything
// other than a recognised algorithm. The split-on-equals shape is
// deliberately the same as Stripe's so existing webhook receivers
// in the ecosystem can reuse their verification code with a
// header-name swap.
func FormatSignatureHeader(digestHex string) string {
	return "sha256=" + digestHex
}

// SignRequest is the dispatcher's primary entry point: given a
// DispatchRequest + signing secret + timestamp, produce the three
// headers the transport sets on every outbound request and the
// canonical-request string the receiver will re-compute.
//
// Returns the canonical string in addition to the headers so the
// caller can persist a SHA-256 of the canonical (which equals the
// digest in this construction; the audit log stores the body hash
// only, but having the canonical available simplifies golden-test
// assertions).
func SignRequest(req *DispatchRequest, ts time.Time) (headers map[string]string, canonical string, err error) {
	if req == nil {
		return nil, "", errors.New("runtime: sign: nil request")
	}
	if req.URL == "" {
		return nil, "", errors.New("runtime: sign: empty URL")
	}
	secret, err := req.SigningSecret.Bytes()
	if err != nil {
		return nil, "", fmt.Errorf("runtime: sign: decode secret: %w", err)
	}
	canonical, err = CanonicalRequest(ts, req.RequestID, "POST", req.URL, req.Body)
	if err != nil {
		return nil, "", err
	}
	digest := SignCanonical(canonical, secret)
	headers = map[string]string{
		TimestampHeaderName: ts.UTC().Format(time.RFC3339Nano),
		RequestIDHeaderName: req.RequestID.String(),
		SignatureHeaderName: FormatSignatureHeader(digest),
		"Content-Type":      "application/json",
	}
	return headers, canonical, nil
}

// VerifySignature re-computes the HMAC and compares it to the
// supplied digest in constant time. Receivers vendor this function
// (or its sibling in the runtime/verify subpackage) to validate
// inbound requests from the dispatcher.
//
// Returns nil iff the signature matches AND the timestamp is
// within MaxClockSkew of now. Returns a descriptive error otherwise.
// The descriptive error is INTERNAL-only — receivers MUST return a
// uniform 401 to the dispatcher to avoid leaking which check failed
// (whether the timestamp was off vs. the digest was wrong) to a
// would-be attacker probing the verification surface.
func VerifySignature(secret []byte, ts time.Time, requestID uuid.UUID, method, targetURL string, body []byte, suppliedSignatureHeader string, now time.Time) error {
	if len(secret) == 0 {
		return errors.New("runtime: verify: empty secret")
	}
	if !strings.HasPrefix(suppliedSignatureHeader, "sha256=") {
		return fmt.Errorf("runtime: verify: unrecognised algorithm prefix in %q", suppliedSignatureHeader)
	}
	suppliedDigest := strings.TrimPrefix(suppliedSignatureHeader, "sha256=")
	suppliedBytes, err := hex.DecodeString(suppliedDigest)
	if err != nil {
		return fmt.Errorf("runtime: verify: digest not hex: %w", err)
	}
	canonical, err := CanonicalRequest(ts, requestID, method, targetURL, body)
	if err != nil {
		return err
	}
	expectedHex := SignCanonical(canonical, secret)
	expectedBytes, err := hex.DecodeString(expectedHex)
	if err != nil {
		// hex.EncodeToString never produces invalid hex.
		return fmt.Errorf("runtime: verify: internal expected-hex decode: %w", err)
	}
	if subtle.ConstantTimeCompare(suppliedBytes, expectedBytes) != 1 {
		return errors.New("runtime: verify: signature mismatch")
	}
	if drift := now.Sub(ts); drift > MaxClockSkew || drift < -MaxClockSkew {
		return fmt.Errorf("runtime: verify: timestamp drift %s exceeds %s", drift, MaxClockSkew)
	}
	return nil
}
