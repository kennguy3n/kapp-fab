package marketplace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// bundleHashRegex pins the hex-string shape callers must pass to
// Store.PublishVersion / Store.GetVersion and that VerifyBundleHash
// expects from external input. Matches the DB CHECK constraint on
// marketplace_extension_versions.bundle_hash exactly (lower-case hex,
// 64 chars = 256 bits / 4 bits-per-nibble). HashBundle / HashBundleBytes
// always produce a string in this shape, but an external caller that
// constructs PublishVersionInput manually (e.g. an admin CLI passing
// a header value) could pass an upper-case hash or a non-hex string —
// without this guard the row would fail the DB CHECK and surface as
// an opaque SQLSTATE 23514 instead of the targeted ErrInvalidManifest.
// Validating in Go gives the publisher a clear field-level error and
// avoids a wasted round-trip.
var bundleHashRegex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// IsValidBundleHash reports whether s is in the canonical form the
// marketplace stores in marketplace_extension_versions.bundle_hash —
// lower-case hex, exactly 64 chars. The DB CHECK enforces the same
// shape, so a true response here is also a guarantee the value will
// be accepted by the constraint.
func IsValidBundleHash(s string) bool {
	return bundleHashRegex.MatchString(s)
}

// MaxBundleSizeBytes is the hard cap on a single .tar.gz upload. Spec
// §2 ("Total bundle size (post-extract) 10 MiB"). We reuse the same
// limit for the compressed archive because (a) gzip on JSON / YAML /
// PDF assets gives a modest ratio, and (b) the DB CHECK constraint on
// marketplace_extension_versions.bundle_size_bytes pins the same
// 10 MiB ceiling — keeping the values identical means the validator,
// the API layer, and the DB all reject the same payload.
const MaxBundleSizeBytes int64 = 10 * 1024 * 1024 // 10 MiB

// MaxManifestSizeBytes is the hard cap on kapp-extension.yaml. Spec
// §2 ("Manifest YAML size 64 KiB"). A manifest exceeding this is
// almost certainly a manifest-injection attack or a misencoded file
// (binary-as-yaml) — either way we reject before parsing.
const MaxManifestSizeBytes int64 = 64 * 1024 // 64 KiB

// MaxSingleFileBytes is the hard cap on any individual file inside the
// bundle. Spec §2 ("Any single file inside the bundle 2 MiB"). The
// bundle extractor (B6) enforces this per-entry; this constant lives
// here so the spec hard-limit values are colocated.
const MaxSingleFileBytes int64 = 2 * 1024 * 1024 // 2 MiB

// MaxIconWidth / MaxIconHeight pin the icon dimensions to 256×256 per
// spec §2. The image decoder lives in the upload pipeline (B6 calls
// image.DecodeConfig on assets/icon.png); the constants are exported
// here so the validator code path can reference one source of truth.
const (
	MaxIconWidth  = 256
	MaxIconHeight = 256
)

// HashBundle reads the full bundle from r, returning the lower-case
// hex SHA-256 plus the byte count. It enforces MaxBundleSizeBytes
// streaming — once the bundle exceeds the cap we abort with
// ErrBundleTooLarge instead of buffering the entire payload into
// memory. Returns (hex, size, error); size is the number of bytes
// actually consumed before either EOF or the size cap fired.
//
// The caller is responsible for closing r (HashBundle is read-only and
// does not retain a reference to it).
func HashBundle(r io.Reader) (string, int64, error) {
	if r == nil {
		return "", 0, errors.New("marketplace: nil bundle reader")
	}
	h := sha256.New()
	// LimitReader returns EOF after MaxBundleSizeBytes; we then peek
	// one more byte to distinguish "exactly at cap" (valid) from
	// "exceeds cap" (reject). The +1 lets the peek see the next byte
	// without inflating io.Copy's internal buffer growth.
	limited := io.LimitReader(r, MaxBundleSizeBytes+1)
	size, err := io.Copy(h, limited)
	if err != nil {
		return "", size, fmt.Errorf("marketplace: read bundle: %w", err)
	}
	if size > MaxBundleSizeBytes {
		return "", size, ErrBundleTooLarge
	}
	if size == 0 {
		// A zero-byte bundle would still hash to the SHA-256 of the
		// empty string, but it cannot be a valid extension (no
		// manifest). The DB CHECK requires bundle_size_bytes > 0 too,
		// so rejecting here keeps the error surfaced at the right
		// layer for B6.
		return "", 0, fmt.Errorf("%w: bundle is empty", ErrInvalidManifest)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// HashBundleBytes is the in-memory convenience wrapper around
// HashBundle. Useful for tests and for the small-bundle code path
// where the caller has already accumulated the full payload (e.g.
// after a successful streaming upload to object storage where the
// uploader buffered the body for signature verification).
func HashBundleBytes(b []byte) (string, int64, error) {
	if int64(len(b)) > MaxBundleSizeBytes {
		return "", int64(len(b)), ErrBundleTooLarge
	}
	if len(b) == 0 {
		return "", 0, fmt.Errorf("%w: bundle is empty", ErrInvalidManifest)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), int64(len(b)), nil
}

// VerifyBundleHash returns nil iff sha256(r) matches expected (case-
// insensitively). The runtime install path calls this immediately
// after fetching the bundle from object storage so a tampered or
// truncated download surfaces as ErrBundleHashMismatch — never as a
// silent install of a poisoned bundle. Per spec §10, the bundle hash
// is the integrity anchor.
func VerifyBundleHash(r io.Reader, expected string) error {
	if expected == "" {
		return errors.New("marketplace: VerifyBundleHash: expected hash required")
	}
	got, _, err := HashBundle(r)
	if err != nil {
		return err
	}
	// CHECK constraint on the DB already lower-cases the persisted
	// value via the regex, but a caller-supplied string from a header
	// or query parameter could be upper-case; normalise both sides.
	if !equalASCIIFold(got, expected) {
		return fmt.Errorf("%w: expected %s, got %s", ErrBundleHashMismatch, expected, got)
	}
	return nil
}

// ErrBundleHashMismatch is returned by VerifyBundleHash when the
// streamed bytes hash to a different SHA-256 than the persisted
// bundle_hash on marketplace_extension_versions. The install runtime
// translates this into a hard install failure that surfaces in the
// install record's failure_reason column.
var ErrBundleHashMismatch = errors.New("marketplace: bundle hash mismatch")

// equalASCIIFold compares two lower-case hex strings without
// allocating. Used to normalise hash comparison; both values come
// from controlled sources (sha256 hex output / DB column with regex
// CHECK) so we never see multibyte runes.
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
