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
