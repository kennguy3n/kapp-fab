package auth

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestOIDCStateCodec_SealOpenRoundTrip(t *testing.T) {
	key, err := GenerateOIDCStateKey()
	if err != nil {
		t.Fatalf("GenerateOIDCStateKey: %v", err)
	}
	codec, err := NewOIDCStateCodec(key, 10*time.Minute)
	if err != nil {
		t.Fatalf("NewOIDCStateCodec: %v", err)
	}
	in := OIDCLoginState{
		State:    "state-abc",
		Nonce:    "nonce-xyz",
		Verifier: "pkce-verifier-secret",
		ReturnTo: "/dashboard",
	}
	sealed, err := codec.Seal(in)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The PKCE verifier must not appear in the sealed value.
	if strings.Contains(sealed, in.Verifier) || strings.Contains(sealed, in.State) {
		t.Fatal("sealed state leaks plaintext secrets")
	}
	out, err := codec.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if out.State != in.State || out.Nonce != in.Nonce ||
		out.Verifier != in.Verifier || out.ReturnTo != in.ReturnTo {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
	if out.IssuedAt == 0 {
		t.Error("Seal should stamp IssuedAt")
	}
}

func TestOIDCStateCodec_RejectsTamper(t *testing.T) {
	key, _ := GenerateOIDCStateKey()
	codec, _ := NewOIDCStateCodec(key, 10*time.Minute)
	sealed, err := codec.Seal(OIDCLoginState{State: "s", Nonce: "n", Verifier: "v"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip a byte near the end (within the ciphertext/tag).
	b := []byte(sealed)
	b[len(b)-1] ^= 0x01
	if _, err := codec.Open(string(b)); err == nil {
		t.Fatal("Open accepted tampered ciphertext; want auth failure")
	}
}

func TestOIDCStateCodec_RejectsWrongKey(t *testing.T) {
	k1, _ := GenerateOIDCStateKey()
	k2, _ := GenerateOIDCStateKey()
	c1, _ := NewOIDCStateCodec(k1, 10*time.Minute)
	c2, _ := NewOIDCStateCodec(k2, 10*time.Minute)

	sealed, err := c1.Seal(OIDCLoginState{State: "s", Verifier: "v"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := c2.Open(sealed); err == nil {
		t.Fatal("Open with a different key succeeded; want auth failure")
	}
}

func TestOIDCStateCodec_RejectsExpired(t *testing.T) {
	key, _ := GenerateOIDCStateKey()
	codec, _ := NewOIDCStateCodec(key, time.Second)
	// Stamp an IssuedAt well in the past so it exceeds maxAge.
	sealed, err := codec.Seal(OIDCLoginState{
		State:    "s",
		Verifier: "v",
		IssuedAt: time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := codec.Open(sealed); err == nil {
		t.Fatal("Open accepted expired state; want expiry error")
	}
}

func TestNewOIDCStateCodec_RejectsBadKeyLength(t *testing.T) {
	if _, err := NewOIDCStateCodec([]byte("short"), time.Minute); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}

func TestDeriveOIDCStateKey(t *testing.T) {
	k1, err := DeriveOIDCStateKey("the-jwt-secret")
	if err != nil {
		t.Fatalf("DeriveOIDCStateKey: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("derived key length = %d, want 32", len(k1))
	}
	// Deterministic: same secret -> same key (so replicas agree).
	k2, _ := DeriveOIDCStateKey("the-jwt-secret")
	if !bytes.Equal(k1, k2) {
		t.Fatal("DeriveOIDCStateKey is not deterministic")
	}
	if _, err := DeriveOIDCStateKey(""); err == nil {
		t.Fatal("expected error for empty secret")
	}
}
