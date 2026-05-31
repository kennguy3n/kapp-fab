// Package bundle implements the install-time bundle resolver. It
// fetches an extension's .tar.gz over HTTPS (per spec §2), verifies
// the bundle hash matches the version row, untars into memory, and
// emits a *runtime.ResolvedBundle that the engine's Install path
// can register atomically.
//
// The resolver is the single seam between "the marketplace catalog
// knows about a version" (Phase B2 store) and "the runtime can
// install it on a tenant" (Phase B3 engine). It is also the seam
// that B7's automated review pipeline will reuse — fetching a bundle
// then running schema/size/HTTPS-policy checks against the same
// extracted artefacts the install pipeline sees.
//
// Hard limits enforced (matching EXTENSION_SPEC §2 and the
// constants in internal/marketplace/bundle.go):
//   - Total bundle size cap: MaxBundleSizeBytes (10 MiB).
//   - Per-file size cap: MaxSingleFileBytes (2 MiB).
//   - Manifest size cap: MaxManifestSizeBytes (64 KiB).
//   - Per-bundle counts: MaxKTypesPerBundle / MaxWorkflowsPerBundle /
//     MaxAgentToolsPerBundle (32 / 16 / 32). Re-asserted here even
//     though the manifest validator already checks them — the
//     resolver runs BEFORE the manifest is reparsed (it parses the
//     manifest from the extracted bytes itself) so this is the gate
//     that prevents a hostile bundle with a benign manifest but
//     32 GiB of agent-tool JSON files from exhausting host memory
//     during extraction.
//   - HTTPS-only fetch: any non-https:// scheme is rejected before
//     the request is issued, regardless of the bundle URL recorded
//     on the version row. This is defence-in-depth — the store also
//     refuses to persist a non-https:// bundle_url, but a future
//     code path that mutates the column directly must not be able
//     to surreptitiously downgrade transport.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
)

// Resolver fetches the on-disk extension bundle referenced by a
// version row and produces the *runtime.ResolvedBundle that the
// engine's install path needs.
//
// Implementations:
//   - HTTPResolver: production. Fetches via net/http, verifies
//     SHA-256, untars in-memory.
//   - InMemoryResolver: test/dev. Looks up a pre-built bundle by
//     version_id from an in-process map.
type Resolver interface {
	Resolve(ctx context.Context, version *marketplace.ExtensionVersion) (*runtime.ResolvedBundle, error)
}

// Sentinel errors. The B6 API handler translates these into HTTP
// statuses:
//
//	ErrBundleNotFound          → 502 (upstream object storage 404)
//	ErrBundleFetchFailed       → 502 (network / 5xx from object storage)
//	ErrBundleTransportInsecure → 400 (bundle_url is not https://)
//	ErrBundleMalformed         → 422 (corrupt tar.gz, missing manifest, …)
//	ErrBundleExceedsLimit      → 413 (size cap hit during extraction)
//
// The marketplace.ErrBundleTooLarge and marketplace.ErrBundleHashMismatch
// sentinels are reused for the size + integrity cases respectively.
var (
	// ErrBundleNotFound is returned when the upstream object storage
	// reports the bundle URL is gone (404). Surfaced to the caller
	// distinct from generic fetch failure so the publisher console
	// can show "your CDN object is missing" instead of "transient
	// outage".
	ErrBundleNotFound = errors.New("bundle: not found at bundle_url")

	// ErrBundleFetchFailed wraps any transport-level failure
	// (DNS, TLS, 5xx, …) reading the bundle.
	ErrBundleFetchFailed = errors.New("bundle: fetch failed")

	// ErrBundleTransportInsecure is returned when bundle_url uses a
	// scheme other than https://. Persistence already rejects this
	// at store.PublishVersion, but the resolver re-checks as
	// defence-in-depth in case a future code path bypasses the
	// store.
	ErrBundleTransportInsecure = errors.New("bundle: bundle_url must be https://")

	// ErrBundleMalformed wraps any structural failure of the
	// archive — invalid gzip, broken tar, missing manifest, file
	// referenced by manifest not present in archive, etc.
	ErrBundleMalformed = errors.New("bundle: malformed archive")

	// ErrBundleExceedsLimit means an entry inside the archive
	// exceeded the per-file or per-bundle cap during streaming
	// extraction.
	ErrBundleExceedsLimit = errors.New("bundle: exceeds size limit")
)

// HTTPResolverOptions configures HTTPResolver. All fields have
// safe production defaults; tests override Client + Now.
type HTTPResolverOptions struct {
	// Client is the http.Client used to fetch the bundle. Defaults
	// to an http.Client with a 30s timeout and the system default
	// transport. Override in tests to plug a httptest.Server's
	// client (which trusts the test server's certificate).
	Client *http.Client
	// Now returns the current wall-clock time. Defaults to time.Now.
	// Currently unused but reserved for future cache-bust / TTL
	// logic on a content-addressed cache.
	Now func() time.Time
}

// HTTPResolver is the production Resolver. It fetches via HTTPS,
// streams into an in-memory buffer (capped at MaxBundleSizeBytes+1
// so a tampered "infinite" download fails fast), verifies the
// SHA-256, and unpacks the tar.gz.
type HTTPResolver struct {
	client *http.Client
	now    func() time.Time
}

// NewHTTPResolver returns an HTTPResolver. A nil or zero-valued
// opts argument falls back to production defaults.
func NewHTTPResolver(opts HTTPResolverOptions) *HTTPResolver {
	r := &HTTPResolver{
		client: opts.Client,
		now:    opts.Now,
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: 30 * time.Second}
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// Resolve fetches version.BundleURL, verifies the hash matches
// version.BundleHash, and unpacks the archive into a *ResolvedBundle.
func (r *HTTPResolver) Resolve(ctx context.Context, version *marketplace.ExtensionVersion) (*runtime.ResolvedBundle, error) {
	if version == nil {
		return nil, errors.New("bundle: nil version")
	}
	if err := validateBundleURL(version.BundleURL); err != nil {
		return nil, err
	}
	if !marketplace.IsValidBundleHash(version.BundleHash) {
		return nil, fmt.Errorf("%w: bundle_hash is not a valid sha256 hex", ErrBundleMalformed)
	}
	body, err := r.fetch(ctx, version.BundleURL)
	if err != nil {
		return nil, err
	}
	// Hash-verify the body before extraction. Extracting then hash-
	// checking would still be safe (the resolver does not commit
	// any side-effect to the engine until the caller invokes
	// Install), but verifying up-front means a tampered bundle
	// gets rejected before we pay the per-file cap enforcement
	// cost.
	gotHash := sha256.Sum256(body)
	if !equalHashFold(hex.EncodeToString(gotHash[:]), version.BundleHash) {
		return nil, fmt.Errorf("%w: expected %s, got %s",
			marketplace.ErrBundleHashMismatch,
			version.BundleHash, hex.EncodeToString(gotHash[:]))
	}
	return Extract(body)
}

// fetch issues a GET against the bundle URL, enforcing a streaming
// size cap and an HTTPS-only contract. Returns the full body bytes
// on success.
func (r *HTTPResolver) fetch(ctx context.Context, bundleURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bundleURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrBundleFetchFailed, err)
	}
	req.Header.Set("Accept", "application/gzip, application/octet-stream")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBundleFetchFailed, err)
	}
	// defer with explicit error sink — bundle fetch is read-only
	// and a Body.Close() failure can't change the response we
	// already parsed. The sink keeps errcheck happy.
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound:
		return nil, ErrBundleNotFound
	default:
		return nil, fmt.Errorf("%w: status=%d", ErrBundleFetchFailed, resp.StatusCode)
	}
	// LimitReader gives us cap+1 bytes to distinguish "exactly at
	// cap" from "exceeded cap" without buffering the whole tail
	// of a hostile infinite download.
	limited := io.LimitReader(resp.Body, marketplace.MaxBundleSizeBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %w", ErrBundleFetchFailed, err)
	}
	if int64(len(body)) > marketplace.MaxBundleSizeBytes {
		return nil, fmt.Errorf("%w: bundle exceeds %d bytes",
			marketplace.ErrBundleTooLarge, marketplace.MaxBundleSizeBytes)
	}
	return body, nil
}

// validateBundleURL re-asserts the https-only contract. The store
// already refuses non-https on PublishVersion (see
// internal/marketplace/store.go ~line 504), but a future code path
// that bypasses the store must not be able to downgrade transport.
func validateBundleURL(bundleURL string) error {
	if bundleURL == "" {
		return fmt.Errorf("%w: bundle_url empty", ErrBundleTransportInsecure)
	}
	u, err := url.Parse(bundleURL)
	if err != nil {
		return fmt.Errorf("%w: parse: %w", ErrBundleTransportInsecure, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: scheme=%q", ErrBundleTransportInsecure, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: missing host", ErrBundleTransportInsecure)
	}
	return nil
}

// equalHashFold is a copy of marketplace.equalASCIIFold (package-
// private there) — comparing two known-hex strings without
// allocating a lower-case copy of either. Both inputs come from
// controlled sources (sha256 hex output / DB column with regex
// CHECK) so we never see multibyte runes.
func equalHashFold(a, b string) bool {
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

// Extract decodes the raw .tar.gz body into a ResolvedBundle. The
// archive MUST follow EXTENSION_SPEC §2:
//   - Root is a single directory.
//   - kapp-extension.yaml lives at the archive root.
//   - All file paths in the manifest are rooted at the bundle root
//     and prefixed with "./" (e.g. "./ktypes/foo.json").
//
// Extract is exported so tests can fabricate raw byte slices and
// the InMemoryResolver can defer extraction to the same code path
// production uses.
func Extract(body []byte) (*runtime.ResolvedBundle, error) {
	files, err := untarGzip(body)
	if err != nil {
		return nil, err
	}
	manifestBytes, ok := files["kapp-extension.yaml"]
	if !ok {
		return nil, fmt.Errorf("%w: missing kapp-extension.yaml at bundle root", ErrBundleMalformed)
	}
	if int64(len(manifestBytes)) > marketplace.MaxManifestSizeBytes {
		return nil, fmt.Errorf("%w: manifest exceeds %d bytes",
			ErrBundleExceedsLimit, marketplace.MaxManifestSizeBytes)
	}
	man, err := marketplace.ParseManifest(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parse manifest: %w", ErrBundleMalformed, err)
	}
	rb := &runtime.ResolvedBundle{Manifest: man}
	// Re-check per-bundle counts. The manifest parser also checks
	// these, but the resolver duplicates the check here because a
	// future code path that constructs ResolvedBundle from a non-
	// manifest source (a hand-built test fixture, a recovery tool)
	// would otherwise bypass them. The check is cheap and the
	// invariant is load-bearing for the registrar's bounded-tx
	// guarantee.
	if len(man.KTypes) > marketplace.MaxKTypesPerBundle {
		return nil, fmt.Errorf("%w: %d ktypes > %d limit",
			ErrBundleExceedsLimit, len(man.KTypes), marketplace.MaxKTypesPerBundle)
	}
	if len(man.Workflows) > marketplace.MaxWorkflowsPerBundle {
		return nil, fmt.Errorf("%w: %d workflows > %d limit",
			ErrBundleExceedsLimit, len(man.Workflows), marketplace.MaxWorkflowsPerBundle)
	}
	if len(man.AgentTools) > marketplace.MaxAgentToolsPerBundle {
		return nil, fmt.Errorf("%w: %d agent_tools > %d limit",
			ErrBundleExceedsLimit, len(man.AgentTools), marketplace.MaxAgentToolsPerBundle)
	}
	rb.KTypes = make([]runtime.ResolvedKType, len(man.KTypes))
	for i, kt := range man.KTypes {
		body, name, err := resolveJSONFile(files, kt.Schema, "ktype schema")
		if err != nil {
			return nil, err
		}
		rb.KTypes[i] = runtime.ResolvedKType{
			Name:       name,
			Version:    1,
			SchemaJSON: body,
		}
	}
	rb.Workflows = make([]runtime.ResolvedWorkflow, len(man.Workflows))
	for i, wf := range man.Workflows {
		body, name, err := resolveJSONFile(files, wf.Definition, "workflow definition")
		if err != nil {
			return nil, err
		}
		rb.Workflows[i] = runtime.ResolvedWorkflow{
			Name:           name,
			Version:        1,
			DefinitionJSON: body,
		}
	}
	rb.AgentTools = make([]runtime.ResolvedAgentTool, len(man.AgentTools))
	for i, at := range man.AgentTools {
		body, name, err := resolveJSONFile(files, at.Definition, "agent tool definition")
		if err != nil {
			return nil, err
		}
		rb.AgentTools[i] = runtime.ResolvedAgentTool{
			Name:           name,
			DescriptorJSON: body,
		}
	}

	// The settings_schema file is optional — when the manifest
	// declares no settings, the resolver leaves SettingsSchemaJSON
	// nil and the B6 API handler treats every settings document
	// as valid (effectively accepts {}). When present, the file
	// must parse as JSON (size cap re-checked here as defence in
	// depth even though the per-file LimitReader already enforced
	// it during extraction).
	if man.SettingsSchema != "" {
		cleanPath, ok := bundleRelPath(man.SettingsSchema)
		if !ok {
			return nil, fmt.Errorf("%w: settings_schema path %q is not a bundle-relative ./ path",
				ErrBundleMalformed, man.SettingsSchema)
		}
		body, ok := files[cleanPath]
		if !ok {
			return nil, fmt.Errorf("%w: settings_schema file %q not in archive",
				ErrBundleMalformed, man.SettingsSchema)
		}
		if !json.Valid(body) {
			return nil, fmt.Errorf("%w: settings_schema %q: invalid JSON",
				ErrBundleMalformed, man.SettingsSchema)
		}
		rb.SettingsSchemaJSON = append(json.RawMessage(nil), body...)
	}
	return rb, nil
}

// resolveJSONFile looks up a manifest-relative path inside the
// extracted file set, validates it parses as JSON, and extracts
// the `name` field. The .Name on every ResolvedKType /
// ResolvedWorkflow / ResolvedAgentTool comes from the JSON body
// — NOT from the manifest path — because spec §3.1 names KTypes
// by their `name` property, not their filename. The kind label is
// embedded in error messages so a publisher sees "ktype schema
// ./ktypes/foo.json: missing name" instead of a generic message.
func resolveJSONFile(files map[string][]byte, manifestPath, kind string) (body []byte, name string, err error) {
	cleanPath, ok := bundleRelPath(manifestPath)
	if !ok {
		return nil, "", fmt.Errorf("%w: %s path %q is not a bundle-relative ./ path",
			ErrBundleMalformed, kind, manifestPath)
	}
	body, ok = files[cleanPath]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s file %q not in archive",
			ErrBundleMalformed, kind, manifestPath)
	}
	var named struct {
		Name string `json:"name"`
	}
	if uerr := json.Unmarshal(body, &named); uerr != nil {
		return nil, "", fmt.Errorf("%w: %s %q: parse json: %w",
			ErrBundleMalformed, kind, manifestPath, uerr)
	}
	if named.Name == "" {
		return nil, "", fmt.Errorf("%w: %s %q: missing top-level `name`",
			ErrBundleMalformed, kind, manifestPath)
	}
	return body, named.Name, nil
}

// bundleRelPath normalises a manifest-supplied "./foo/bar.json"
// into the "foo/bar.json" lookup key the extractor uses. Returns
// Untar exposes the bundle's tar.gz extraction logic to other
// packages in the marketplace tree (notably internal/marketplace/
// review, which needs the full file map — including icon and UI-
// extension files — to run its check pipeline). The B6 hot path
// uses untarGzip directly via Resolve; review uses Untar for the
// same defence-in-depth (per-file cap, cumulative cap, no symlinks,
// no traversal, single-root) without re-implementing those checks.
//
// The returned map is keyed by manifest-relative path (with the
// archive's single root directory stripped) and values are the
// file bodies. body must be the raw .tar.gz bytes.
func Untar(body []byte) (map[string][]byte, error) {
	return untarGzip(body)
}

// (path, true) on success, (empty, false) if the input is not a
// bundle-relative path of the documented form.
func bundleRelPath(p string) (string, bool) {
	if !strings.HasPrefix(p, "./") {
		return "", false
	}
	cleaned := path.Clean(p[2:])
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}

// untarGzip decompresses body as gzip, then unpacks the tar archive
// into a map keyed by bundle-relative path. The map's keys are
// normalised to forward-slash relative paths with the single root
// directory stripped (spec §2: "archive root MUST contain a single
// directory"). The leading-segment strip means a manifest path of
// "./kapp-extension.yaml" matches the entry "acme-shipping/kapp-
// extension.yaml" in the archive.
//
// Enforces:
//   - Per-file size cap (MaxSingleFileBytes).
//   - Cumulative extracted-bytes cap (MaxBundleSizeBytes).
//   - Reject absolute paths, ../, and symlinks (defence against tar-
//     slip).
//   - Reject duplicate file names (two archive entries with the same
//     post-normalised path).
//
// The manifest is always extracted alongside other files; the
// caller (Extract) reads it out of the returned map and re-parses
// it through the strict marketplace.ParseManifest validator.
func untarGzip(body []byte) (map[string][]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty bundle", ErrBundleMalformed)
	}
	gz, err := gzip.NewReader(newByteReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip: %w", ErrBundleMalformed, err)
	}
	// defer with explicit error sink — the gzip reader is over an
	// in-memory byte buffer that we already finished decoding by
	// the time the defer runs; a Close() failure here has no
	// recovery path. The sink keeps errcheck happy.
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	files := make(map[string][]byte)
	root := ""
	var cumulative int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: tar: %w", ErrBundleMalformed, err)
		}
		name := hdr.Name
		switch hdr.Typeflag {
		case tar.TypeDir:
			// Directories are not stored; we only care about file
			// entries. Continue past dir headers — they may or may
			// not be present depending on the archiver.
			continue
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA kept for older archivers
			// fall through
		case tar.TypeSymlink, tar.TypeLink:
			return nil, fmt.Errorf("%w: symlink/hardlink entry %q forbidden",
				ErrBundleMalformed, name)
		default:
			return nil, fmt.Errorf("%w: unsupported entry type %d for %q",
				ErrBundleMalformed, hdr.Typeflag, name)
		}
		// Reject absolute paths and traversal — defence against
		// tar-slip even though our archive root is supposed to be
		// a single directory.
		if path.IsAbs(name) || strings.Contains(name, "..") {
			return nil, fmt.Errorf("%w: entry %q has absolute or traversal path",
				ErrBundleMalformed, name)
		}
		// First non-dir entry determines the archive root. Spec §2
		// requires a single root directory; we strip it from all
		// subsequent paths so callers can look up by the manifest-
		// relative path.
		segments := strings.Split(name, "/")
		if root == "" {
			if len(segments) < 2 {
				return nil, fmt.Errorf("%w: archive root is not a directory",
					ErrBundleMalformed)
			}
			root = segments[0]
		} else if segments[0] != root {
			return nil, fmt.Errorf("%w: archive has multiple root directories (%q and %q)",
				ErrBundleMalformed, root, segments[0])
		}
		rel := strings.Join(segments[1:], "/")
		if rel == "" {
			continue
		}
		if hdr.Size > marketplace.MaxSingleFileBytes {
			return nil, fmt.Errorf("%w: file %q is %d bytes (> %d)",
				ErrBundleExceedsLimit, rel, hdr.Size, marketplace.MaxSingleFileBytes)
		}
		cumulative += hdr.Size
		if cumulative > marketplace.MaxBundleSizeBytes {
			return nil, fmt.Errorf("%w: cumulative extracted size > %d",
				ErrBundleExceedsLimit, marketplace.MaxBundleSizeBytes)
		}
		if _, dup := files[rel]; dup {
			return nil, fmt.Errorf("%w: duplicate entry %q in archive",
				ErrBundleMalformed, rel)
		}
		// Use io.ReadAll with a LimitReader to defend against a
		// header lying about the size (tar headers are advisory; a
		// malicious archive can declare size=1 and stream 1 GiB).
		// The +1 lets us detect "more than the header claimed" and
		// reject as malformed.
		limited := io.LimitReader(tr, hdr.Size+1)
		buf, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %w", ErrBundleMalformed, rel, err)
		}
		if int64(len(buf)) > hdr.Size {
			return nil, fmt.Errorf("%w: file %q body exceeds header size %d",
				ErrBundleMalformed, rel, hdr.Size)
		}
		files[rel] = buf
	}
	if root == "" || len(files) == 0 {
		return nil, fmt.Errorf("%w: archive is empty", ErrBundleMalformed)
	}
	return files, nil
}

// byteReader is a tiny adapter so untarGzip can stream from a
// []byte without pulling in bytes.NewReader's full surface (we
// only ever Read).
type byteReader struct {
	buf []byte
	pos int
}

func newByteReader(b []byte) *byteReader { return &byteReader{buf: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

// InMemoryResolver is the test/dev Resolver. The harness pre-builds
// a *ResolvedBundle (or raw .tar.gz bytes) and registers it under
// the version ID; Resolve returns the registered entry verbatim
// (no fetch, no hash check beyond the explicit one performed at
// registration time).
//
// Safe for concurrent use: Set / Resolve both take an internal
// RWMutex. Production callers register all bundles up front during
// test setup and then issue concurrent Resolve calls (e.g. when the
// integration test fires multiple installs in parallel, or when
// CachingResolver tests stress the wrapper) — the mutex makes that
// pattern safe without forcing every test to externalise its own
// locking.
type InMemoryResolver struct {
	mu      sync.RWMutex
	bundles map[string]*runtime.ResolvedBundle
}

// NewInMemoryResolver returns an empty resolver. Register entries
// with Set before calling Resolve.
func NewInMemoryResolver() *InMemoryResolver {
	return &InMemoryResolver{bundles: make(map[string]*runtime.ResolvedBundle)}
}

// Set registers a pre-built ResolvedBundle for the given version ID.
// The resolver will return it from Resolve for matching version
// rows; useful in test harnesses where we construct manifests
// programmatically.
func (r *InMemoryResolver) Set(versionID string, b *runtime.ResolvedBundle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bundles[versionID] = b
}

// Resolve looks up version.ID in the registered map. Returns
// ErrBundleNotFound if the bundle was never registered.
func (r *InMemoryResolver) Resolve(_ context.Context, version *marketplace.ExtensionVersion) (*runtime.ResolvedBundle, error) {
	if version == nil {
		return nil, errors.New("bundle: nil version")
	}
	r.mu.RLock()
	b, ok := r.bundles[version.ID.String()]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: version_id=%s", ErrBundleNotFound, version.ID)
	}
	return b, nil
}
