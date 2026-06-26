package tenant

import (
	"bytes"
	"testing"
)

func TestWrapUnwrapDEK_RoundTrip(t *testing.T) {
	kek := []byte("0123456789ABCDEF0123456789ABCDEF")
	dek := []byte("FEDCBA9876543210FEDCBA9876543210")

	wrapped, err := wrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("wrapDEK: %v", err)
	}
	if !startsWith(wrapped, wrappedDEKPrefix) {
		t.Fatalf("wrapped DEK missing prefix: %s", wrapped)
	}

	unwrapped, err := unwrapDEK(kek, wrapped)
	if err != nil {
		t.Fatalf("unwrapDEK: %v", err)
	}
	if !bytes.Equal(dek, unwrapped) {
		t.Fatalf("round-trip mismatch: %x != %x", dek, unwrapped)
	}
}

func TestUnwrapDEK_WrongKEK(t *testing.T) {
	kek := []byte("0123456789ABCDEF0123456789ABCDEF")
	wrongKEK := []byte("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")
	dek := []byte("FEDCBA9876543210FEDCBA9876543210")

	wrapped, _ := wrapDEK(kek, dek)
	_, err := unwrapDEK(wrongKEK, wrapped)
	if err == nil {
		t.Fatal("unwrap with wrong KEK should fail")
	}
}

func TestUnwrapDEK_InvalidPrefix(t *testing.T) {
	kek := []byte("0123456789ABCDEF0123456789ABCDEF")
	_, err := unwrapDEK(kek, "not-a-wrapped-key")
	if err == nil {
		t.Fatal("unwrap with invalid prefix should fail")
	}
}

func TestGenerateDEK_Randomness(t *testing.T) {
	dek1, err := generateDEK()
	if err != nil {
		t.Fatalf("generateDEK: %v", err)
	}
	dek2, err := generateDEK()
	if err != nil {
		t.Fatalf("generateDEK 2: %v", err)
	}
	if len(dek1) != keySize {
		t.Fatalf("DEK wrong size: %d", len(dek1))
	}
	if bytes.Equal(dek1, dek2) {
		t.Fatal("two generated DEKs are identical — RNG not working")
	}
}

func TestEnvelopeKeyManager_DeriveKEK(t *testing.T) {
	km, err := NewEnvelopeKeyManager([]byte("0123456789ABCDEF0123456789ABCDEF"), nil, 0)
	if err != nil {
		t.Fatalf("NewEnvelopeKeyManager: %v", err)
	}
	kek1, err := km.deriveKEK(testTenantID)
	if err != nil {
		t.Fatalf("deriveKEK: %v", err)
	}
	kek2, err := km.deriveKEK(otherTenantID)
	if err != nil {
		t.Fatalf("deriveKEK 2: %v", err)
	}
	if bytes.Equal(kek1, kek2) {
		t.Fatal("KEKs for different tenants are identical")
	}
	if len(kek1) != keySize {
		t.Fatalf("KEK wrong size: %d", len(kek1))
	}
}

func TestEnvelopeKeyManager_KEKIndependentOfFieldKey(t *testing.T) {
	km, _ := NewEnvelopeKeyManager([]byte("0123456789ABCDEF0123456789ABCDEF"), nil, 0)
	kek, _ := km.deriveKEK(testTenantID)
	fieldKey, _ := DeriveKey([]byte("0123456789ABCDEF0123456789ABCDEF"), testTenantID)
	if bytes.Equal(kek, fieldKey) {
		t.Fatal("KEK and field-encryption key are identical — labels not domain-separated")
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
