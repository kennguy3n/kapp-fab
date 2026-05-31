package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundlestore"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
)

// TestLocalResolver_MatchAndDelegate covers the three branches of
// LocalResolver.Resolve:
//
//	1) bundle_url starts with the marketplace URL base → fetch
//	   from the in-memory store, hash-verify, Extract.
//	2) bundle_url is something else (e.g. publisher CDN) →
//	   delegate to the wrapped Resolver.
//	3) bundle_url is malformed or hash doesn't parse → fall
//	   through to delegate, which can fail safely.
func TestLocalResolver_MatchAndDelegate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	body := buildTinyBundle(t)
	hash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(hash[:])

	store := &fakeByteSource{body: body, hash: hashHex}
	delegate := &fakeResolver{}

	base := "https://kapp.example.com"
	lr := NewLocalResolver(delegate, store, base)

	t.Run("matches marketplace URL → reads from store", func(t *testing.T) {
		ver := &marketplace.ExtensionVersion{
			ID:         uuid.New(),
			BundleURL:  base + "/api/v1/marketplace/bundles/" + hashHex + ".tar.gz",
			BundleHash: hashHex,
			BundleSizeBytes: int64(len(body)),
		}
		rb, err := lr.Resolve(ctx, ver)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if rb == nil || rb.Manifest == nil {
			t.Fatalf("nil result")
		}
		if delegate.called {
			t.Fatalf("delegate should not be called for marketplace URLs")
		}
	})

	t.Run("bare-hash form (no .tar.gz suffix)", func(t *testing.T) {
		delegate.called = false
		ver := &marketplace.ExtensionVersion{
			BundleURL:  base + "/api/v1/marketplace/bundles/" + hashHex,
			BundleHash: hashHex,
			BundleSizeBytes: int64(len(body)),
		}
		_, err := lr.Resolve(ctx, ver)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if delegate.called {
			t.Fatalf("delegate should not be called")
		}
	})

	t.Run("publisher CDN URL → delegates", func(t *testing.T) {
		delegate.called = false
		ver := &marketplace.ExtensionVersion{
			BundleURL:  "https://publisher.example.com/bundle.tar.gz",
			BundleHash: hashHex,
			BundleSizeBytes: int64(len(body)),
		}
		_, err := lr.Resolve(ctx, ver)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !delegate.called {
			t.Fatalf("delegate should be called for non-marketplace URLs")
		}
	})

	t.Run("hash mismatch between URL and version row", func(t *testing.T) {
		// Even though URL matches the prefix, the URL hash
		// disagrees with the version row's bundle_hash. Reject
		// with ErrBundleHashMismatch before reading.
		bogusHash := strings.Repeat("0", 64)
		ver := &marketplace.ExtensionVersion{
			BundleURL:  base + "/api/v1/marketplace/bundles/" + bogusHash + ".tar.gz",
			BundleHash: hashHex,
			BundleSizeBytes: int64(len(body)),
		}
		_, err := lr.Resolve(ctx, ver)
		if !errors.Is(err, marketplace.ErrBundleHashMismatch) {
			t.Fatalf("expected ErrBundleHashMismatch, got %v", err)
		}
	})

	t.Run("bytes hash mismatch (store row tampered)", func(t *testing.T) {
		// Store returns bytes that don't match the version row's
		// declared hash. Reject after read.
		other := []byte("not the real bundle")
		tampered := &fakeByteSource{body: other, hash: hashHex}
		lr2 := NewLocalResolver(delegate, tampered, base)
		ver := &marketplace.ExtensionVersion{
			BundleURL:  base + "/api/v1/marketplace/bundles/" + hashHex + ".tar.gz",
			BundleHash: hashHex,
			BundleSizeBytes: int64(len(body)),
		}
		_, err := lr2.Resolve(ctx, ver)
		if !errors.Is(err, marketplace.ErrBundleHashMismatch) {
			t.Fatalf("expected ErrBundleHashMismatch, got %v", err)
		}
	})
}

// TestLocalResolver_DisabledFallback: with urlBase="" or store=nil
// every URL passes through to the delegate, even URLs that look
// marketplace-like. Captures the "deploy hasn't enabled marketplace-
// hosted bundles" mode.
func TestLocalResolver_DisabledFallback(t *testing.T) {
	t.Parallel()
	delegate := &fakeResolver{}
	lr := NewLocalResolver(delegate, nil, "")
	ver := &marketplace.ExtensionVersion{
		BundleURL:  "https://kapp.example.com/api/v1/marketplace/bundles/abc.tar.gz",
		BundleHash: strings.Repeat("a", 64),
	}
	_, _ = lr.Resolve(context.Background(), ver)
	if !delegate.called {
		t.Fatalf("delegate must be called when LocalResolver is disabled")
	}
}

// TestLocalResolver_NotFound: store returns ErrBundleNotFound, the
// resolver translates to the bundle package's sentinel so callers
// can map to 404.
//
// Also pins Devin Review
// ANALYSIS_pr-review-job-20b9bdccfe6d463c9a4d6ac7f0fea816_0001:
// the wrapped error MUST carry operator-actionable context (the
// marketplace URL base, the hash, and the documented multi-replica
// constraint) so the operator can diagnose a non-shared bundlestore
// from the access log without having to grep code.
func TestLocalResolver_NotFound(t *testing.T) {
	t.Parallel()
	delegate := &fakeResolver{}
	store := &fakeByteSource{err: bundlestore.ErrBundleNotFound}
	base := "https://kapp.example.com"
	lr := NewLocalResolver(delegate, store, base)
	h := strings.Repeat("a", 64)
	bundleURL := base + "/api/v1/marketplace/bundles/" + h
	_, err := lr.Resolve(context.Background(), &marketplace.ExtensionVersion{
		BundleURL:  bundleURL,
		BundleHash: h,
	})
	if !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("expected ErrBundleNotFound, got %v", err)
	}
	// Wrapped message must name the hash, the URL, and the
	// "shared across replicas" constraint so an operator reading
	// the access log can diagnose the misconfiguration.
	msg := err.Error()
	for _, want := range []string{h, bundleURL, "shared across replicas"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("wrapped error missing operator context %q: %s", want, msg)
		}
	}
	if delegate.called {
		t.Fatalf("delegate must NOT be called on local-store miss: HTTPResolver fall-through hits the same tenant-gated endpoint and fails 401 — the architecturally honest fix is to surface the misconfiguration via the wrapped error, not attempt a doomed retry")
	}
}

// --- helpers -------------------------------------------------------------

type fakeByteSource struct {
	body []byte
	hash string
	err  error
}

func (f *fakeByteSource) Fetch(_ context.Context, _ string) (*bundlestore.BundleUpload, io.ReadCloser, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	up := &bundlestore.BundleUpload{
		ContentHash: f.hash,
		SizeBytes:   int64(len(f.body)),
		ContentType: bundlestore.DefaultContentType,
	}
	return up, io.NopCloser(bytes.NewReader(f.body)), nil
}

type fakeResolver struct {
	called bool
}

func (f *fakeResolver) Resolve(_ context.Context, _ *marketplace.ExtensionVersion) (*runtime.ResolvedBundle, error) {
	f.called = true
	return &runtime.ResolvedBundle{Manifest: &marketplace.Manifest{Name: "fake.delegate", Version: "0.0.0"}}, nil
}

// buildTinyBundle constructs a minimal valid bundle for the
// resolver to extract: one root directory + a kapp-extension.yaml
// with the bare-minimum manifest fields.
func buildTinyBundle(t *testing.T) []byte {
	t.Helper()
	manifest := []byte(`schema_version: 1
name: ken_dev.local_resolver_test
version: 0.0.1
author: tests
license: MIT
description: local resolver test bundle
min_kapp_version: "0.1.0"
max_kapp_version: "1.0.0"
features_required: []
permissions_required: []
`)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "bundle/kapp-extension.yaml",
		Mode:     0o644,
		Size:     int64(len(manifest)),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
