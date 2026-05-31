package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundle"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundlestore"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Phase B8 — publisher bundle uploads + marketplace-hosted serving.
//
// B6 (#130) shipped the publish surface as "publisher hosts the
// bundle on its own CDN; POST a JSON body with bundle_url +
// bundle_hash + bundle_size and we'll fetch from there at install
// time." That requires the publisher to operate a CDN, which is a
// hard prerequisite for the kapp-publish CLI tool we ship in this
// phase — the publisher experience cannot require "first, go set up
// a CDN somewhere with HTTPS and a stable URL."
//
// This file adds:
//
//   * POST /api/v1/publisher/{publisher_id}/bundles
//       Multipart upload of a tar.gz. We hash + extract + validate
//       the manifest, store the bytes in bundlestore.Store, and
//       return {bundle_url, bundle_hash, bundle_size}. The
//       publisher then passes those fields to
//       POST /api/v1/marketplace/publisher/extensions/{ext_id}/versions
//       (or the equivalent CLI flow) to publish a version.
//
//   * GET  /api/v1/marketplace/bundles/{hash}.tar.gz
//       Public-within-tenant streaming GET that the install-time
//       HTTPResolver fetches when a marketplace-hosted bundle_url
//       is recorded on a version row. Content-addressed so the
//       URL is immutable and Cache-Control: immutable.
//
// Authorisation: the upload endpoint requires membership on the
// publisher_id (member role suffices — any publisher member can
// upload bundles; only owners can manage members + keys, but key
// rotation and bundle uploads are member-level operations).
//
// Size cap: bundles larger than marketplace.MaxBundleSizeBytes
// (10 MiB) are rejected with 413. The handler enforces the cap on
// the streamed body via http.MaxBytesReader so a hostile client
// cannot exhaust memory by streaming a 10 GiB body before we read
// the size header.

// uploadBundleResponse is the JSON shape returned from the upload
// endpoint. BundleURL is a marketplace-hosted URL the publisher
// passes to PublishVersion; BundleHash + BundleSize feed the same
// PublishVersion fields. ExpiresAt is the GC deadline for an
// upload that has NOT yet been referenced by a PublishVersion
// call; once referenced the row is immortal and the field is
// omitted entirely (nil pointer + omitempty).
//
// Round-7 Devin Review
// BUG_pr-review-job-2430454d8f6e45f2bac501c46cdcab2a_0001
// flagged that this field used to be a non-pointer time.Time
// always computed as upload.CreatedAt + OrphanRetention. For a
// content-addressed dedup hit on a row that was ALREADY referenced
// by an earlier PublishVersion call, Upload returns the original
// row (bundlestore/store.go:561) — so the response would surface
// an ExpiresAt potentially weeks in the past, falsely implying
// the bundle was about to be GC'd. The list endpoint already
// used *time.Time + expiresAtOrNil correctly; this brings the
// upload response onto the same shape so the publisher console
// and dashboard agree about the same underlying row.
type uploadBundleResponse struct {
	UploadID    uuid.UUID  `json:"upload_id"`
	BundleURL   string     `json:"bundle_url"`
	BundleHash  string     `json:"bundle_hash"`
	BundleSize  int64      `json:"bundle_size"`
	ContentType string     `json:"content_type"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	// ManifestPreview lets the publisher console show
	// "this bundle declares X ktypes / Y workflows / Z tools"
	// without a second extract — the upload handler already did
	// the work to validate, so we surface the cheap summary.
	ManifestPreview manifestPreview `json:"manifest_preview"`
}

// manifestPreview is the bundle-summary subset of the parsed
// manifest. Surfaced from the upload endpoint so the publisher
// can sanity-check what they uploaded before calling PublishVersion.
type manifestPreview struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	Publisher       string `json:"publisher"`
	Description     string `json:"description,omitempty"`
	KTypesCount     int    `json:"ktypes_count"`
	WorkflowsCount  int    `json:"workflows_count"`
	AgentToolsCount int    `json:"agent_tools_count"`
	WebhooksCount   int    `json:"webhooks_count"`
	UIExtCount      int    `json:"ui_extensions_count"`
}

// uploadPublisherBundle is the POST handler for
// /api/v1/publisher/{publisher_id}/bundles.
//
// Body shape:
//   - multipart/form-data with one file field named "bundle".
//     content-type SHOULD be application/gzip (the handler
//     accepts application/octet-stream and application/x-gzip
//     as historical aliases the CLI may send).
//
// Authz: caller MUST be a member (any role) of publisher_id.
//
// On success: 201 Created + uploadBundleResponse. The
// publisher's next call is POST /api/v1/marketplace/publisher/
// extensions/{ext_id}/versions with the returned bundle_url /
// bundle_hash / bundle_size.
func (h *marketplaceHandlers) uploadPublisherBundle(w http.ResponseWriter, r *http.Request) {
	if h.bundles == nil || h.bundleURLBase == "" {
		http.Error(w, "marketplace-hosted bundle uploads are not enabled on this deployment", http.StatusServiceUnavailable)
		return
	}
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

	// Cap the body before parsing the multipart form. The +4 KiB
	// slack accounts for multipart boundary / form metadata
	// overhead so a bundle right at MaxBundleSizeBytes still
	// fits. http.MaxBytesReader returns a *MaxBytesError on
	// overflow which we map to 413.
	const multipartOverhead = 4 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, marketplace.MaxBundleSizeBytes+multipartOverhead)
	if err := r.ParseMultipartForm(marketplace.MaxBundleSizeBytes + multipartOverhead); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "bundle exceeds 10 MiB cap", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("invalid multipart body: %v", err), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("bundle")
	if err != nil {
		http.Error(w, "missing 'bundle' form field (multipart file required)", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	contentType := header.Header.Get("Content-Type")
	// Normalise common aliases the CLI / curl may send.
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "application/gzip", "application/x-gzip", "application/octet-stream", "":
		contentType = bundlestore.DefaultContentType
	default:
		http.Error(w, fmt.Sprintf("content-type %q not accepted (only application/gzip)", contentType),
			http.StatusBadRequest)
		return
	}

	// Read into memory under the same cap. We need the full bytes
	// to hash + validate + store atomically. The cap is enforced
	// twice: by MaxBytesReader above (raw body) and by the
	// explicit length check here (defence in depth in case the
	// header lies).
	limited := io.LimitReader(file, marketplace.MaxBundleSizeBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "bundle exceeds 10 MiB cap", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("read bundle body: %v", err), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > marketplace.MaxBundleSizeBytes {
		http.Error(w, "bundle exceeds 10 MiB cap", http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty bundle body", http.StatusBadRequest)
		return
	}

	// Extract + validate the bundle before storing. Doing this
	// here (rather than at PublishVersion time) gives the
	// publisher immediate feedback on a malformed bundle and
	// avoids storing bytes that can never be published.
	rb, err := bundle.Extract(body)
	if err != nil {
		// bundle.Extract returns ErrBundleMalformed,
		// ErrBundleExceedsLimit, etc. — map via h.writeError.
		h.writeError(w, err)
		return
	}

	// Enforce publisher-slug consistency: the manifest declares a
	// publisher slug; that MUST match the publisher_id we
	// authorised against. Otherwise a member of publisher A
	// could upload bundles whose manifest claims publisher B,
	// then attempt to publish under B via a separate compromised
	// account. Rejecting at upload time is defence-in-depth on
	// top of the PublishVersion gate (which we add separately).
	//
	// EqualFold mirrors the version-submit (publish) handler and
	// the dashboard's requirePublisherOwnsExtension — keeping all
	// three slug-equality gates on the same comparator. publisher
	// slugs are regex-restricted to `^[a-z][a-z0-9_]{2,31}$`
	// (lowercase only, see internal/marketplace/manifest.go) so
	// the two callers cannot differ in practice today; matching
	// case-insensitively is defence-in-depth against a future
	// loosening of the slug constraint.
	// Publisher slug MUST be populated. ParseManifest derives it
	// from the manifest's `name` field via SplitN(".", 2) and the
	// `name` field is required + regex-enforced, so an empty
	// Publisher after a successful Extract is impossible TODAY.
	// We still reject explicitly rather than letting an empty
	// Publisher bypass the slug-equality gate via the previous
	// `Publisher != ""` short-circuit (Devin Review
	// ANALYSIS_pr-review-job-6c5aa7fef9214efaacd238cc9ba21472_0002).
	// Defence-in-depth against a future refactor that weakens the
	// derivation invariant.
	if rb.Manifest == nil {
		http.Error(w, "extracted bundle has no manifest", http.StatusBadRequest)
		return
	}
	if rb.Manifest.Publisher == "" {
		http.Error(w, "manifest publisher segment is empty (manifest name must be `<publisher>.<slug>`)",
			http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(rb.Manifest.Publisher, pub.Slug) {
		http.Error(w, fmt.Sprintf(
			"manifest publisher %q does not match upload publisher %q",
			rb.Manifest.Publisher, pub.Slug,
		), http.StatusBadRequest)
		return
	}

	upload, err := h.bundles.Upload(r.Context(), bundlestore.UploadInput{
		Bytes:       body,
		PublisherID: pubID,
		UploadedBy:  userID,
		ContentType: contentType,
	})
	if err != nil {
		if errors.Is(err, bundlestore.ErrBundleTooLarge) {
			http.Error(w, "bundle exceeds 10 MiB cap", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("store bundle: %v", err), http.StatusInternalServerError)
		return
	}

	resp := uploadBundleResponse{
		UploadID:    upload.ID,
		BundleURL:   marketplaceBundleURL(h.bundleURLBase, upload.ContentHash),
		BundleHash:  upload.ContentHash,
		BundleSize:  upload.SizeBytes,
		ContentType: upload.ContentType,
		ExpiresAt:   expiresAtOrNil(upload.CreatedAt, upload.ReferencedAt),
		ManifestPreview: manifestPreview{
			Name:            rb.Manifest.Name,
			Version:         rb.Manifest.Version,
			Publisher:       rb.Manifest.Publisher,
			Description:     rb.Manifest.Description,
			KTypesCount:     len(rb.Manifest.KTypes),
			WorkflowsCount:  len(rb.Manifest.Workflows),
			AgentToolsCount: len(rb.Manifest.AgentTools),
			WebhooksCount:   len(rb.Manifest.WebhooksConsumed),
			UIExtCount:      len(rb.Manifest.UIExtensions),
		},
	}
	writeJSON(w, http.StatusCreated, resp)
}

// serveBundleByHash is the public-within-tenant GET handler for
// /api/v1/marketplace/bundles/{hash}.tar.gz.
//
// Returns the raw tar.gz bytes for a marketplace-hosted bundle.
// The bundle is content-addressed so the URL is immutable and
// served with Cache-Control: public, immutable. Missing hashes
// return 404. Hashes that don't parse return 400 (defence-in-depth
// against a malformed URL component).
//
// This is the path the install-time HTTPResolver fetches when a
// version row's bundle_url points back at the marketplace itself.
// It's symmetric with any publisher-hosted CDN: same GET-then-
// SHA-256-verify pipeline.
func (h *marketplaceHandlers) serveBundleByHash(w http.ResponseWriter, r *http.Request) {
	if h.bundles == nil {
		http.Error(w, "marketplace-hosted bundles are not enabled", http.StatusNotFound)
		return
	}
	hashParam := chi.URLParam(r, "hash")
	// Strip the optional .tar.gz suffix so the URL the CLI
	// generates ("…/abc1234.tar.gz") parses the same as the
	// raw-hash form the resolver may construct internally.
	hash := strings.TrimSuffix(hashParam, ".tar.gz")
	if !marketplace.IsValidBundleHash(hash) {
		http.Error(w, "invalid bundle hash (expected lowercase hex SHA-256, 64 chars)",
			http.StatusBadRequest)
		return
	}
	upload, rc, err := h.bundles.Fetch(r.Context(), hash)
	if err != nil {
		if errors.Is(err, bundlestore.ErrBundleNotFound) {
			http.Error(w, "bundle not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("fetch bundle: %v", err), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", upload.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", upload.SizeBytes))
	w.Header().Set("ETag", `"`+upload.ContentHash+`"`)
	// Content is content-addressed (hash is in the URL) → safe to
	// mark immutable. Any future B8.x scheme that swaps bytes
	// under a hash would be a SHA-256 collision, not a real
	// invalidation.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Connection already half-streamed; nothing useful to log
		// beyond what the access log captures. Swallow.
		return
	}
}

// marketplaceBundleURL builds the externally-visible URL for a
// marketplace-hosted bundle. base is the trimmed host prefix; hash
// is the SHA-256 hex.
//
// The /api/v1/marketplace/bundles/<hash>.tar.gz path is on the
// tenant-chain route group (mounts under tenantChain) — i.e. an
// authenticated tenant user can fetch it. That matches the rest of
// the marketplace surface: the catalog browse endpoints are also
// anonymous-within-tenant. The bundle resolver attaches whatever
// auth the install-time admin user has, so the install pipeline
// fetches with a valid session.
func marketplaceBundleURL(base, hash string) string {
	return base + "/api/v1/marketplace/bundles/" + hash + ".tar.gz"
}

// listMyPublisherBundleUploads returns the upload history for a
// publisher (caller MUST be a member). Used by the publisher
// dashboard's "uploaded bundles" view so the publisher can find a
// hash they uploaded earlier without re-uploading the file.
func (h *marketplaceHandlers) listMyPublisherBundleUploads(w http.ResponseWriter, r *http.Request) {
	if h.bundles == nil {
		http.Error(w, "marketplace-hosted bundle uploads are not enabled on this deployment", http.StatusServiceUnavailable)
		return
	}
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
	rows, err := h.bundles.ListPublisherUploads(r.Context(), pubID, 0)
	if err != nil {
		http.Error(w, fmt.Sprintf("list uploads: %v", err), http.StatusInternalServerError)
		return
	}
	out := make([]uploadBundleListItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, uploadBundleListItem{
			UploadID:     row.ID,
			BundleURL:    marketplaceBundleURL(h.bundleURLBase, row.ContentHash),
			BundleHash:   row.ContentHash,
			BundleSize:   row.SizeBytes,
			ContentType:  row.ContentType,
			CreatedAt:    row.CreatedAt,
			ReferencedAt: row.ReferencedAt,
			ExpiresAt:    expiresAtOrNil(row.CreatedAt, row.ReferencedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// uploadBundleListItem is the per-row shape used by the bundle
// history list. Distinct from uploadBundleResponse because the
// list view does not re-extract the manifest (avoid re-fetching
// every body just to render the dashboard).
type uploadBundleListItem struct {
	UploadID     uuid.UUID  `json:"upload_id"`
	BundleURL    string     `json:"bundle_url"`
	BundleHash   string     `json:"bundle_hash"`
	BundleSize   int64      `json:"bundle_size"`
	ContentType  string     `json:"content_type"`
	CreatedAt    time.Time  `json:"created_at"`
	ReferencedAt *time.Time `json:"referenced_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// expiresAtOrNil returns the GC deadline for an unreferenced upload,
// or nil if the row is already referenced (and thus immortal).
func expiresAtOrNil(createdAt time.Time, referencedAt *time.Time) *time.Time {
	if referencedAt != nil {
		return nil
	}
	t := createdAt.Add(bundlestore.OrphanRetention)
	return &t
}

// writeJSON-style helper used only inside this file for the
// ad-hoc 503 envelope. Production deploys with the bundle store
// disabled (h.bundles == nil) should never reach the upload
// endpoints because the routes don't mount, but the handler-level
// guard above provides belt-and-braces.
var _ = json.Marshal // keep import alive if all callsites move
