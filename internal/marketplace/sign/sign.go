// Package sign verifies and (in tests) produces ed25519 signatures
// over extension bundle bytes. The publisher signs the raw tar.gz
// bytes — not a hash — to avoid length-extension attack surface and
// to match ed25519's "sign what you authenticate" best practice.
//
// The public key is a 32-byte ed25519 public key, base64-standard
// encoded (44 chars). The signature is the 64-byte ed25519 signature,
// base64-standard encoded (88 chars). Both encodings include padding.
//
// The package is intentionally minimal: Verify is the only function
// callers in production need. Sign exists for tests and for the
// publisher CLI (which is out of scope for v1 — publishers use any
// ed25519 tool; we just specify the wire format).
package sign

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// PublicKeySize is the byte length of an ed25519 public key. The DB
// CHECK on marketplace_publisher_keys.public_key_b64 enforces that
// base64-decoding the column produces exactly this many bytes (via a
// pg-side LENGTH check).
const PublicKeySize = ed25519.PublicKeySize

// SignatureSize is the byte length of an ed25519 signature.
const SignatureSize = ed25519.SignatureSize

// ErrInvalidPublicKey signals that pubKeyB64 could not be decoded or
// has wrong length. The pipeline returns this as an `error` severity
// finding with code=signature.invalid_key so the publisher can see
// which of their registered keys is malformed.
var ErrInvalidPublicKey = errors.New("sign: invalid ed25519 public key encoding")

// ErrInvalidSignatureEncoding signals that sigB64 could not be decoded
// or has wrong length. Distinct from a signature that decodes cleanly
// but fails verification (see ErrSignatureMismatch) because the
// failure modes have different operator remediations.
var ErrInvalidSignatureEncoding = errors.New("sign: invalid ed25519 signature encoding")

// ErrSignatureMismatch is the canonical "bundle bytes do not match
// signature" error. Returned both when the bytes were tampered with
// post-sign and when the publisher signed under a different key than
// the one they registered.
var ErrSignatureMismatch = errors.New("sign: signature does not verify against bundle")

// Verify checks that sigB64 is a valid ed25519 signature over
// bundleBytes produced by the private key corresponding to pubKeyB64.
// Returns nil on success, or one of ErrInvalidPublicKey /
// ErrInvalidSignatureEncoding / ErrSignatureMismatch on failure.
//
// bundleBytes is the raw tar.gz body — the same byte stream a SHA-256
// over which produces marketplace_extension_versions.bundle_hash.
// Signing the bundle bytes (not the hash) follows ed25519's design:
// the signature commits to the actual message, so a future hash
// migration (e.g. spec V2 changes to SHA3-256) doesn't invalidate
// historical signatures.
func Verify(bundleBytes []byte, sigB64, pubKeyB64 string) error {
	pubKey, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		return fmt.Errorf("%w: base64 decode: %w", ErrInvalidPublicKey, err)
	}
	if len(pubKey) != PublicKeySize {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidPublicKey, len(pubKey), PublicKeySize)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("%w: base64 decode: %w", ErrInvalidSignatureEncoding, err)
	}
	if len(sig) != SignatureSize {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidSignatureEncoding, len(sig), SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubKey), bundleBytes, sig) {
		return ErrSignatureMismatch
	}
	return nil
}

// Sign produces an ed25519 signature over bundleBytes using privKey
// and returns the base64-encoded signature. Used by tests and by any
// future publisher CLI we ship; production callers verify only.
func Sign(bundleBytes []byte, privKey ed25519.PrivateKey) (string, error) {
	if len(privKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("sign: private key has wrong length: got %d, want %d", len(privKey), ed25519.PrivateKeySize)
	}
	sig := ed25519.Sign(privKey, bundleBytes)
	return base64.StdEncoding.EncodeToString(sig), nil
}

// EncodePublicKey returns the canonical base64 form of pubKey, the
// same encoding stored in marketplace_publisher_keys.public_key_b64.
// Convenience for tests and for the CLI; production callers don't
// re-encode keys (they're stored in canonical form at insert time).
func EncodePublicKey(pubKey ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pubKey)
}
