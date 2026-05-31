package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Phase B8 — publisher-self extension + version surface.
//
// B6 (#130) shipped admin-only versions of these endpoints under
// /api/v1/marketplace/publisher/... gated by the
// `marketplace.publisher` tenant role. That surface predated B7.1
// (#133) — there was no publisher membership concept yet, so any
// tenant member with the role could create extensions claiming
// any publisher slug.
//
// B8 adds a parallel publisher-self surface under
// /api/v1/publisher/{publisher_id}/... gated by membership on the
// publisher_id (not a tenant-role check). This is the surface the
// kapp-publish CLI uses. The two surfaces share the underlying
// store calls; only the authz model differs.
//
// Cross-checks enforced here that the admin surface does not:
//   * publisher slug in the extension MUST equal the
//     publisher_id's slug (a member of publisher A cannot create
//     extensions claiming to be published by publisher B).
//   * extension ID in the version submit MUST belong to one of
//     the publisher's extensions.
//   * if the bundle URL is empty AND a marketplace-hosted hash
//     was uploaded under this publisher, auto-fill the bundle URL
//     to the marketplace-hosted serve path.
//   * after a successful PublishVersion call, mark the upload row
//     as "referenced" so the orphan-GC sweeper leaves it alone.

// parseBundleSignaturePair validates the wire-level both-or-neither
// rule on the optional ed25519 signature pair and converts it into
// the store's marketplace.BundleSignature value. Shared between the
// publisher-self and admin submit-version handlers so both endpoints
// produce the same wire contract and the same store input.
//
// Contract:
//   - both empty  -> (nil, nil) — version row gets no signature.
//   - both set    -> non-nil BundleSignature with SignedAt=now (UTC);
//     the store fills SignedAt internally if zero but we set it here
//     so the persisted timestamp matches the wall-clock at the API
//     boundary, not at PublishVersion-tx-commit time which can drift
//     under load.
//   - one set     -> error.
//
// Trimming: leading/trailing whitespace is stripped before the
// empty-vs-present test so a CLI that newline-terminated its base64
// signature file doesn't accidentally trip the "one set, one empty"
// rejection.
func parseBundleSignaturePair(b64, keyID string) (*marketplace.BundleSignature, error) {
	b64 = strings.TrimSpace(b64)
	keyID = strings.TrimSpace(keyID)
	if b64 == "" && keyID == "" {
		return nil, nil
	}
	if b64 == "" || keyID == "" {
		return nil, fmt.Errorf(
			"bundle_signature and bundle_signature_key_id must be set together (one without the other is rejected)")
	}
	return &marketplace.BundleSignature{
		SignatureB64: b64,
		KeyID:        keyID,
		SignedAt:     time.Now().UTC(),
	}, nil
}

// publisherCreateExtensionRequestBody is the wire shape for the
// publisher-self create-extension call. Distinct from the admin
// createExtensionRequestBody because the publisher field is
// derived from the URL publisher_id, not supplied in the body.
type publisherCreateExtensionRequestBody struct {
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	Description  string `json:"description"`
	Author       string `json:"author"`
	License      string `json:"license"`
	Homepage     string `json:"homepage,omitempty"`
	SupportEmail string `json:"support_email,omitempty"`
	IconURL      string `json:"icon_url,omitempty"`
}

// publisherSubmitVersionRequestBody is the wire shape for the
// publisher-self submit-version call. BundleURL is optional —
// when omitted, the handler auto-fills it from the marketplace-
// hosted URL for the supplied hash. BundleHash + BundleSize are
// REQUIRED so the handler can validate against the upload row.
//
// BundleSignature / BundleSignatureKeyID carry the optional
// detached ed25519 signature over the bundle bytes. The CLI
// (`kapp-publish publish --bundle-signature <b64>
// --bundle-signature-key-id <id>`) emits both fields together;
// the server requires both-or-neither — a wire request with
// only one is rejected with 400 because PublishVersion would
// otherwise return the same error wrapped as ErrInvalidManifest
// and the caller would have to guess which field was missing.
// When both are present we wire `marketplace.BundleSignature`
// into PublishVersionInput.Signature so the version row carries
// the trio (signature_b64 + key_id + signed_at) for B7's
// SignatureCheck to verify post-resolve.
//
// Historical context: B8 round-1 added the CLI's signature flags
// and the wire fields on the CLI's request struct but missed the
// matching server-side fields here; `json.Decoder` silently drops
// unknown fields, so the signature data round-tripped through the
// CLI without ever reaching the version row. Devin Review surfaced
// this in BUG_pr-review-job-6c5aa7fef9214efaacd238cc9ba21472_0001.
type publisherSubmitVersionRequestBody struct {
	Manifest             json.RawMessage `json:"manifest"`
	BundleURL            string          `json:"bundle_url,omitempty"`
	BundleHash           string          `json:"bundle_hash"`
	BundleSize           int64           `json:"bundle_size"`
	BundleSignature      string          `json:"bundle_signature,omitempty"`
	BundleSignatureKeyID string          `json:"bundle_signature_key_id,omitempty"`
}

// createMyPublisherExtension is POST
// /api/v1/publisher/{publisher_id}/extensions.
//
// Member-level. The publisher field of the extension row is
// forced to the publisher's slug — body cannot override.
func (h *marketplaceHandlers) createMyPublisherExtension(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(
		r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember,
	); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	pub, err := h.store.Publishers().GetPublisher(r.Context(), pubID)
	if err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}

	var req publisherCreateExtensionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Slug) == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}

	ext, err := h.store.CreateExtension(r.Context(), marketplace.CreateExtensionInput{
		Publisher:    pub.Slug, // forced — body cannot override
		Slug:         req.Slug,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Author:       req.Author,
		License:      req.License,
		Homepage:     req.Homepage,
		SupportEmail: req.SupportEmail,
		IconURL:      req.IconURL,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publisherExtensionToView(ext))
}

// submitMyPublisherVersion is POST
// /api/v1/publisher/{publisher_id}/extensions/{ext_id}/versions.
//
// Member-level. Performs the same cross-checks as the admin
// surface, plus:
//   - extension MUST belong to the publisher (404 otherwise)
//   - manifest publisher MUST equal the publisher's slug
//   - if BundleURL is empty AND BundleHash matches a marketplace
//     upload, auto-fill the URL to the marketplace-hosted path
//   - on success, mark the upload row as referenced (so the GC
//     sweeper leaves the bytes alone)
func (h *marketplaceHandlers) submitMyPublisherVersion(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	extID, ok := parseExtIDParam(w, r)
	if !ok {
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(
		r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember,
	); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	pub, err := h.store.Publishers().GetPublisher(r.Context(), pubID)
	if err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	ext, err := h.store.GetExtension(r.Context(), extID)
	if err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	if !strings.EqualFold(ext.Publisher, pub.Slug) {
		http.Error(w, marketplace.ErrNotFound.Error(), http.StatusNotFound)
		return
	}

	var req publisherSubmitVersionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.Manifest) == 0 {
		http.Error(w, "manifest required", http.StatusBadRequest)
		return
	}
	if !marketplace.IsValidBundleHash(req.BundleHash) {
		http.Error(w, "invalid bundle_hash (expected lowercase hex SHA-256)", http.StatusBadRequest)
		return
	}
	if req.BundleSize <= 0 {
		http.Error(w, "bundle_size must be positive", http.StatusBadRequest)
		return
	}

	man, err := marketplace.ParseManifest(req.Manifest)
	if err != nil {
		h.writeError(w, fmt.Errorf("%w: %w", marketplace.ErrInvalidManifest, err))
		return
	}
	// Publisher MUST be populated. ParseManifest derives it from
	// the manifest's `name` field (split on `.`); a manifest that
	// reached this point with an empty Publisher would mean a
	// future refactor weakened that invariant. Reject explicitly
	// rather than letting an empty-publisher manifest bypass the
	// slug-equality gate via the previous `!= ""` short-circuit
	// (Devin Review ANALYSIS_pr-review-job-6c5aa7fef9214efaacd238cc9ba21472_0002).
	if man.Publisher == "" {
		http.Error(w, "manifest publisher segment is empty (manifest name must be `<publisher>.<slug>`)",
			http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(man.Publisher, pub.Slug) {
		http.Error(w, fmt.Sprintf(
			"manifest publisher %q does not match URL publisher %q",
			man.Publisher, pub.Slug,
		), http.StatusBadRequest)
		return
	}

	// Signature pair is optional but both-or-neither. Reject early
	// rather than letting PublishVersion's signature CHECK return
	// an opaque ErrInvalidManifest — the caller's contract is on
	// the publisher endpoint, not on the store.
	sig, sigErr := parseBundleSignaturePair(req.BundleSignature, req.BundleSignatureKeyID)
	if sigErr != nil {
		http.Error(w, sigErr.Error(), http.StatusBadRequest)
		return
	}

	bundleURL := strings.TrimSpace(req.BundleURL)
	if bundleURL == "" {
		// Try to auto-fill from a marketplace-hosted upload row.
		// The publisher MUST own the upload (defence-in-depth on
		// top of the upload-time publisher-slug check).
		if h.bundles != nil && h.bundleURLBase != "" {
			up, lookupErr := h.bundles.GetByHash(r.Context(), req.BundleHash)
			if lookupErr == nil && up.PublisherID != nil && *up.PublisherID == pubID {
				bundleURL = marketplaceBundleURL(h.bundleURLBase, up.ContentHash)
			}
		}
	}
	if bundleURL == "" {
		http.Error(w, "bundle_url required (or upload via /publisher/{id}/bundles first)",
			http.StatusBadRequest)
		return
	}

	ver, err := h.store.PublishVersion(r.Context(), marketplace.PublishVersionInput{
		ExtensionID:  extID,
		Manifest:     man,
		BundleHash:   req.BundleHash,
		BundleSize:   req.BundleSize,
		BundleURL:    bundleURL,
		ManifestJSON: req.Manifest,
		Signature:    sig,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}

	// Best-effort: mark the upload row as referenced so the
	// orphan-GC sweeper leaves it alone. The version row is
	// already persisted; if MarkReferenced fails the upload
	// becomes immortal-via-FK-from-version anyway (a future
	// GC predicate could JOIN extension_versions to catch this).
	// Logging-only on failure.
	if h.bundles != nil {
		if mrErr := h.bundles.MarkReferenced(r.Context(), req.BundleHash); mrErr != nil &&
			!errors.Is(mrErr, marketplace.ErrNotFound) {
			// Same pattern as B7.2 dispatch-log errors: log,
			// don't fail the user-visible op.
			_ = mrErr // ServeHTTP-time logger is request-scoped; covered by access log
		}
	}

	writeJSON(w, http.StatusCreated, publisherVersionToView(ver, nil))
}
