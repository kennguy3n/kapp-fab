package tenant

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestFailClosedOnMissingMasterKey verifies the production posture for
// the per-tenant field-encryption master key (P0-4 in
// docs/SECURITY_HARDENING_PLAN.md): a missing key is fatal in
// production and tolerated in dev/staging.
func TestFailClosedOnMissingMasterKey(t *testing.T) {
	missingErr := ErrMasterKeyMissing

	t.Run("production fails closed", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "production")
		err := FailClosedOnMissingMasterKey(missingErr)
		if err == nil {
			t.Fatalf("expected fail-closed error in production, got nil")
		}
		if !errors.Is(err, ErrMasterKeyMissing) {
			t.Fatalf("expected error to wrap ErrMasterKeyMissing, got %v", err)
		}
	})

	t.Run("prod alias fails closed", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "prod")
		if err := FailClosedOnMissingMasterKey(missingErr); err == nil {
			t.Fatalf("expected fail-closed error for KAPP_ENV=prod, got nil")
		}
	})

	t.Run("dev tolerates missing key", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "dev")
		if err := FailClosedOnMissingMasterKey(missingErr); err != nil {
			t.Fatalf("expected nil in dev, got %v", err)
		}
	})

	t.Run("staging tolerates missing key", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "staging")
		if err := FailClosedOnMissingMasterKey(missingErr); err != nil {
			t.Fatalf("expected nil in staging, got %v", err)
		}
	})

	t.Run("unset env tolerates missing key", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "")
		if err := FailClosedOnMissingMasterKey(missingErr); err != nil {
			t.Fatalf("expected nil when KAPP_ENV unset, got %v", err)
		}
	})

	t.Run("non-sentinel error is always fatal", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "dev")
		other := errors.New("tenant: malformed base64")
		if err := FailClosedOnMissingMasterKey(other); err == nil {
			t.Fatalf("expected non-sentinel error to be fatal even in dev, got nil")
		}
	})

	t.Run("nil error is nil", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "production")
		if err := FailClosedOnMissingMasterKey(nil); err != nil {
			t.Fatalf("expected nil for nil error, got %v", err)
		}
	})
}

// TestHMACStringDeterministic confirms the audit HMAC digest is stable
// for a given (master key, tenant, value) triple, which is the invariant
// the audit redaction path relies on to detect "no change" without
// seeing the value.
func TestHMACStringDeterministic(t *testing.T) {
	km, err := NewKeyManager(make([]byte, 32), 0)
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	tenantID := uuid.New()
	a, err := km.HMACString(tenantID, "secret-value")
	if err != nil {
		t.Fatalf("HMACString: %v", err)
	}
	b, err := km.HMACString(tenantID, "secret-value")
	if err != nil {
		t.Fatalf("HMACString second call: %v", err)
	}
	if a == "" {
		t.Fatalf("expected non-empty digest")
	}
	if a != b {
		t.Fatalf("expected deterministic digest: %q != %q", a, b)
	}
	// A different value produces a different digest.
	c, _ := km.HMACString(tenantID, "other-value")
	if a == c {
		t.Fatalf("expected distinct digest for distinct value")
	}
}

// TestHMACStringNilKeyManager returns empty digest, never an error, so
// the redaction path degrades gracefully when no master key is wired.
func TestHMACStringNilKeyManager(t *testing.T) {
	var km *KeyManager
	got, err := km.HMACString(uuid.New(), "x")
	if err != nil {
		t.Fatalf("nil key manager should not error, got %v", err)
	}
	if got != "" {
		t.Fatalf("nil key manager should return empty digest, got %q", got)
	}
}

// TestSanitizeForLog verifies the P1-7 log-redaction chokepoint: ciphertext
// envelopes and registered sentinels never reach a log/error surface.
func TestSanitizeForLog(t *testing.T) {
	t.Run("ciphertext envelope stripped", func(t *testing.T) {
		ct := EncryptStringForTest(t, "secret-value")
		in := "record.data=" + ct + " status=active"
		out := SanitizeForLog(in)
		if strings.Contains(out, ct) {
			t.Fatalf("ciphertext leaked into sanitized output: %s", out)
		}
		if !strings.Contains(out, "<ciphertext>") {
			t.Fatalf("expected <ciphertext> placeholder, got %s", out)
		}
		if !strings.Contains(out, "status=active") {
			t.Fatalf("non-sensitive text should pass through, got %s", out)
		}
	})

	t.Run("registered sentinel stripped", func(t *testing.T) {
		sentinel := "LIVE_TOKEN_abc123"
		RegisterLogSentinel(sentinel)
		in := "auth failed for token=" + sentinel + " retry=3"
		out := SanitizeForLog(in)
		if strings.Contains(out, sentinel) {
			t.Fatalf("sentinel leaked into sanitized output: %s", out)
		}
		if !strings.Contains(out, "<redacted>") {
			t.Fatalf("expected <redacted> placeholder, got %s", out)
		}
	})

	t.Run("plain text unchanged", func(t *testing.T) {
		in := "tenant=abc status=active count=3"
		if got := SanitizeForLog(in); got != in {
			t.Fatalf("plain text should be unchanged, got %s", got)
		}
	})

	t.Run("empty unchanged", func(t *testing.T) {
		if got := SanitizeForLog(""); got != "" {
			t.Fatalf("empty should stay empty, got %q", got)
		}
	})
}

// EncryptStringForTest encrypts a value under a throwaway key so the
// sanitizer test can use a real ciphertext envelope without depending on
// the environment.
func EncryptStringForTest(t *testing.T, plaintext string) string {
	t.Helper()
	km, err := NewKeyManager(make([]byte, 32), 0)
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	ct, err := km.EncryptString(uuid.New(), plaintext)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	return ct
}
