package tenant

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

var (
	testTenantID  = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	otherTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
)

func TestBlindIndex_DeterministicAndTruncated(t *testing.T) {
	km, err := NewKeyManagerWithPrev([]byte("0123456789ABCDEF0123456789ABCDEF"), nil, 0)
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	d1, err := km.BlindIndex(testTenantID, "alice@example.com")
	if err != nil {
		t.Fatalf("BlindIndex: %v", err)
	}
	d2, err := km.BlindIndex(testTenantID, "alice@example.com")
	if err != nil {
		t.Fatalf("BlindIndex 2: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("blind index not deterministic: %s != %s", d1, d2)
	}
	// Different values → different digests
	d3, _ := km.BlindIndex(testTenantID, "bob@example.com")
	if d1 == d3 {
		t.Fatal("different values produced same digest")
	}
	// Different tenants → different digests for same value
	d4, _ := km.BlindIndex(otherTenantID, "alice@example.com")
	if d1 == d4 {
		t.Fatal("different tenants produced same digest")
	}
}

func TestBlindIndex_NilKeyManager(t *testing.T) {
	var km *KeyManager
	d, err := km.BlindIndex(testTenantID, "test")
	if err != nil {
		t.Fatalf("nil km should not error: %v", err)
	}
	if d != "" {
		t.Fatalf("nil km should return empty digest, got %s", d)
	}
}

func TestBlindIndex_IndependentOfAuditHMAC(t *testing.T) {
	km, _ := NewKeyManagerWithPrev([]byte("0123456789ABCDEF0123456789ABCDEF"), nil, 0)
	bi, _ := km.BlindIndex(testTenantID, "test-value")
	au, _ := km.HMACString(testTenantID, "test-value")
	if bi == au {
		t.Fatal("blind index and audit HMAC produced same digest — keys not domain-separated")
	}
	// Blind index should be shorter (16 bytes / 24 base64 chars) than
	// audit HMAC (32 bytes / 44 base64 chars)
	if len(bi) >= len(au) {
		t.Fatalf("blind index (%d chars) should be shorter than audit HMAC (%d chars)", len(bi), len(au))
	}
}

func TestBlindIndex_Base64Encoded(t *testing.T) {
	km, _ := NewKeyManagerWithPrev([]byte("0123456789ABCDEF0123456789ABCDEF"), nil, 0)
	d, _ := km.BlindIndex(testTenantID, "test")
	// Base64 standard encoding uses only [A-Za-z0-9+/=]
	for _, c := range d {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=", c) {
			t.Fatalf("blind index contains non-base64 char %q in %q", c, d)
		}
	}
}
