package sign_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/marketplace/sign"
)

func TestVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerate(t)
	bundle := []byte("a real-ish .tar.gz body, opaque to the verifier")

	sigB64, err := sign.Sign(bundle, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(bundle, sigB64, sign.EncodePublicKey(pub)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerify_TamperedBundle(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerate(t)
	bundle := []byte("the original bytes")
	sigB64, err := sign.Sign(bundle, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Flip a single byte in the bundle; signature MUST fail.
	tampered := append([]byte(nil), bundle...)
	tampered[0] ^= 0x01
	err = sign.Verify(tampered, sigB64, sign.EncodePublicKey(pub))
	if !errors.Is(err, sign.ErrSignatureMismatch) {
		t.Fatalf("expected ErrSignatureMismatch on tampered bundle, got %v", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	t.Parallel()
	_, priv := mustGenerate(t)
	otherPub, _ := mustGenerate(t)
	bundle := []byte("signed by one publisher, verified under another's key")
	sigB64, err := sign.Sign(bundle, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	err = sign.Verify(bundle, sigB64, sign.EncodePublicKey(otherPub))
	if !errors.Is(err, sign.ErrSignatureMismatch) {
		t.Fatalf("expected ErrSignatureMismatch with wrong key, got %v", err)
	}
}

func TestVerify_InvalidPublicKeyEncoding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"not base64", "!!!not-base64!!!"},
		{"too short", base64.StdEncoding.EncodeToString([]byte("short"))},
		{"too long", base64.StdEncoding.EncodeToString(make([]byte, 64))},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := sign.Verify([]byte("anything"), strings.Repeat("A", 88), tc.key)
			if !errors.Is(err, sign.ErrInvalidPublicKey) {
				t.Fatalf("expected ErrInvalidPublicKey for %q, got %v", tc.name, err)
			}
		})
	}
}

func TestVerify_InvalidSignatureEncoding(t *testing.T) {
	t.Parallel()
	pub, _ := mustGenerate(t)
	pubB64 := sign.EncodePublicKey(pub)
	cases := []struct {
		name string
		sig  string
	}{
		{"empty", ""},
		{"not base64", "!!!"},
		{"too short", base64.StdEncoding.EncodeToString([]byte("short"))},
		{"too long", base64.StdEncoding.EncodeToString(make([]byte, 128))},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := sign.Verify([]byte("anything"), tc.sig, pubB64)
			if !errors.Is(err, sign.ErrInvalidSignatureEncoding) {
				t.Fatalf("expected ErrInvalidSignatureEncoding for %q, got %v", tc.name, err)
			}
		})
	}
}

func TestVerify_EmptyBundle(t *testing.T) {
	t.Parallel()
	// Verifying an empty bundle is structurally legal — ed25519 signs
	// any byte sequence. We exercise this case so a future caller
	// hitting an empty buffer (e.g. a zero-byte tar.gz read) gets a
	// signature-mismatch error rather than a special-cased empty
	// rejection. The point is to lock in the contract.
	pub, priv := mustGenerate(t)
	sigB64, err := sign.Sign(nil, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(nil, sigB64, sign.EncodePublicKey(pub)); err != nil {
		t.Fatalf("verify empty: %v", err)
	}
	if err := sign.Verify([]byte{0}, sigB64, sign.EncodePublicKey(pub)); !errors.Is(err, sign.ErrSignatureMismatch) {
		t.Fatalf("expected ErrSignatureMismatch when bytes differ, got %v", err)
	}
}

func TestSign_RejectsBadPrivateKey(t *testing.T) {
	t.Parallel()
	if _, err := sign.Sign([]byte("x"), ed25519.PrivateKey{}); err == nil {
		t.Fatal("expected error from sign.Sign with empty private key")
	}
	if _, err := sign.Sign([]byte("x"), make(ed25519.PrivateKey, 16)); err == nil {
		t.Fatal("expected error from sign.Sign with short private key")
	}
}

func mustGenerate(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}
