package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// makeBundle constructs a real .tar.gz fixture with the documented
// EXTENSION_SPEC §2 shape. Returns (body, sha256-hex). The fixture
// is the same shape the runtime + integration tests will exercise
// in B6 (publisher submits a bundle → install handler resolves it).
// Tests must NOT mock the algorithm here — bundle hashing is the
// integrity anchor between marketplace catalog and runtime, so
// every test feeds real bytes through Extract / HTTPResolver.
func makeBundle(t *testing.T, root string, files map[string]string) (body []byte, hash string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Write the root dir entry first so an archiver tool inspecting
	// the bundle sees the canonical layout. The resolver skips dir
	// entries; this is purely for round-trip correctness.
	hdr := &tar.Header{
		Name:     root + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("makeBundle: write dir header: %v", err)
	}
	// File order matters for canonical archive hashing — tests rely
	// on a stable bundle hash across runs. Use the natural map
	// iteration order; Go's map iteration is randomized but each
	// individual test call lives or dies on the final SHA so we
	// don't compare hashes between tests.
	for name, content := range files {
		hdr := &tar.Header{
			Name:     path.Join(root, name),
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("makeBundle: header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("makeBundle: write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("makeBundle: close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("makeBundle: close gzip: %v", err)
	}
	body = buf.Bytes()
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])
}

// canonicalManifestYAML returns a minimal-but-valid kapp-extension
// manifest used by every resolver test. The publisher + slug names
// must be lower-snake-case per spec §3 (and matching the manifest
// validator's regex at internal/marketplace/manifest.go:374).
func canonicalManifestYAML(publisher, slug string) string {
	return fmt.Sprintf(`schema_version: 1
name: "%[1]s.%[2]s"
version: "1.0.0"
author: "ACME Corp"
license: "MIT"
description: |
  Test extension fixture.
homepage: "https://example.com/%[2]s"
support_email: "support@example.com"
min_kapp_version: "1.0.0"
features_required:
  - "inventory"
permissions_required:
  - "inventory.read"
ktypes:
  - schema: ./ktypes/shipping_label.json
workflows:
  - definition: ./workflows/shipping_workflow.json
agent_tools:
  - definition: ./tools/generate_label.json
    handler: webhook
    endpoint: "${EXTENSION_WEBHOOK_BASE}/generate-label"
    timeout: "10s"
    retry:
      max_attempts: 2
      backoff: "exponential"
`, publisher, slug)
}

// canonicalFiles returns the bundle file map matching canonicalManifestYAML.
func canonicalFiles(publisher, slug string) map[string]string {
	return map[string]string{
		"kapp-extension.yaml":             canonicalManifestYAML(publisher, slug),
		"ktypes/shipping_label.json":      fmt.Sprintf(`{"name":"ext.%s.shipping_label","fields":[{"name":"tracking_no","type":"string"}]}`, publisher),
		"workflows/shipping_workflow.json": fmt.Sprintf(`{"name":"ext.%s.shipping_flow","states":["draft","shipped"]}`, publisher),
		"tools/generate_label.json":       fmt.Sprintf(`{"name":"ext.%s.generate_label","description":"Create shipping label"}`, publisher),
	}
}

func TestExtract_HappyPath(t *testing.T) {
	body, _ := makeBundle(t, "acme-shipping", canonicalFiles("acme", "shipping"))
	rb, err := Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rb.Manifest == nil {
		t.Fatal("Manifest nil")
	}
	if rb.Manifest.Name != "acme.shipping" {
		t.Fatalf("Manifest.Name = %q, want acme.shipping", rb.Manifest.Name)
	}
	if rb.Manifest.Publisher != "acme" || rb.Manifest.Slug != "shipping" {
		t.Fatalf("Publisher/Slug = %q/%q, want acme/shipping",
			rb.Manifest.Publisher, rb.Manifest.Slug)
	}
	if len(rb.KTypes) != 1 || rb.KTypes[0].Name != "ext.acme.shipping_label" {
		t.Fatalf("KTypes = %+v", rb.KTypes)
	}
	if len(rb.Workflows) != 1 || rb.Workflows[0].Name != "ext.acme.shipping_flow" {
		t.Fatalf("Workflows = %+v", rb.Workflows)
	}
	if len(rb.AgentTools) != 1 || rb.AgentTools[0].Name != "ext.acme.generate_label" {
		t.Fatalf("AgentTools = %+v", rb.AgentTools)
	}
}

func TestExtract_MissingManifest(t *testing.T) {
	files := canonicalFiles("acme", "shipping")
	delete(files, "kapp-extension.yaml")
	body, _ := makeBundle(t, "acme-shipping", files)
	_, err := Extract(body)
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
}

func TestExtract_MissingReferencedFile(t *testing.T) {
	files := canonicalFiles("acme", "shipping")
	delete(files, "ktypes/shipping_label.json")
	body, _ := makeBundle(t, "acme-shipping", files)
	_, err := Extract(body)
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
	if !strings.Contains(err.Error(), "ktype schema") {
		t.Fatalf("Extract error message should mention the kind: %v", err)
	}
}

func TestExtract_JSONFileMissingName(t *testing.T) {
	files := canonicalFiles("acme", "shipping")
	files["ktypes/shipping_label.json"] = `{"fields":[{"name":"foo","type":"string"}]}`
	body, _ := makeBundle(t, "acme-shipping", files)
	_, err := Extract(body)
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
	if !strings.Contains(err.Error(), "missing top-level `name`") {
		t.Fatalf("Extract error message wrong: %v", err)
	}
}

func TestExtract_SymlinkRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must := func(h *tar.Header) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("hdr: %v", err)
		}
	}
	must(&tar.Header{Name: "acme-shipping/", Typeflag: tar.TypeDir, Mode: 0o755})
	must(&tar.Header{Name: "acme-shipping/kapp-extension.yaml", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	must(&tar.Header{Name: "acme-shipping/evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o644})
	tw.Close()
	gz.Close()
	_, err := Extract(buf.Bytes())
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error message should mention symlink: %v", err)
	}
}

func TestExtract_TraversalRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Tar headers with ".." trying to escape the archive root.
	hdr := &tar.Header{Name: "acme-shipping/../../etc/passwd", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("hdr: %v", err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tw.Close()
	gz.Close()
	_, err := Extract(buf.Bytes())
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
}

func TestExtract_PerFileSizeCap(t *testing.T) {
	files := canonicalFiles("acme", "shipping")
	// Per-file cap is 2 MiB; build a 2.5 MiB blob in a referenced
	// file (the agent tool definition). Use a JSON document so the
	// resolver's JSON parser doesn't reject early on a different
	// failure mode.
	huge := strings.Repeat("a", int(marketplace.MaxSingleFileBytes+1024))
	files["tools/generate_label.json"] = fmt.Sprintf(`{"name":"ext.acme.x","description":%q}`, huge)
	body, _ := makeBundle(t, "acme-shipping", files)
	_, err := Extract(body)
	if !errors.Is(err, ErrBundleExceedsLimit) {
		t.Fatalf("Extract: want ErrBundleExceedsLimit, got %v", err)
	}
}

func TestExtract_DuplicateEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i < 2; i++ {
		hdr := &tar.Header{
			Name:     "acme-shipping/kapp-extension.yaml",
			Typeflag: tar.TypeReg,
			Size:     1,
			Mode:     0o644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("hdr: %v", err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tw.Close()
	gz.Close()
	_, err := Extract(buf.Bytes())
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
}

func TestExtract_MultipleRoots(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"acme/kapp-extension.yaml", "evil/payload.json"} {
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("hdr: %v", err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tw.Close()
	gz.Close()
	_, err := Extract(buf.Bytes())
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
}

func TestExtract_BadGzip(t *testing.T) {
	_, err := Extract([]byte("not a gzip"))
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
}

func TestExtract_EmptyBody(t *testing.T) {
	_, err := Extract(nil)
	if !errors.Is(err, ErrBundleMalformed) {
		t.Fatalf("Extract: want ErrBundleMalformed, got %v", err)
	}
}

func TestHTTPResolver_HappyPath(t *testing.T) {
	body, hash := makeBundle(t, "acme-shipping", canonicalFiles("acme", "shipping"))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	r := NewHTTPResolver(HTTPResolverOptions{Client: srv.Client()})
	ver := &marketplace.ExtensionVersion{
		ID:              uuid.New(),
		BundleURL:       srv.URL + "/bundle.tar.gz",
		BundleHash:      hash,
		BundleSizeBytes: int64(len(body)),
	}
	rb, err := r.Resolve(context.Background(), ver)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rb.Manifest.Name != "acme.shipping" {
		t.Fatalf("Manifest.Name = %q, want acme.shipping", rb.Manifest.Name)
	}
}

func TestHTTPResolver_HashMismatch(t *testing.T) {
	body, _ := makeBundle(t, "acme-shipping", canonicalFiles("acme", "shipping"))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	r := NewHTTPResolver(HTTPResolverOptions{Client: srv.Client()})
	ver := &marketplace.ExtensionVersion{
		ID:         uuid.New(),
		BundleURL:  srv.URL + "/bundle.tar.gz",
		BundleHash: strings.Repeat("0", 64), // wrong hash
	}
	_, err := r.Resolve(context.Background(), ver)
	if !errors.Is(err, marketplace.ErrBundleHashMismatch) {
		t.Fatalf("Resolve: want ErrBundleHashMismatch, got %v", err)
	}
}

func TestHTTPResolver_HTTPSchemeRejected(t *testing.T) {
	r := NewHTTPResolver(HTTPResolverOptions{})
	ver := &marketplace.ExtensionVersion{
		ID:         uuid.New(),
		BundleURL:  "http://insecure.example/bundle.tar.gz",
		BundleHash: strings.Repeat("0", 64),
	}
	_, err := r.Resolve(context.Background(), ver)
	if !errors.Is(err, ErrBundleTransportInsecure) {
		t.Fatalf("Resolve: want ErrBundleTransportInsecure, got %v", err)
	}
}

func TestHTTPResolver_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	r := NewHTTPResolver(HTTPResolverOptions{Client: srv.Client()})
	ver := &marketplace.ExtensionVersion{
		ID:         uuid.New(),
		BundleURL:  srv.URL + "/missing.tar.gz",
		BundleHash: strings.Repeat("0", 64),
	}
	_, err := r.Resolve(context.Background(), ver)
	if !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("Resolve: want ErrBundleNotFound, got %v", err)
	}
}

func TestHTTPResolver_TooLarge(t *testing.T) {
	// Server returns more than MaxBundleSizeBytes; the resolver
	// should refuse instead of buffering the whole body.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		const chunk = 64 * 1024
		buf := make([]byte, chunk)
		written := int64(0)
		for written < marketplace.MaxBundleSizeBytes+2*chunk {
			n, err := w.Write(buf)
			if err != nil {
				return
			}
			written += int64(n)
		}
	}))
	defer srv.Close()
	r := NewHTTPResolver(HTTPResolverOptions{Client: srv.Client()})
	ver := &marketplace.ExtensionVersion{
		ID:         uuid.New(),
		BundleURL:  srv.URL + "/huge.tar.gz",
		BundleHash: strings.Repeat("0", 64),
	}
	_, err := r.Resolve(context.Background(), ver)
	if !errors.Is(err, marketplace.ErrBundleTooLarge) {
		t.Fatalf("Resolve: want ErrBundleTooLarge, got %v", err)
	}
}

func TestHTTPResolver_5xxIsFetchFailed(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := NewHTTPResolver(HTTPResolverOptions{Client: srv.Client()})
	ver := &marketplace.ExtensionVersion{
		ID:         uuid.New(),
		BundleURL:  srv.URL + "/x.tar.gz",
		BundleHash: strings.Repeat("0", 64),
	}
	_, err := r.Resolve(context.Background(), ver)
	if !errors.Is(err, ErrBundleFetchFailed) {
		t.Fatalf("Resolve: want ErrBundleFetchFailed, got %v", err)
	}
}

func TestInMemoryResolver(t *testing.T) {
	body, _ := makeBundle(t, "acme-shipping", canonicalFiles("acme", "shipping"))
	rb, err := Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	r := NewInMemoryResolver()
	id := uuid.New()
	r.Set(id.String(), rb)
	out, err := r.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: id})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out != rb {
		t.Fatal("InMemoryResolver should return the same *ResolvedBundle pointer")
	}
	_, err = r.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: uuid.New()})
	if !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("missing version: want ErrBundleNotFound, got %v", err)
	}
}

// Silence the unused-import linter for io if a future edit drops
// reflection on body sizes. Currently every test path uses it
// directly.
var _ = io.Discard
