package marketplace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestHashBundleBytesRoundTrip(t *testing.T) {
	payload := []byte("kapp-extension bundle payload v1")
	want := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(want[:])

	got, size, err := HashBundleBytes(payload)
	if err != nil {
		t.Fatalf("HashBundleBytes returned error: %v", err)
	}
	if got != wantHex {
		t.Errorf("hash mismatch: want %s, got %s", wantHex, got)
	}
	if size != int64(len(payload)) {
		t.Errorf("size mismatch: want %d, got %d", len(payload), size)
	}
}

func TestHashBundleStreamingEqualsBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("kapp-ext-"), 8192)
	wantHex, wantSize, err := HashBundleBytes(payload)
	if err != nil {
		t.Fatalf("HashBundleBytes: %v", err)
	}
	gotHex, gotSize, err := HashBundle(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("HashBundle: %v", err)
	}
	if gotHex != wantHex {
		t.Errorf("streaming hash mismatch: want %s, got %s", wantHex, gotHex)
	}
	if gotSize != wantSize {
		t.Errorf("streaming size mismatch: want %d, got %d", wantSize, gotSize)
	}
}

func TestHashBundleRejectsEmpty(t *testing.T) {
	_, _, err := HashBundleBytes(nil)
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("nil payload: want ErrInvalidManifest, got %v", err)
	}
	_, _, err = HashBundle(bytes.NewReader(nil))
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("empty reader: want ErrInvalidManifest, got %v", err)
	}
}

func TestHashBundleRejectsOversize(t *testing.T) {
	// Build a payload one byte over the 10 MiB cap.
	payload := bytes.Repeat([]byte{0x42}, int(MaxBundleSizeBytes)+1)
	_, _, err := HashBundleBytes(payload)
	if !errors.Is(err, ErrBundleTooLarge) {
		t.Fatalf("HashBundleBytes oversized: want ErrBundleTooLarge, got %v", err)
	}
	_, _, err = HashBundle(bytes.NewReader(payload))
	if !errors.Is(err, ErrBundleTooLarge) {
		t.Fatalf("HashBundle oversized: want ErrBundleTooLarge, got %v", err)
	}
}

func TestHashBundleAcceptsExactlyMaxSize(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAA}, int(MaxBundleSizeBytes))
	hex1, size1, err := HashBundleBytes(payload)
	if err != nil {
		t.Fatalf("HashBundleBytes at-cap: %v", err)
	}
	hex2, size2, err := HashBundle(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("HashBundle at-cap: %v", err)
	}
	if hex1 != hex2 || size1 != size2 || size1 != MaxBundleSizeBytes {
		t.Fatalf("at-cap mismatch: bytes=(%s,%d) stream=(%s,%d) want size=%d",
			hex1, size1, hex2, size2, MaxBundleSizeBytes)
	}
}

func TestVerifyBundleHashSuccessAndMismatch(t *testing.T) {
	payload := []byte("verify-me")
	want, _, err := HashBundleBytes(payload)
	if err != nil {
		t.Fatalf("seed hash: %v", err)
	}

	// Success path (lower- and upper-case both accepted).
	for _, expected := range []string{want, strings.ToUpper(want)} {
		if err := VerifyBundleHash(bytes.NewReader(payload), expected); err != nil {
			t.Errorf("VerifyBundleHash(%s): %v", expected, err)
		}
	}

	// Tamper: flip one byte and expect a mismatch.
	tampered := append([]byte(nil), payload...)
	tampered[0] ^= 0xff
	err = VerifyBundleHash(bytes.NewReader(tampered), want)
	if !errors.Is(err, ErrBundleHashMismatch) {
		t.Fatalf("tampered verify: want ErrBundleHashMismatch, got %v", err)
	}
}

func TestVerifyBundleHashRejectsEmptyExpected(t *testing.T) {
	if err := VerifyBundleHash(bytes.NewReader([]byte("x")), ""); err == nil {
		t.Fatal("expected error when expected hash is empty, got nil")
	}
}

func TestHashBundleNilReader(t *testing.T) {
	_, _, err := HashBundle(nil)
	if err == nil {
		t.Fatal("expected error on nil reader")
	}
}

func TestIsValidBundleHash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"too_short", "abcdef", false},
		{"too_long", "a" + string(make([]byte, 64)), false},
		{"non_hex", "g" + string(make([]byte, 63)), false},
		{"uppercase_hex", "ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890", false},
		{"sha256_zero_input", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"sha256_with_spaces", "  e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsValidBundleHash(tc.in); got != tc.want {
				t.Errorf("IsValidBundleHash(%q): want %v, got %v", tc.in, tc.want, got)
			}
		})
	}
}
