package runtime

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestCanonicalRequest_GoldenShape pins the canonical-request
// string to a known-good shape. If this test fails, the receive-
// side verification in every deployed extension breaks — so the
// shape MUST NOT change without a SignatureHeaderName version bump.
func TestCanonicalRequest_GoldenShape(t *testing.T) {
	ts := time.Date(2024, 1, 15, 12, 30, 0, 123456789, time.UTC)
	rid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	body := []byte(`{"hello":"world"}`)
	got, err := CanonicalRequest(ts, rid, "POST", "https://ext.example.com/tools/print?v=1", body)
	if err != nil {
		t.Fatalf("CanonicalRequest: %v", err)
	}
	want := "2024-01-15T12:30:00.123456789Z\n" +
		"11111111-2222-3333-4444-555555555555\n" +
		"POST\n" +
		"/tools/print?v=1\n" +
		"93a23971a914e5eacbf0a8d25154cda309c3c1c72fbb9914d47c60f3cb681588\n"
	if got != want {
		t.Errorf("canonical mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestCanonicalRequest_EmptyBody pins the hash of zero bytes —
// that's the same SHA-256(empty) constant every receiver should
// recognise.
func TestCanonicalRequest_EmptyBody(t *testing.T) {
	ts := time.Date(2024, 1, 15, 12, 30, 0, 0, time.UTC)
	rid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	got, err := CanonicalRequest(ts, rid, "POST", "https://ext.example.com/lifecycle/pre_install", nil)
	if err != nil {
		t.Fatalf("CanonicalRequest: %v", err)
	}
	// SHA-256("") = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
	if !strings.Contains(got, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
		t.Errorf("canonical does not embed SHA-256(empty): %q", got)
	}
}

// TestCanonicalRequest_QueryStringIncluded asserts that ?v=1 vs.
// ?v=2 produce different canonicals — a receiver implementation
// that strips the query string would mis-verify.
func TestCanonicalRequest_QueryStringIncluded(t *testing.T) {
	ts := time.Date(2024, 1, 15, 12, 30, 0, 0, time.UTC)
	rid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	body := []byte(`{}`)
	v1, _ := CanonicalRequest(ts, rid, "POST", "https://ext.example.com/x?v=1", body)
	v2, _ := CanonicalRequest(ts, rid, "POST", "https://ext.example.com/x?v=2", body)
	if v1 == v2 {
		t.Errorf("query string not included in canonical: v1=v2=%q", v1)
	}
}

// TestSignCanonical_DeterministicHex pins the HMAC output for a
// known canonical+secret. Catches accidental algorithm swaps
// (e.g. SHA-1, raw HMAC without hex encoding).
func TestSignCanonical_DeterministicHex(t *testing.T) {
	canonical := "2024-01-15T12:30:00Z\n" +
		"11111111-2222-3333-4444-555555555555\n" +
		"POST\n" +
		"/x\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n"
	secret := []byte("0123456789abcdef0123456789abcdef")
	got := SignCanonical(canonical, secret)
	if len(got) != 64 {
		t.Errorf("hex digest length %d, want 64", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("digest not lower-hex: %v", err)
	}
	// Pin the exact value. Recomputing this with `openssl
	// dgst -sha256 -hmac '0123456789abcdef0123456789abcdef'`
	// produces this hex.
	want := "82e7979a1a22064e004e140422b2b2c11e5a9f9d43802de3269dbbb70b890a63"
	if got != want {
		// Compute expected for diagnostic message — DO NOT change
		// the constant above without re-running the openssl
		// command on the literal canonical string.
		t.Errorf("hmac digest changed: got %q want %q (regenerate with openssl dgst -sha256 -hmac and update if intentional)", got, want)
	}
}

// TestSignRequest_HeadersComplete asserts that the dispatcher's
// three required headers are all present and have the expected
// shapes.
func TestSignRequest_HeadersComplete(t *testing.T) {
	secret, err := GenerateSigningSecret()
	if err != nil {
		t.Fatalf("GenerateSigningSecret: %v", err)
	}
	req := &DispatchRequest{
		TenantID:           uuid.New(),
		InstallationID:     uuid.New(),
		ExtensionID:        uuid.New(),
		ExtensionVersionID: uuid.New(),
		Kind:               KindToolInvoke,
		URL:                "https://ext.example.com/tools/print",
		Body:               []byte(`{"label":"abc"}`),
		Timeout:            5 * time.Second,
		Retry:              &RetryPolicy{MaxAttempts: 1, Backoff: "exponential"},
		SigningSecret:      secret,
		RequestID:          uuid.New(),
	}
	ts := time.Now().UTC()
	headers, canonical, err := SignRequest(req, ts)
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	if headers[TimestampHeaderName] == "" {
		t.Errorf("missing timestamp header")
	}
	if headers[RequestIDHeaderName] != req.RequestID.String() {
		t.Errorf("request id header mismatch: %q", headers[RequestIDHeaderName])
	}
	sig := headers[SignatureHeaderName]
	if !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("signature missing sha256= prefix: %q", sig)
	}
	if headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type: got %q", headers["Content-Type"])
	}
	// Canonical must contain the body hash.
	if !strings.Contains(canonical, BodyHashHex(req.Body)) {
		t.Errorf("canonical missing body hash")
	}
}

// TestVerifySignature_HappyPath round-trips a sign+verify with the
// same secret + timestamp.
func TestVerifySignature_HappyPath(t *testing.T) {
	secret, _ := GenerateSigningSecret()
	rawSecret, _ := secret.Bytes()
	req := &DispatchRequest{
		URL:           "https://ext.example.com/x",
		Body:          []byte(`{"k":"v"}`),
		SigningSecret: secret,
		RequestID:     uuid.New(),
	}
	ts := time.Now().UTC()
	headers, _, err := SignRequest(req, ts)
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	if err := VerifySignature(rawSecret, ts, req.RequestID, "POST", req.URL, req.Body, headers[SignatureHeaderName], ts); err != nil {
		t.Errorf("VerifySignature: %v", err)
	}
}

// TestVerifySignature_TamperedBody asserts that a single-byte
// change to the body invalidates the signature.
func TestVerifySignature_TamperedBody(t *testing.T) {
	secret, _ := GenerateSigningSecret()
	rawSecret, _ := secret.Bytes()
	req := &DispatchRequest{
		URL:           "https://ext.example.com/x",
		Body:          []byte(`{"k":"v"}`),
		SigningSecret: secret,
		RequestID:     uuid.New(),
	}
	ts := time.Now().UTC()
	headers, _, _ := SignRequest(req, ts)
	tamperedBody := []byte(`{"k":"VV"}`)
	err := VerifySignature(rawSecret, ts, req.RequestID, "POST", req.URL, tamperedBody, headers[SignatureHeaderName], ts)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("tampered body should fail verification: %v", err)
	}
}

// TestVerifySignature_TimestampDrift asserts that a stale timestamp
// (> MaxClockSkew) is rejected even if the signature itself is
// valid. Replay protection.
func TestVerifySignature_TimestampDrift(t *testing.T) {
	secret, _ := GenerateSigningSecret()
	rawSecret, _ := secret.Bytes()
	req := &DispatchRequest{
		URL:           "https://ext.example.com/x",
		Body:          []byte(`{}`),
		SigningSecret: secret,
		RequestID:     uuid.New(),
	}
	signedAt := time.Now().UTC()
	headers, _, _ := SignRequest(req, signedAt)
	now := signedAt.Add(MaxClockSkew + time.Second)
	err := VerifySignature(rawSecret, signedAt, req.RequestID, "POST", req.URL, req.Body, headers[SignatureHeaderName], now)
	if err == nil || !strings.Contains(err.Error(), "drift") {
		t.Errorf("stale timestamp should fail verification: %v", err)
	}
}

// TestVerifySignature_WrongSecret asserts that a different secret
// produces a different digest and verification fails.
func TestVerifySignature_WrongSecret(t *testing.T) {
	secretA, _ := GenerateSigningSecret()
	secretB, _ := GenerateSigningSecret()
	rawB, _ := secretB.Bytes()
	req := &DispatchRequest{
		URL:           "https://ext.example.com/x",
		Body:          []byte(`{}`),
		SigningSecret: secretA,
		RequestID:     uuid.New(),
	}
	ts := time.Now().UTC()
	headers, _, _ := SignRequest(req, ts)
	// Verify with wrong secret.
	err := VerifySignature(rawB, ts, req.RequestID, "POST", req.URL, req.Body, headers[SignatureHeaderName], ts)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("wrong secret should fail verification: %v", err)
	}
}

// TestVerifySignature_UnknownPrefix asserts that a future algorithm
// prefix (e.g. sha512=...) is rejected by current receivers.
func TestVerifySignature_UnknownPrefix(t *testing.T) {
	secret, _ := GenerateSigningSecret()
	rawSecret, _ := secret.Bytes()
	err := VerifySignature(rawSecret, time.Now().UTC(), uuid.New(), "POST", "https://x.example.com/", nil, "sha512=deadbeef", time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "algorithm") {
		t.Errorf("unknown prefix should fail verification: %v", err)
	}
}

// TestGenerateSigningSecret_FormatAndUniqueness covers the
// "43-char base64url" property and "no two consecutive calls
// return the same secret" property.
func TestGenerateSigningSecret_FormatAndUniqueness(t *testing.T) {
	seen := map[SigningSecret]bool{}
	for i := 0; i < 100; i++ {
		s, err := GenerateSigningSecret()
		if err != nil {
			t.Fatalf("GenerateSigningSecret: %v", err)
		}
		if len(s) != 43 {
			t.Errorf("secret length %d, want 43", len(s))
		}
		if seen[s] {
			t.Errorf("duplicate secret on iteration %d: %s", i, s)
		}
		seen[s] = true
		// Format check via Bytes round-trip.
		raw, err := s.Bytes()
		if err != nil {
			t.Errorf("Bytes: %v", err)
		}
		if len(raw) != 32 {
			t.Errorf("decoded secret length %d, want 32", len(raw))
		}
	}
}

// TestSigningSecret_Bytes_BadFormat covers the defensive code path
// for a column value that bypassed GenerateSigningSecret (e.g.
// hand-written via direct SQL).
func TestSigningSecret_Bytes_BadFormat(t *testing.T) {
	tests := []struct {
		name   string
		secret SigningSecret
	}{
		{"empty", ""},
		{"short", "AAAAAA"},
		{"long", SigningSecret(strings.Repeat("A", 100))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.secret.Bytes(); err == nil {
				t.Errorf("expected error for %q", tc.secret)
			}
		})
	}
}

// TestIsValidWebhookBase covers the URL sanity checks the engine
// applies before dispatch.
func TestIsValidWebhookBase(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		valid bool
	}{
		{"https valid", "https://ext.example.com", true},
		{"https with path", "https://ext.example.com/api/v1", true},
		{"http rejected", "http://ext.example.com", false},
		{"empty rejected", "", false},
		{"no scheme rejected", "ext.example.com", false},
		{"file scheme rejected", "file:///etc/passwd", false},
		{"javascript scheme rejected", "javascript:alert(1)", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := IsValidWebhookBase(tc.base)
			if tc.valid && err != nil {
				t.Errorf("valid base rejected: %v", err)
			}
			if !tc.valid && err == nil {
				t.Errorf("invalid base accepted: %s", tc.base)
			}
		})
	}
}

// TestRetryPolicy_BackoffDelay pins the exact delay sequence for
// each backoff strategy.
func TestRetryPolicy_BackoffDelay(t *testing.T) {
	tests := []struct {
		name   string
		policy *RetryPolicy
		want   []time.Duration
	}{
		{
			name:   "linear",
			policy: &RetryPolicy{MaxAttempts: 5, Backoff: "linear"},
			want: []time.Duration{
				0,               // attempt 1
				1 * time.Second, // attempt 2
				2 * time.Second, // attempt 3
				3 * time.Second, // attempt 4
				4 * time.Second, // attempt 5
			},
		},
		{
			name:   "exponential",
			policy: &RetryPolicy{MaxAttempts: 5, Backoff: "exponential"},
			want: []time.Duration{
				0,               // attempt 1
				1 * time.Second, // attempt 2
				2 * time.Second, // attempt 3
				4 * time.Second, // attempt 4
				8 * time.Second, // attempt 5
			},
		},
		{
			name:   "nil policy",
			policy: nil,
			want:   []time.Duration{0, 0, 0},
		},
		{
			name:   "unknown backoff",
			policy: &RetryPolicy{MaxAttempts: 3, Backoff: "fibonacci"},
			want:   []time.Duration{0, 0, 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i, want := range tc.want {
				got := tc.policy.BackoffDelay(i + 1)
				if got != want {
					t.Errorf("attempt %d: got %v want %v", i+1, got, want)
				}
			}
		})
	}
}

// TestLifecyclePhase_Path covers the URL convention.
func TestLifecyclePhase_Path(t *testing.T) {
	tests := []struct {
		phase LifecyclePhase
		want  string
	}{
		{PhasePreInstall, "/lifecycle/pre_install"},
		{PhasePostInstall, "/lifecycle/post_install"},
		{PhasePreUninstall, "/lifecycle/pre_uninstall"},
		{PhasePostUninstall, "/lifecycle/post_uninstall"},
	}
	for _, tc := range tests {
		got := tc.phase.LifecyclePath()
		if got != tc.want {
			t.Errorf("phase %s: got %q want %q", tc.phase, got, tc.want)
		}
	}
}

// TestDispatchKindForPhase covers the phase → audit-log kind map.
func TestDispatchKindForPhase(t *testing.T) {
	tests := map[LifecyclePhase]DispatchKind{
		PhasePreInstall:    KindLifecyclePreInstall,
		PhasePostInstall:   KindLifecyclePostInstall,
		PhasePreUninstall:  KindLifecyclePreUninstall,
		PhasePostUninstall: KindLifecyclePostUninstall,
	}
	for phase, want := range tests {
		got := DispatchKindForPhase(phase)
		if got != want {
			t.Errorf("phase %s: got %q want %q", phase, got, want)
		}
	}
}

// TestInstallRequest_Validate covers the field-level sanity checks.
func TestInstallRequest_Validate(t *testing.T) {
	good := &InstallRequest{
		TenantID:    uuid.New(),
		ExtensionID: uuid.New(),
		VersionID:   uuid.New(),
		WebhookBase: "https://ext.example.com",
	}
	if err := good.Validate(); err != nil {
		t.Errorf("good request rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*InstallRequest)
	}{
		{"nil tenant", func(r *InstallRequest) { r.TenantID = uuid.Nil }},
		{"nil extension", func(r *InstallRequest) { r.ExtensionID = uuid.Nil }},
		{"nil version", func(r *InstallRequest) { r.VersionID = uuid.Nil }},
		{"http base", func(r *InstallRequest) { r.WebhookBase = "http://x" }},
		{"empty base", func(r *InstallRequest) { r.WebhookBase = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := *good
			tc.mutate(&r)
			if err := r.Validate(); err == nil {
				t.Errorf("malformed request accepted")
			}
		})
	}
}

// TestInstallRequest_NormalizedWebhookBase asserts trailing slashes
// are stripped.
func TestInstallRequest_NormalizedWebhookBase(t *testing.T) {
	r := &InstallRequest{WebhookBase: "https://ext.example.com///"}
	if got := r.NormalizedWebhookBase(); got != "https://ext.example.com" {
		t.Errorf("normalized base: %q", got)
	}
}
