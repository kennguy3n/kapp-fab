package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundlestore"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
)

// Phase B8 — marketplace-hosted bundle resolver shim.
//
// When a publisher uploads a bundle via POST
// /api/v1/publisher/{publisher_id}/bundles, the upload endpoint
// records the bundle bytes in bundlestore.Store and hands the
// publisher back a marketplace-hosted bundle_url of the form
//   {KAPP_MARKETPLACE_BUNDLE_URL_BASE}/api/v1/marketplace/bundles/{hash}.tar.gz
// which the publisher then passes to PublishVersion. At install
// time the engine asks the configured Resolver to fetch that URL.
//
// Going through the HTTPResolver to fetch from ourselves is
// wasteful (a localhost loopback through the LB) and breaks in
// dev (HTTPResolver enforces https://). LocalResolver fixes both
// problems: if the bundle_url's prefix matches the marketplace
// URL base, serve the bytes directly from bundlestore.Store and
// skip the HTTP round trip entirely. Otherwise delegate to the
// wrapped resolver (HTTPResolver in production, anything in tests).
//
// SHA-256 verification: the wrapped resolver's downstream caller
// (engine.Install / engine.Upgrade) verifies the bytes against
// the version row's bundle_hash. LocalResolver does NOT skip
// that step — it just supplies the bytes from a local source.
// The hash check is still the source of truth.
//
// LocalResolver also accepts the `{base}/api/v1/marketplace/bundles/{hash}`
// form without the .tar.gz suffix (the suffix is decorative — the
// upload endpoint generates URLs with it; older callers may have
// stored the bare-hash form). This matches the server-side
// chi.URLParam(r, "hash") which strips .tar.gz before lookup.

// LocalResolver short-circuits marketplace-hosted bundle_urls so
// the install pipeline reads bytes from the in-process
// bundlestore.Store instead of a self-loopback HTTP fetch. Wraps
// a delegate Resolver for non-marketplace URLs (publisher CDNs,
// the in-memory test resolver, etc.).
type LocalResolver struct {
	delegate Resolver
	store    bundleByteSource
	urlBase  string
}

// bundleByteSource is the narrow surface LocalResolver needs out
// of bundlestore.Store: pull a row by its content_hash and a
// reader over its bytes. Defined as a local interface so tests
// can swap in a fake without dragging in pgxpool.
type bundleByteSource interface {
	Fetch(ctx context.Context, hash string) (*bundlestore.BundleUpload, io.ReadCloser, error)
}

// NewLocalResolver returns a LocalResolver that delegates to
// `delegate` for anything not matching `urlBase`. If `store` is
// nil or `urlBase` is empty the LocalResolver is effectively the
// delegate (marketplace-hosted serving disabled — every fetch
// goes through the delegate).
func NewLocalResolver(delegate Resolver, store bundleByteSource, urlBase string) *LocalResolver {
	return &LocalResolver{
		delegate: delegate,
		store:    store,
		urlBase:  strings.TrimRight(urlBase, "/"),
	}
}

// Resolve returns the parsed bundle. For marketplace-hosted
// bundles the bytes are pulled directly from bundlestore.Store;
// for any other URL the wrapped resolver handles the fetch.
//
// The signature matches Resolver so callers can swap implementations
// without changing wiring.
func (l *LocalResolver) Resolve(ctx context.Context, version *marketplace.ExtensionVersion) (*runtime.ResolvedBundle, error) {
	if version == nil {
		return nil, fmt.Errorf("local resolver: nil version")
	}
	hash, ok := l.matchMarketplaceHash(version.BundleURL)
	if !ok || l.store == nil {
		if l.delegate == nil {
			return nil, fmt.Errorf("local resolver: no delegate and url %q is not marketplace-hosted", version.BundleURL)
		}
		return l.delegate.Resolve(ctx, version)
	}

	// SHA-256 consistency check: a malformed publish call could
	// have stored a marketplace URL whose hash component disagrees
	// with the version row's bundle_hash. Reject before reading
	// the bytes — the engine will reject again after Extract, but
	// catching it here gives a clearer error path and avoids
	// allocating the bytes only to discard them.
	if !equalHashFold(hash, version.BundleHash) {
		return nil, fmt.Errorf("%w: url hash %q != version bundle_hash %q",
			marketplace.ErrBundleHashMismatch, hash, version.BundleHash)
	}

	_, rc, err := l.store.Fetch(ctx, hash)
	if err != nil {
		if errors.Is(err, bundlestore.ErrBundleNotFound) {
			return nil, ErrBundleNotFound
		}
		return nil, fmt.Errorf("%w: local store: %w", ErrBundleFetchFailed, err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(io.LimitReader(rc, marketplace.MaxBundleSizeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read local bytes: %w", ErrBundleFetchFailed, err)
	}
	if int64(len(body)) > marketplace.MaxBundleSizeBytes {
		return nil, fmt.Errorf("%w: bundle exceeds %d bytes",
			marketplace.ErrBundleTooLarge, marketplace.MaxBundleSizeBytes)
	}

	// Hash-verify the body before extraction. Same shape as the
	// HTTPResolver Resolve path — the engine relies on this
	// before in-tx registration. We already checked the URL hash
	// segment matches the version row above, but verifying the
	// bytes themselves closes the case where the store row's
	// content_hash drifted from the bytes (impossible today given
	// the upload pipeline, but the resolver doesn't trust upstream
	// catalogues — that's the point of the verify).
	gotHash := sha256.Sum256(body)
	gotHex := hex.EncodeToString(gotHash[:])
	if !equalHashFold(gotHex, version.BundleHash) {
		return nil, fmt.Errorf("%w: expected %s, got %s",
			marketplace.ErrBundleHashMismatch, version.BundleHash, gotHex)
	}
	return Extract(body)
}

// matchMarketplaceHash returns the {hash} segment if `bundleURL`
// is rooted at the marketplace bundle URL base, otherwise
// ("", false). Accepts both "{base}/api/v1/marketplace/bundles/{hash}.tar.gz"
// and "{base}/api/v1/marketplace/bundles/{hash}" forms.
func (l *LocalResolver) matchMarketplaceHash(bundleURL string) (string, bool) {
	if l.urlBase == "" {
		return "", false
	}
	prefix := l.urlBase + "/api/v1/marketplace/bundles/"
	if !strings.HasPrefix(bundleURL, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(bundleURL, prefix)
	// Strip any query string or fragment (defence-in-depth — the
	// upload endpoint never emits these but a tampered version row
	// could).
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSuffix(rest, ".tar.gz")
	if !marketplace.IsValidBundleHash(rest) {
		return "", false
	}
	return rest, true
}
