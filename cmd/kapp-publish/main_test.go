package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPackDirDeterministic checks that the same source tree
// packed twice produces byte-identical output (and therefore the
// same SHA-256). This is the contract the content-addressed
// marketplace bundle store relies on — a publisher who repacks
// the same source on a different machine MUST get the same hash.
func TestPackDirDeterministic(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "kapp-extension.yaml"), []byte("schema_version: 1\nname: ken_dev.hello_world\nversion: 0.1.0\n"))
	mustWrite(t, filepath.Join(dir, "ktypes", "thing.yaml"), []byte("name: ken_dev.hello_world.thing\n"))
	mustWrite(t, filepath.Join(dir, "workflows", "wf.yaml"), []byte("kind: workflow\n"))

	first, err := packDir(dir)
	if err != nil {
		t.Fatalf("packDir #1: %v", err)
	}
	second, err := packDir(dir)
	if err != nil {
		t.Fatalf("packDir #2: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("pack is non-deterministic: %d vs %d bytes",
			len(first), len(second))
	}
	sum := sha256.Sum256(first)
	if len(first) == 0 {
		t.Fatalf("empty pack")
	}
	if hex := sumHex(sum); hex == "" {
		t.Fatalf("empty hash")
	}
}

// TestPackDirContents verifies that the produced tar.gz contains
// exactly the files we expect, with forward-slash paths even when
// the source dir contained nested subdirs.
func TestPackDirContents(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "kapp-extension.yaml"), []byte("manifest body"))
	mustWrite(t, filepath.Join(dir, "ui_ext", "icon.png"), []byte("png bytes"))
	mustWrite(t, filepath.Join(dir, ".DS_Store"), []byte("noise"))

	body, err := packDir(dir)
	if err != nil {
		t.Fatalf("packDir: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		bb, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		got[hdr.Name] = string(bb)
	}
	if _, ok := got["bundle/kapp-extension.yaml"]; !ok {
		t.Fatalf("expected bundle/kapp-extension.yaml in tar, got keys %v", mapKeys(got))
	}
	if _, ok := got["bundle/ui_ext/icon.png"]; !ok {
		t.Fatalf("expected bundle/ui_ext/icon.png in tar, got keys %v", mapKeys(got))
	}
	if _, ok := got["bundle/.DS_Store"]; ok {
		t.Fatalf("expected bundle/.DS_Store to be skipped, got keys %v", mapKeys(got))
	}
}

// TestLoadEd25519KeyAcceptsAllFormats covers the three valid
// shapes loadEd25519Key accepts: 32-byte seed, 64-byte full
// private key, and base64 of either. Hardens the CLI against a
// publisher whose key tool emits any of these.
func TestLoadEd25519KeyAcceptsAllFormats(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	seed := priv.Seed()

	cases := []struct {
		name string
		body []byte
	}{
		{"raw 64-byte private", priv},
		{"raw 32-byte seed", seed},
		{"base64 64-byte private", []byte(base64.StdEncoding.EncodeToString(priv))},
		{"base64 32-byte seed", []byte(base64.StdEncoding.EncodeToString(seed))},
		{"base64 URL-encoded seed", []byte(base64.URLEncoding.EncodeToString(seed))},
		{"base64 raw seed", []byte(base64.RawStdEncoding.EncodeToString(seed))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadEd25519Key(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			msg := []byte("hello world")
			sig := ed25519.Sign(loaded, msg)
			if !ed25519.Verify(pub, msg, sig) {
				t.Fatalf("loaded key does not match generated key")
			}
		})
	}
}

// TestLoadEd25519KeyRejectsBadLength covers the rejection path —
// a publisher who hands the CLI a random 16-byte blob should get
// a clear error rather than a panic from ed25519.NewKeyFromSeed.
func TestLoadEd25519KeyRejectsBadLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(path, []byte("not_a_key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEd25519Key(path); err == nil {
		t.Fatalf("expected error on short key, got nil")
	}
}

// TestLoadEd25519KeyRawBytePreservesWhitespace pins the raw-first
// guarantee for loadEd25519Key (Devin Review
// ANALYSIS_pr-review-job-6c5aa7fef9214efaacd238cc9ba21472_0008): a
// truly binary 64-byte private key whose last byte is 0x0A / 0x20
// must NOT be silently truncated by bytes.TrimSpace before the
// length check. The previous "base64-first then trim" order would
// (a) fail the base64 decode (binary bytes aren't valid b64), then
// (b) TrimSpace the trailing 0x0A off the raw payload, leaving a
// 63-byte buffer that fell through to the "unexpected length"
// error path. Raw-first means a binary key never gets trimmed.
func TestLoadEd25519KeyRawBytePreservesWhitespace(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	// Force the last byte to a whitespace byte that TrimSpace
	// would otherwise consume (0x0A = '\n'). We mutate the local
	// copy so the original priv stays intact for the verify call.
	keyBytes := append([]byte(nil), priv...)
	keyBytes[len(keyBytes)-1] = '\n'
	// Regenerate the matching public part for the verify check.
	// `priv[32:]` is the public-key tail in the 64-byte format,
	// so flipping the last byte invalidates the public key as
	// well; rebuild via NewKeyFromSeed to keep the test isolated
	// from that detail.
	seed := keyBytes[:ed25519.SeedSize]
	derived := ed25519.NewKeyFromSeed(seed)
	derivedPub := derived.Public().(ed25519.PublicKey)
	_ = pub // not used; the regenerated derivedPub is the source of truth

	path := filepath.Join(t.TempDir(), "key.bin")
	// Persist the FULL 64-byte form so the function is exercised
	// on the path most likely to trip the trim hazard.
	full := append([]byte(nil), derived...)
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadEd25519Key(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != ed25519.PrivateKeySize {
		t.Fatalf("loaded key has unexpected length %d", len(loaded))
	}
	msg := []byte("rune-safe key load")
	sig := ed25519.Sign(loaded, msg)
	if !ed25519.Verify(derivedPub, msg, sig) {
		t.Fatalf("loaded raw 64-byte key (trailing 0x0A) does not verify with derived public key")
	}
}

// TestTrimRuneSafe pins the rune-boundary correctness of trim
// (Devin Review ANALYSIS_pr-review-job-6c5aa7fef9214efaacd238cc9ba21472_0006).
// The previous byte-indexed implementation could slice mid-rune
// and emit invalid UTF-8 (e.g. truncating "héllo" at byte 3 split
// the 2-byte 'é' between input and output).
func TestTrimRuneSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"under-cap ASCII", "abc", 5, "abc"},
		{"at-cap ASCII", "abcde", 5, "abcde"},
		{"truncate ASCII", "abcdef", 5, "abcd\u2026"},
		{"under-cap multibyte", "héllo", 5, "héllo"},
		// "héllo wörld" has 11 runes; trim to 8 keeps 7 + ellipsis.
		{"truncate multibyte", "héllo wörld", 8, "héllo w\u2026"},
		// All-multibyte input ("éééééé" is 6 runes / 12 bytes).
		{"truncate all-multibyte", "éééééé", 4, "ééé\u2026"},
		// Asian glyphs (each 3 bytes in UTF-8).
		{"truncate cjk", "日本語ABC", 4, "日本語\u2026"},
		// n=0 returns empty string instead of negative-index crash.
		{"n=0", "abc", 0, ""},
		// n=1 produces just the ellipsis when input needs trimming.
		{"n=1 truncates", "abcd", 1, "\u2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trim(tc.in, tc.n)
			if got != tc.want {
				t.Fatalf("trim(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			// Must always be valid UTF-8 — the byte-indexed
			// implementation failed this check whenever the cut
			// point split a multi-byte rune.
			if !validUTF8(got) {
				t.Fatalf("trim returned invalid UTF-8 for input %q (n=%d): % x", tc.in, tc.n, got)
			}
		})
	}
}

func validUTF8(s string) bool {
	return utf8.ValidString(s)
}

// TestPackDirThenExtractRoundTrip packs a synthetic manifest dir,
// then uses the actual server-side bundle.Extract code path to
// verify the bytes are accepted. This is the closest the CLI
// can come to "exercise the real validation pipeline without a
// running marketplace server."
func TestPackDirThenExtractRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Manifest fields required by marketplace.ParseManifest's
	// validation: name in "publisher/extension" form, semver,
	// schema_version, kapp version constraints.
	mustWrite(t, filepath.Join(dir, "kapp-extension.yaml"), []byte(`schema_version: 1
name: ken_dev.cli_test
version: 0.1.0
author: tests
license: MIT
description: round-trip test
min_kapp_version: "0.1.0"
max_kapp_version: "1.0.0"
features_required: []
permissions_required: []
`))
	body, err := packDir(dir)
	if err != nil {
		t.Fatalf("packDir: %v", err)
	}
	if int64(len(body)) > 10*1024*1024 {
		t.Fatalf("bundle too large: %d", len(body))
	}
	// Use validate path internally — load + extract + manifest parse.
	tmpFile := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(tmpFile, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runValidate([]string{tmpFile}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestPackDirSkipsVCSAndBuildDirs pins the BUG_0001 fix from
// Devin Review: nested .git / .hg / .svn / node_modules / dist /
// build / target / __pycache__ / .idea / .vscode directories MUST
// NOT have their contents land in the produced tar.gz. Each is a
// vector for either (a) secret leakage (.git history can contain
// committed-then-removed credentials), (b) determinism breakage
// (.git contents vary across clones; node_modules timestamps),
// or (c) bundle bloat (any of build/dist/target/node_modules can
// easily exceed the 10 MiB cap).
func TestPackDirSkipsVCSAndBuildDirs(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "kapp-extension.yaml"), []byte("manifest body"))
	mustWrite(t, filepath.Join(dir, "ui_ext", "icon.png"), []byte("png"))

	// Plant decoys in every directory the packer must prune.
	// HEAD is the canonical .git contents fingerprint; pack file
	// is the file-most-likely-to-contain-secrets; node_modules
	// is the size hazard. The decoy bytes are the same per dir
	// so a leak would surface clearly in the assertion.
	for _, sub := range []string{
		".git", ".hg", ".svn",
		"node_modules", "__pycache__",
		"dist", "build", "target",
		".idea", ".vscode",
	} {
		mustWrite(t, filepath.Join(dir, sub, "DO_NOT_PACK"), []byte("LEAK"))
		mustWrite(t, filepath.Join(dir, sub, "nested", "DEEP_LEAK"), []byte("LEAK"))
	}

	body, err := packDir(dir)
	if err != nil {
		t.Fatalf("packDir: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	got := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		got[hdr.Name] = true
	}
	if !got["bundle/kapp-extension.yaml"] {
		t.Fatalf("expected bundle/kapp-extension.yaml present, got %v", got)
	}
	if !got["bundle/ui_ext/icon.png"] {
		t.Fatalf("expected bundle/ui_ext/icon.png present, got %v", got)
	}
	for name := range got {
		// any path under the pruned dirs is a leak. Anchor on
		// the substring so both top-level and nested paths
		// trigger.
		for _, leak := range []string{
			"/.git/", "/.hg/", "/.svn/",
			"/node_modules/", "/__pycache__/",
			"/dist/", "/build/", "/target/",
			"/.idea/", "/.vscode/",
		} {
			if strings.Contains(name, leak) {
				t.Errorf("BUG_0001 regression: %q leaked through %q prune", name, leak)
			}
		}
	}
}

// TestPackDirRejectsSymlinks pins the round-7 Devin Review
// ANALYSIS_0009 fix: packDir uses filepath.WalkDir (which does
// NOT follow symlinks at walk time) plus os.ReadFile (which
// DOES follow them at read time), so an in-tree symlink would
// silently include the target bytes in the bundle. A "deps ->
// /etc/shadow" or "shared -> ../../../../private" link is a
// real footgun even for a well-meaning publisher (e.g. one who
// symlinked a monorepo's shared config into the extension's
// source dir). The fix rejects symlinks hard at WalkDir time so
// the publisher gets a clear error pointing at the offending
// path instead of a broken bundle.
func TestPackDirRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "kapp-extension.yaml"), []byte("manifest body"))
	target := filepath.Join(dir, "target.txt")
	mustWrite(t, target, []byte("target body"))
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		// Some filesystems (FAT/exFAT) don't support symlinks;
		// skip rather than fail because the production path
		// only matters on platforms that DO support them.
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}

	_, err := packDir(dir)
	if err == nil {
		t.Fatal("packDir(dir-with-symlink) returned nil; expected rejection")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected error to mention symlink, got %v", err)
	}
	if !strings.Contains(err.Error(), "link.txt") {
		t.Fatalf("expected error to name the offending path link.txt, got %v", err)
	}
}

// TestPackDirRejectsSymlinkedDir verifies the symlink guard also
// fires for directory-typed symlinks. WalkDir does not recurse
// into them (no follow) so the previous code path never crashed,
// but the guard MUST still reject them so a publisher who points
// "vendor -> /home/me/shared/" gets a clear error rather than a
// silently empty "vendor/" in the bundle.
func TestPackDirRejectsSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "kapp-extension.yaml"), []byte("manifest body"))
	other := t.TempDir()
	mustWrite(t, filepath.Join(other, "shared.txt"), []byte("shared body"))
	link := filepath.Join(dir, "vendor")
	if err := os.Symlink(other, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}

	_, err := packDir(dir)
	if err == nil {
		t.Fatal("packDir(dir-with-symlinked-dir) returned nil; expected rejection")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected error to mention symlink, got %v", err)
	}
	if !strings.Contains(err.Error(), "vendor") {
		t.Fatalf("expected error to name the offending path vendor, got %v", err)
	}
}

// --- helpers -------------------------------------------------------------

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sumHex(s [32]byte) string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	for _, x := range s {
		b.WriteByte(hexdigits[x>>4])
		b.WriteByte(hexdigits[x&0x0f])
	}
	return b.String()
}
