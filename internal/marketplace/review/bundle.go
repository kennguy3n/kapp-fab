package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundle"
)

// Bundle is the resolved-and-extracted bundle as the review
// pipeline sees it. Distinct from runtime.ResolvedBundle because
// review needs strictly more material:
//
//   - RawBytes for SignatureCheck (ed25519 verifies over the raw
//     .tar.gz body, not the resolved file tree).
//   - Files keyed by manifest-relative path so IconCheck and
//     UIStaticAnalysisCheck can read arbitrary files inside the
//     archive without having to re-parse the manifest's path
//     vocabulary.
//   - Hash so MaxSize / hash-drift checks can compare against the
//     `bundle_hash` column on the version row.
//
// Lifetime: a Bundle is constructed once per version per
// pipeline run; checks treat it as read-only. The struct is not
// shared across runs (no cache analog to bundle.CachingResolver
// here — review is one-shot per submission, retry-on-rescan).
type Bundle struct {
	Version  *marketplace.ExtensionVersion
	Manifest *marketplace.Manifest

	// RawBytes is the unmodified .tar.gz body fetched from the
	// publisher's CDN. Required for SignatureCheck.
	RawBytes []byte

	// Hash is the SHA-256 of RawBytes, lower-case hex. Recomputed
	// here from RawBytes (not trusted from Version.BundleHash) so
	// HashCheck can detect drift between the catalog row and the
	// CDN object.
	Hash string

	// Files is the post-extracted file map (path → body). Keys are
	// manifest-relative paths with the archive root directory
	// stripped — same convention as bundle.untarGzip.
	Files map[string][]byte
}

// SourceFetcher is the seam that lets the pipeline run against
// either an HTTP CDN (production) or an in-memory bytes table
// (tests). The pipeline always calls Fetch with the catalog's
// version row; the implementation is responsible for retrieving
// the bytes and returning them.
type SourceFetcher interface {
	// Fetch returns the raw .tar.gz body for the given version.
	// Implementations MAY cache; the pipeline does not.
	Fetch(ctx context.Context, version *marketplace.ExtensionVersion) ([]byte, error)
}

// HTTPSource fetches bundle bytes from the publisher's CDN over
// HTTPS. Mirrors bundle.HTTPResolver's transport tuning so review
// and B6 see the same per-fetch budget (timeout / max size).
//
// HTTPSource intentionally does NOT untar — that's the pipeline's
// concern (the pipeline needs the raw bytes for signature, AND the
// extracted files; doing both in one call keeps the contract sharp).
type HTTPSource struct {
	Client  *http.Client
	Timeout time.Duration
}

// NewHTTPSource returns an HTTPSource with sensible defaults.
func NewHTTPSource() *HTTPSource {
	return &HTTPSource{
		Client:  http.DefaultClient,
		Timeout: 30 * time.Second,
	}
}

// Fetch downloads the bundle bytes. Enforces a hard size cap of
// marketplace.MaxBundleSizeBytes; bundles larger than the cap are
// rejected without buffering past the limit. The pipeline runs
// BundleSizeCheck against the post-fetch RawBytes too; the cap
// here is purely defensive so a malicious URL can't exhaust the
// worker's RAM.
func (s *HTTPSource) Fetch(ctx context.Context, version *marketplace.ExtensionVersion) ([]byte, error) {
	if version == nil || version.BundleURL == "" {
		return nil, errors.New("review: nil version or empty bundle URL")
	}
	if !strings.HasPrefix(version.BundleURL, "https://") {
		// Defence in depth — the publisher submit endpoint
		// already enforces https for bundle_url at validation
		// time. If a malformed row sneaks through (e.g. a
		// hand-edited migration), refuse to fetch.
		return nil, fmt.Errorf("review: bundle URL must be https (got %q)", version.BundleURL)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, version.BundleURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("review: build request: %w", err)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("review: fetch bundle: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("review: bundle fetch returned %d", resp.StatusCode)
	}
	// +1 sentinel so we can tell "exactly at cap" from "over cap".
	limited := io.LimitReader(resp.Body, marketplace.MaxBundleSizeBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("review: read bundle: %w", err)
	}
	if int64(len(body)) > marketplace.MaxBundleSizeBytes {
		return nil, fmt.Errorf("review: bundle exceeds %d byte cap", marketplace.MaxBundleSizeBytes)
	}
	return body, nil
}

// MemorySource is the test-only SourceFetcher backed by an in-
// memory table of (version.ID → bytes). Concurrent-safe (the
// pipeline may resolve in parallel batches).
type MemorySource struct {
	bytes map[string][]byte
}

// NewMemorySource returns an empty MemorySource ready to be loaded
// via Set.
func NewMemorySource() *MemorySource {
	return &MemorySource{bytes: make(map[string][]byte)}
}

// Set registers the .tar.gz body for a version. Overwrites silently
// (tests intentionally re-load with mutated bytes for tamper cases).
func (m *MemorySource) Set(versionID string, body []byte) {
	if m.bytes == nil {
		m.bytes = make(map[string][]byte)
	}
	m.bytes[versionID] = append([]byte(nil), body...)
}

// Fetch returns the registered body for version.ID, or an error if
// the test forgot to Set one.
func (m *MemorySource) Fetch(_ context.Context, version *marketplace.ExtensionVersion) ([]byte, error) {
	if version == nil {
		return nil, errors.New("review: nil version")
	}
	if m.bytes == nil {
		return nil, fmt.Errorf("review: no bytes registered for %s", version.ID)
	}
	body, ok := m.bytes[version.ID.String()]
	if !ok {
		return nil, fmt.Errorf("review: no bytes registered for %s", version.ID)
	}
	return body, nil
}

// LoadReviewBundle fetches the raw .tar.gz, computes its hash,
// untars it through the canonical bundle.Untar (so per-file and
// cumulative caps are enforced once across review + B6), and
// parses the manifest. Returns a Bundle that any check can
// read.
//
// Bundle-shape errors (gzip corrupt / tar slip / manifest parse
// failure) are returned as errors here rather than as findings —
// the pipeline treats a fundamentally-malformed bundle as
// unreviewable and surfaces a single high-level "bundle.unloadable"
// finding. Individual checks should not have to defend against a
// nil manifest or a missing entries map.
func LoadReviewBundle(ctx context.Context, src SourceFetcher, version *marketplace.ExtensionVersion) (*Bundle, error) {
	if version == nil {
		return nil, errors.New("review: nil version")
	}
	if src == nil {
		return nil, errors.New("review: nil source")
	}
	raw, err := src.Fetch(ctx, version)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	files, err := bundle.Untar(raw)
	if err != nil {
		return nil, fmt.Errorf("review: untar bundle: %w", err)
	}
	manifestBytes, ok := files["kapp-extension.yaml"]
	if !ok {
		return nil, fmt.Errorf("review: bundle missing kapp-extension.yaml")
	}
	manifest, err := marketplace.ParseManifest(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("review: parse manifest: %w", err)
	}
	return &Bundle{
		Version:  version,
		Manifest: manifest,
		RawBytes: raw,
		Hash:     hash,
		Files:    files,
	}, nil
}
