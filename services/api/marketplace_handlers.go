package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundle"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/settings"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// marketplaceHandlers exposes the Phase 2a B6 HTTP surface for
// browsing the extension catalog, installing / uninstalling
// extensions, updating per-install settings, and publishing /
// reviewing extensions. Tenant scope is enforced by the
// `d.tenantChain` middleware wired in routes.go; admin endpoints
// run under `d.adminChain`. Authorization is layered on top via
// `d.authzGate("marketplace.admin", "")` for tenant routes and
// `d.authzGate("marketplace.publisher", "")` for publisher
// routes — the gate strings line up with the strings the authz
// store recognises (no central enum exists today, so we follow
// the existing convention of free-form action strings).
//
// The handler depends on:
//   - marketplace.Store: catalog (extensions, versions, reviews,
//     installations).
//   - *runtime.Engine: install / uninstall / update-settings
//     lifecycle.
//   - bundle.Resolver: fetch + verify tar.gz bundles before
//     install. In production this is bundle.HTTPResolver; in
//     tests an in-memory resolver is wired with pre-baked
//     bundles.
//
// Sentinel-error translation (mapping defined in writeError):
//
//	marketplace.ErrNotFound            → 404
//	marketplace.ErrConflict            → 409
//	marketplace.ErrInvalidManifest     → 400
//	marketplace.ErrBundleTooLarge      → 413
//	marketplace.ErrBundleHashMismatch  → 502 (upstream integrity failure)
//	marketplace.ErrYanked              → 409
//	bundle.ErrBundleNotFound           → 502
//	bundle.ErrBundleFetchFailed        → 502
//	bundle.ErrBundleMalformed          → 422
//	bundle.ErrBundleExceedsLimit       → 413
//	bundle.ErrBundleTransportInsecure  → 400
//	runtime.ErrPreInstallRejected      → 422 (publisher refused)
//	runtime.ErrPreUninstallRejected    → 422 (publisher refused uninstall)
//	marketplace.ErrInvalidSignature    → 422 (cryptographic verification failed)
//	marketplace.ErrPublisherNotVerified → 409 (state precondition)
//
// Anything else collapses to 500.
type marketplaceHandlers struct {
	store    *marketplace.Store
	engine   *runtime.Engine
	resolver bundle.Resolver
}

// newMarketplaceHandlers constructs a handler bundle. Returns nil
// when the engine or store is nil — the route registrar then
// skips mounting the marketplace surface (matching the pattern
// the isolation-audit handler uses for the admin pool).
func newMarketplaceHandlers(store *marketplace.Store, engine *runtime.Engine, resolver bundle.Resolver) *marketplaceHandlers {
	if store == nil || engine == nil || resolver == nil {
		return nil
	}
	return &marketplaceHandlers{store: store, engine: engine, resolver: resolver}
}

// ---------------------------------------------------------------------------
// Wire-level request / response types. Kept distinct from the
// marketplace package's storage types so a future schema column
// can be added without leaking into the public surface and so the
// JSON shape is documented in one place.
// ---------------------------------------------------------------------------

type listExtensionsResponse struct {
	Items []marketplace.Extension `json:"items"`
}

type getExtensionResponse struct {
	Extension marketplace.Extension          `json:"extension"`
	Versions  []marketplace.ExtensionVersion `json:"versions"`
}

type listVersionsResponse struct {
	Items []marketplace.ExtensionVersion `json:"items"`
}

type listInstallationsResponse struct {
	Items []installationView `json:"items"`
}

// installationView mirrors marketplace.Installation but inlines a
// settings map so the JSON consumer doesn't have to round-trip the
// raw JSONB body. The marketplace.Installation.Settings field is
// tagged `json:"-"` because its raw form is a []byte; we surface
// the parsed object here.
type installationView struct {
	ID                    uuid.UUID              `json:"id"`
	TenantID              uuid.UUID              `json:"tenant_id"`
	ExtensionID           uuid.UUID              `json:"extension_id"`
	ExtensionVersionID    uuid.UUID              `json:"extension_version_id"`
	Status                marketplace.InstallStatus `json:"status"`
	Settings              map[string]any         `json:"settings"`
	WebhookBase           string                 `json:"webhook_base"`
	InstalledBy           *uuid.UUID             `json:"installed_by,omitempty"`
	InstalledAt           string                 `json:"installed_at"`
	UpdatedAt             string                 `json:"updated_at"`
	LastHealthCheckAt     string                 `json:"last_health_check_at,omitempty"`
	LastHealthCheckStatus string                 `json:"last_health_check_status,omitempty"`
	FailureReason         string                 `json:"failure_reason,omitempty"`
}

func installationToView(in *marketplace.Installation) installationView {
	v := installationView{
		ID:                    in.ID,
		TenantID:              in.TenantID,
		ExtensionID:           in.ExtensionID,
		ExtensionVersionID:    in.ExtensionVersionID,
		Status:                in.Status,
		WebhookBase:           in.WebhookBase,
		InstalledBy:           in.InstalledBy,
		InstalledAt:           in.InstalledAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:             in.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		LastHealthCheckStatus: in.LastHealthCheckStatus,
		FailureReason:         in.FailureReason,
	}
	if in.LastHealthCheckAt != nil {
		v.LastHealthCheckAt = in.LastHealthCheckAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if len(in.Settings) == 0 {
		v.Settings = map[string]any{}
	} else if err := json.Unmarshal(in.Settings, &v.Settings); err != nil {
		// Persisted bytes must be JSON (DB-side CHECK + the
		// engine only ever writes through json.Marshal). A
		// failure here implies catastrophic corruption — empty
		// the map rather than crashing the response so the
		// caller still sees the rest of the installation.
		v.Settings = map[string]any{}
	}
	return v
}

type installRequestBody struct {
	ExtensionID string         `json:"extension_id"`
	VersionID   string         `json:"version_id"`
	WebhookBase string         `json:"webhook_base"`
	Settings    map[string]any `json:"settings"`
}

type installResponse struct {
	Installation  installationView `json:"installation"`
	SigningSecret string           `json:"signing_secret"`
}

type updateSettingsRequestBody struct {
	Settings map[string]any `json:"settings"`
}

type updateSettingsResponse struct {
	Installation installationView `json:"installation"`
}

// upgradeRequestBody is the wire shape for POST
// /installations/{install_id}/upgrade. FromVersionID is required:
// it carries the version the caller observed the install at when
// it decided to upgrade, and the engine re-verifies the in-tx
// row still holds that value before committing the swap. Without
// it, two operators racing on the same install row could both
// commit an upgrade and the second would silently double-bump
// the version_id.
//
// Settings and KeepSettings are mutually exclusive:
//
//   - Settings = nil + KeepSettings = false (default): the engine
//     reads the existing settings document under the row lock
//     and writes it back verbatim. This is the common forward-
//     compatible upgrade within a major version where the new
//     schema is additive.
//   - Settings != nil: the handler validated this document
//     against the TARGET version's settings_schema before the
//     engine sees the request. Use this for breaking-schema
//     upgrades that require a migrated settings document.
//   - KeepSettings = true: explicit "preserve existing" signal,
//     equivalent to omitting Settings. Surfaces in the wire
//     contract so a caller can be unambiguous (vs. "settings: {}"
//     which would WIPE settings to {}).
//
// Cross-major-version contract: the keep-existing branches
// (default and KeepSettings = true) only bypass schema
// validation safely when the upgrade is forward-compatible
// within a major version, i.e. the target schema is additive
// relative to the source schema. Callers MUST supply a migrated
// Settings document when upgrading across major versions whose
// settings_schema has breaking changes — the engine is
// schema-agnostic and will silently persist the stale document
// otherwise. There is no engine-side guard for this: it is the
// caller's responsibility (the publisher's CHANGELOG documents
// which upgrades require Settings).
type upgradeRequestBody struct {
	FromVersionID string         `json:"from_version_id"`
	ToVersionID   string         `json:"to_version_id"`
	Settings      map[string]any `json:"settings,omitempty"`
	KeepSettings  bool           `json:"keep_settings,omitempty"`
	// SettingsProvided distinguishes between "caller did not
	// supply a settings document" (Settings is the zero-value
	// nil map) and "caller explicitly sent settings: null". The
	// JSON decoder collapses both into Settings == nil, so this
	// flag is set by a custom UnmarshalJSON below that inspects
	// the raw bytes.
	SettingsProvided bool `json:"-"`
}

// UnmarshalJSON differentiates "settings absent from the body"
// from "settings: null" from "settings: {}". The engine's three
// branches (Settings != nil, KeepSettings, default-keep) require
// the handler to know which of these the caller intended:
//
//   - {} present and non-null → Settings = parsed map (may be
//     empty), SettingsProvided = true. Engine writes the parsed
//     map (an empty map clears settings to {}).
//   - settings: null → Settings = nil, SettingsProvided = true.
//     Same as KeepSettings: existing settings preserved.
//   - settings absent → Settings = nil, SettingsProvided = false.
//     Default keep-existing branch.
func (r *upgradeRequestBody) UnmarshalJSON(data []byte) error {
	type alias upgradeRequestBody
	aux := struct {
		Settings *map[string]any `json:"settings,omitempty"`
		*alias
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Settings != nil {
		r.Settings = *aux.Settings
		r.SettingsProvided = true
	} else {
		// Either "settings: null" (raw contained the key with
		// JSON null) or the key was absent. Differentiate by
		// re-scanning the raw bytes for the key. This is
		// O(len(body)) but body is bounded by the API max-body
		// limit, so the cost is constant per request.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(data, &probe); err == nil {
			if raw, ok := probe["settings"]; ok && string(raw) == "null" {
				r.SettingsProvided = true
			}
		}
	}
	return nil
}

type upgradeResponse struct {
	Installation  installationView `json:"installation"`
	FromVersionID string           `json:"from_version_id"`
}

type createExtensionRequestBody struct {
	Publisher    string `json:"publisher"`
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	Description  string `json:"description"`
	Author       string `json:"author"`
	License      string `json:"license"`
	Homepage     string `json:"homepage,omitempty"`
	SupportEmail string `json:"support_email,omitempty"`
	IconURL      string `json:"icon_url,omitempty"`
}

type publishVersionRequestBody struct {
	Manifest    json.RawMessage `json:"manifest"`     // raw manifest bytes (YAML or JSON)
	BundleURL   string          `json:"bundle_url"`
	BundleHash  string          `json:"bundle_hash"`
	BundleSize  int64           `json:"bundle_size"`
}

type reviewQueueResponse struct {
	Items []reviewQueueItem `json:"items"`
}

type reviewQueueItem struct {
	VersionID         uuid.UUID                `json:"version_id"`
	Status            marketplace.ReviewStatus `json:"status"`
	ManualReviewNotes string                   `json:"manual_review_notes,omitempty"`
	Reviewer          string                   `json:"reviewer,omitempty"`
	ReviewedAt        string                   `json:"reviewed_at,omitempty"`
	CreatedAt         string                   `json:"created_at"`
	UpdatedAt         string                   `json:"updated_at"`
}

type reviewTransitionRequestBody struct {
	Status      string `json:"status"`
	ManualNotes string `json:"manual_notes,omitempty"`
}

type listRequestBody struct {
	Version string `json:"version"`
}

type yankRequestBody struct {
	Reason string `json:"reason"`
}

// ---------------------------------------------------------------------------
// Tenant-scoped browse / detail endpoints. Mount under tenantChain.
// All four are pure reads; no rate-limit or idempotency
// middleware needed.
// ---------------------------------------------------------------------------

// listExtensions surfaces only the `listed` catalog entries — a
// tenant browsing the marketplace shouldn't see unpublished
// drafts or removed listings. The publisher endpoint
// (listPublisherExtensions) returns the publisher's own
// extensions across all statuses.
//
// Pagination: ?limit= caps the page size (default 100, max 500
// enforced by the store). ?publisher= filters to a single
// publisher. ?q= performs a case-insensitive substring match on
// the display_name / description columns in Go (the store does
// not currently expose a full-text query path; if catalog growth
// makes that expensive we'll add a tsvector column in B6.1).
func (h *marketplaceHandlers) listExtensions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	publisher := strings.TrimSpace(r.URL.Query().Get("publisher"))
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	exts, err := h.store.ListExtensions(r.Context(), marketplace.ListExtensionsFilter{
		Status:    marketplace.ExtensionStatusListed,
		Publisher: publisher,
		Limit:     limit,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	if query != "" {
		filtered := make([]marketplace.Extension, 0, len(exts))
		for i := range exts {
			e := &exts[i]
			if strings.Contains(strings.ToLower(e.DisplayName), query) ||
				strings.Contains(strings.ToLower(e.Description), query) ||
				strings.Contains(strings.ToLower(e.Name), query) {
				filtered = append(filtered, *e)
			}
		}
		exts = filtered
	}
	writeJSON(w, http.StatusOK, listExtensionsResponse{Items: exts})
}

// getExtension returns the extension row plus every approved,
// non-yanked version. The detail page renders the listed_version
// as the default install target; the versions list lets the
// operator pin an older approved version.
func (h *marketplaceHandlers) getExtension(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "ext_id"))
	if err != nil {
		http.Error(w, "invalid extension_id", http.StatusBadRequest)
		return
	}
	ext, err := h.store.GetExtension(r.Context(), id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	// Tenant browse view only includes 'listed' extensions —
	// surface an opaque 404 for any other status so probing
	// for unpublished slugs by id doesn't leak existence.
	if ext.Status != marketplace.ExtensionStatusListed {
		http.Error(w, "extension not found", http.StatusNotFound)
		return
	}
	vers, err := h.listApprovedVersions(r.Context(), ext.ID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, getExtensionResponse{Extension: *ext, Versions: vers})
}

// listVersions returns every approved, non-yanked version of an
// extension. Same status gating as getExtension.
func (h *marketplaceHandlers) listVersions(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "ext_id"))
	if err != nil {
		http.Error(w, "invalid extension_id", http.StatusBadRequest)
		return
	}
	ext, err := h.store.GetExtension(r.Context(), id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if ext.Status != marketplace.ExtensionStatusListed {
		http.Error(w, "extension not found", http.StatusNotFound)
		return
	}
	vers, err := h.listApprovedVersions(r.Context(), id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listVersionsResponse{Items: vers})
}

// listApprovedVersions filters the full version list down to
// versions that have been approved AND are not yanked. The
// review-state-per-version filter happens in Go because the
// existing store API doesn't have a JOIN-with-review-state
// path; the per-extension version count is bounded (10s typical,
// hundreds in pathological cases) so the N+1 query cost is
// acceptable for v1.
func (h *marketplaceHandlers) listApprovedVersions(ctx context.Context, extID uuid.UUID) ([]marketplace.ExtensionVersion, error) {
	all, err := h.store.ListVersions(ctx, extID, false)
	if err != nil {
		return nil, err
	}
	rs := h.store.Reviews()
	approved := make([]marketplace.ExtensionVersion, 0, len(all))
	for i := range all {
		v := &all[i]
		state, err := rs.GetReviewState(ctx, v.ID)
		if err != nil {
			// A missing review row shouldn't happen
			// (PublishVersion seeds one), but if it does
			// the version is not browseable. Skip rather
			// than fail the whole list.
			continue
		}
		if state.Status == marketplace.ReviewStatusApproved {
			approved = append(approved, *v)
		}
	}
	return approved, nil
}

// ---------------------------------------------------------------------------
// Tenant-scoped installation endpoints. Mount under tenantChain.
// install/uninstall need idempotency + rate-limit middleware
// because they trigger external HTTP dispatch and DB writes.
// ---------------------------------------------------------------------------

// listInstallations returns every install row for the requesting
// tenant. RLS + the explicit `WHERE tenant_id = $1` predicate
// (Devin Review ANALYSIS_0005 on PR #128) ensure the rows are
// scoped correctly even under a BYPASSRLS connection role.
func (h *marketplaceHandlers) listInstallations(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	rows, err := h.store.ListInstallationsForTenant(r.Context(), t.ID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]installationView, 0, len(rows))
	for i := range rows {
		out = append(out, installationToView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, listInstallationsResponse{Items: out})
}

// getInstallation returns a single install row. Returns 404 if
// the install_id doesn't belong to the requesting tenant (the
// RLS-scoped GetInstallation surfaces ErrNotFound for that case).
func (h *marketplaceHandlers) getInstallation(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "install_id"))
	if err != nil {
		http.Error(w, "invalid installation_id", http.StatusBadRequest)
		return
	}
	in, err := h.store.GetInstallation(r.Context(), t.ID, id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, installationToView(in))
}

// install drives the end-to-end install flow:
//
//   1. Parse + validate request body (ext id, version id,
//      webhook base, settings).
//   2. Fetch the bundle via the resolver. Bundle hash is
//      verified against the version row inside the resolver.
//   3. Validate the operator-supplied settings against the
//      bundle's settings_schema (when present).
//   4. Call Engine.Install which runs pre_install hook, writes
//      registry rows (KTypes / workflows / tools / webhook
//      subscriptions), commits the install row, then fires
//      post_install best-effort.
//   5. Return the install row + signing secret. The secret is
//      returned exactly once — the publisher uses it to
//      configure their webhook server.
//
// Errors:
//   - 400 for body parse / schema-violation / invalid webhook
//   - 404 if extension or version is unknown
//   - 409 if the tenant already has this extension installed,
//     or if the version is yanked
//   - 413 if the bundle exceeds the size cap
//   - 422 if the bundle is malformed or pre_install rejected
//   - 502 if the bundle fetch failed upstream
func (h *marketplaceHandlers) install(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req installRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	extID, err := uuid.Parse(req.ExtensionID)
	if err != nil {
		http.Error(w, "invalid extension_id", http.StatusBadRequest)
		return
	}
	verID, err := uuid.Parse(req.VersionID)
	if err != nil {
		http.Error(w, "invalid version_id", http.StatusBadRequest)
		return
	}
	if req.WebhookBase == "" {
		http.Error(w, "webhook_base required", http.StatusBadRequest)
		return
	}

	// Load the version row so we can hand it to the resolver
	// for hash verification. Resolver re-checks the bundle URL
	// scheme and the SHA-256 against the catalog-recorded hash.
	ver, err := h.store.GetVersion(r.Context(), verID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if ver.ExtensionID != extID {
		http.Error(w, "version does not belong to extension", http.StatusBadRequest)
		return
	}

	// Defence-in-depth short-circuit BEFORE the bundle fetch:
	// reject installs against yanked versions (or against
	// extensions whose listing has been pulled / suspended) so
	// the CDN / cache layer isn't asked to resolve a bundle we
	// were never going to install. The engine re-checks both
	// invariants inside its tx (under SELECT FOR UPDATE on the
	// extension row) so an extension that transitions to
	// suspended between this check and the engine call is still
	// rejected — this gate is purely an early-out so a yanked-
	// version installation attempt fails on a single SELECT
	// instead of a bundle.Resolver round trip.
	if ver.Yanked {
		h.writeError(w, fmt.Errorf("%w: version %s is yanked", marketplace.ErrYanked, verID))
		return
	}
	ext, err := h.store.GetExtension(r.Context(), extID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	// Only ExtensionStatusListed accepts new installs. The other
	// states (Unpublished — never went live; Deprecated — only
	// existing installs continue; Removed — operator takedown)
	// all reject. Mapped to 409 because the version row itself
	// exists and is well-formed; the catalog-level status is the
	// reason we refuse.
	if ext.Status != marketplace.ExtensionStatusListed {
		h.writeError(w, fmt.Errorf("%w: extension %s is %s",
			marketplace.ErrConflict, extID, ext.Status))
		return
	}

	resolved, err := h.resolver.Resolve(r.Context(), ver)
	if err != nil {
		h.writeError(w, err)
		return
	}

	// Validate settings against the manifest-declared schema.
	// A nil resolved.SettingsSchemaJSON means the manifest
	// declared no schema and we accept any settings document.
	if err := validateInstallSettings(resolved.SettingsSchemaJSON, req.Settings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	actor := actorOrDefault(r.Context())
	result, err := h.engine.Install(r.Context(), &runtime.InstallRequest{
		TenantID:    t.ID,
		ExtensionID: extID,
		VersionID:   verID,
		WebhookBase: req.WebhookBase,
		Settings:    req.Settings,
		InstalledBy: actor,
	}, resolved)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, installResponse{
		Installation:  installationToView(result.Installation),
		SigningSecret: string(result.SigningSecret),
	})
}

// updateSettings validates the new settings against the
// installed version's schema, then calls Engine.UpdateSettings.
// The engine re-validates inside its tx (`SELECT ... FOR
// UPDATE`) so concurrent uninstalls serialize correctly.
func (h *marketplaceHandlers) updateSettings(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "install_id"))
	if err != nil {
		http.Error(w, "invalid installation_id", http.StatusBadRequest)
		return
	}
	var req updateSettingsRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Load the install row so we can resolve the bundle for
	// the version it was installed at (the version's schema
	// is the source of truth for what's valid — NOT the
	// extension's current listed_version, which may be newer).
	//
	// This pre-tx read is intentionally NOT serialised with the
	// engine's in-tx SELECT FOR UPDATE on the same row at
	// engine.go:718-722 — we trade a small TOCTOU window for
	// not holding a row lock across the bundle resolve +
	// JSON-Schema compile (which can be seconds against a cold
	// CDN). Two facts make the trade safe:
	//
	//   1. extension_version_id is functionally immutable for an
	//      install row in B6 — Engine.Upgrade is deferred to
	//      B6.1; the v1 migration path is uninstall-then-reinstall
	//      which creates a fresh row. So in current production
	//      code the value cannot change between our read and the
	//      engine's commit.
	//   2. Extension manifest spec contract: settings schemas are
	//      forward-compatible within a major version (additive
	//      properties only). Even if B6.1 lands a same-tx upgrade
	//      that mutates extension_version_id between our pre-read
	//      and the engine's FOR UPDATE, a document valid under
	//      v1.X is required to be valid under v1.Y for Y > X.
	//
	// Devin Review ANALYSIS_pr-review-job-...-bbc14be-0004.
	in, err := h.store.GetInstallation(r.Context(), t.ID, id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	// Short-circuit on terminal status before doing the
	// (potentially CDN-bound) bundle fetch and JSON-Schema
	// compile. The engine's in-tx SELECT FOR UPDATE at
	// engine.go:718-722 is still the authoritative guard
	// against the TOCTOU window between this check and the
	// settings write — we just save the wasted round-trip
	// on the obvious case where the install is already
	// torn down. The 409 / ErrConflict mapping mirrors what
	// the engine would return.
	if in.Status == marketplace.InstallStatusUninstalled {
		h.writeError(w, fmt.Errorf("%w: installation %s is uninstalled", marketplace.ErrConflict, id))
		return
	}
	ver, err := h.store.GetVersion(r.Context(), in.ExtensionVersionID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	resolved, err := h.resolver.Resolve(r.Context(), ver)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if err := validateInstallSettings(resolved.SettingsSchemaJSON, req.Settings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	actor := actorOrDefault(r.Context())
	res, err := h.engine.UpdateSettings(r.Context(), &runtime.UpdateSettingsRequest{
		TenantID:       t.ID,
		InstallationID: id,
		UpdatedBy:      actor,
		Settings:       req.Settings,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updateSettingsResponse{Installation: installationToView(res.Installation)})
}

// upgrade swaps an installation's extension_version_id to a new
// version (must be the same extension) and atomically re-registers
// the runtime tables (ktypes / workflows / agent_tools / webhook
// subscriptions / posting_hooks) against the new bundle. The
// install row's id, signing_secret, webhook_base, and installed_by
// are preserved — operators are not asked to reconfigure
// anything that didn't change.
//
// Settings semantics (see upgradeRequestBody godoc):
//
//   - Body omits "settings" entirely OR includes "settings: null":
//     the engine reads the existing settings document under FOR
//     UPDATE and writes it back verbatim. KeepSettings=true is
//     the explicit form of the same intent.
//   - Body includes "settings: {...}": the handler validates the
//     new document against the TARGET version's settings_schema,
//     and the engine writes it.
//
// The handler is the layer that owns schema validation (the
// engine is intentionally schema-agnostic — it has the
// resolver-supplied SettingsSchemaJSON only inside its own
// install-time path). For the keep-existing path the engine
// re-reads the existing settings document inside the tx so the
// document the publisher's post_upgrade hook sees matches what
// is in the DB; the handler doesn't re-validate the existing
// document because it was already validated against the FROM
// version's schema at install/update time and the forward-
// compatible-within-major contract guarantees it remains valid
// under additive-only schema changes.
func (h *marketplaceHandlers) upgrade(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	installID, err := uuid.Parse(chi.URLParam(r, "install_id"))
	if err != nil {
		http.Error(w, "invalid installation_id", http.StatusBadRequest)
		return
	}
	var req upgradeRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	fromVer, err := uuid.Parse(req.FromVersionID)
	if err != nil {
		http.Error(w, "invalid from_version_id", http.StatusBadRequest)
		return
	}
	toVer, err := uuid.Parse(req.ToVersionID)
	if err != nil {
		http.Error(w, "invalid to_version_id", http.StatusBadRequest)
		return
	}

	// Reject contradictory combinations at the wire layer rather
	// than letting one field silently win. keep_settings is the
	// explicit "preserve existing" signal; a non-null settings
	// document is the explicit "migrate to this document" signal.
	// Sending both at once is operator confusion — return 400 so
	// the caller fixes the request rather than silently getting
	// one of the two interpretations. "settings: null" + keep_settings
	// is fine because both express the same intent (preserve).
	//
	// Checked here (pre-DB, pre-resolve) because it's pure body
	// validation — no point spending a SELECT + a CDN round trip
	// on a request the wire contract already disallows.
	if req.KeepSettings && req.SettingsProvided && req.Settings != nil {
		http.Error(w, "keep_settings and a non-null settings document are mutually exclusive", http.StatusBadRequest)
		return
	}

	// Pre-flight: confirm the install row exists and is in a
	// permissible state before we spend a CDN round trip on the
	// bundle resolve. The engine's in-tx FOR UPDATE is still the
	// authoritative guard against TOCTOU; this is just an
	// early-out so a doomed request fails on a single SELECT.
	in, err := h.store.GetInstallation(r.Context(), t.ID, installID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	// Allowlist: only 'active' and 'failed' may be upgraded. The
	// engine re-applies this allowlist under FOR UPDATE; this is
	// the same early-out pattern as the rest of this handler.
	// See engine_upgrade.go for the full rationale (disabled
	// installs must not be silently re-activated, pending must
	// not be advanced before first-time setup completes).
	if in.Status != marketplace.InstallStatusActive &&
		in.Status != marketplace.InstallStatusFailed {
		h.writeError(w, fmt.Errorf("%w: installation %s has status %q, upgrade requires 'active' or 'failed'",
			marketplace.ErrConflict, installID, in.Status))
		return
	}

	// Load the target version + extension so we can resolve the
	// bundle and (if the caller supplied a settings document)
	// validate it against the new schema.
	verRow, err := h.store.GetVersion(r.Context(), toVer)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if verRow.ExtensionID != in.ExtensionID {
		// Cross-extension upgrade is not a real upgrade — it's
		// an install of a different extension. Reject with 409
		// rather than 400 because the request is syntactically
		// valid; the precondition (versions must belong to the
		// same extension) fails semantically.
		h.writeError(w, fmt.Errorf("%w: target version %s belongs to extension %s, not install's extension %s",
			marketplace.ErrConflict, toVer, verRow.ExtensionID, in.ExtensionID))
		return
	}
	if verRow.Yanked {
		h.writeError(w, fmt.Errorf("%w: target version %s is yanked", marketplace.ErrYanked, toVer))
		return
	}

	resolved, err := h.resolver.Resolve(r.Context(), verRow)
	if err != nil {
		h.writeError(w, err)
		return
	}

	// Schema-validate the new settings document if the caller
	// supplied one (settings present AND non-null in the body).
	// The keep-existing branches (no settings key OR settings:null
	// OR keep_settings: true) bypass validation — the existing
	// document was already validated against the FROM version's
	// schema and the forward-compatible-within-major contract
	// keeps it valid under additive-only changes.
	if req.SettingsProvided && req.Settings != nil {
		if err := validateInstallSettings(resolved.SettingsSchemaJSON, req.Settings); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Translate the wire flags into the engine's
	// UpgradeRequest contract:
	//
	//   - settings absent (SettingsProvided == false): default
	//     keep-existing branch on the engine.
	//   - settings: null (SettingsProvided == true, Settings ==
	//     nil): equivalent to KeepSettings = true.
	//   - keep_settings: true (regardless of settings field):
	//     KeepSettings = true.
	//   - settings: {...} (SettingsProvided == true, Settings !=
	//     nil): pass through.
	upgradeReq := &runtime.UpgradeRequest{
		TenantID:       t.ID,
		InstallationID: installID,
		FromVersionID:  fromVer,
		ToVersionID:    toVer,
		UpgradedBy:     actorOrDefault(r.Context()),
	}
	switch {
	case req.KeepSettings:
		upgradeReq.KeepSettings = true
	case req.SettingsProvided && req.Settings == nil:
		// Wire form "settings: null" — same as keep-existing.
		upgradeReq.KeepSettings = true
	case req.SettingsProvided && req.Settings != nil:
		upgradeReq.Settings = req.Settings
	}

	result, err := h.engine.Upgrade(r.Context(), upgradeReq, resolved)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, upgradeResponse{
		Installation:  installationToView(result.Installation),
		FromVersionID: result.FromVersionID.String(),
	})
}

// uninstall hard-deletes the registry rows registered at install
// time and marks the install row uninstalled. Lifecycle hooks
// (pre_uninstall + post_uninstall) fire best-effort.
func (h *marketplaceHandlers) uninstall(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "install_id"))
	if err != nil {
		http.Error(w, "invalid installation_id", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	if _, err := h.engine.Uninstall(r.Context(), &runtime.UninstallRequest{
		TenantID:       t.ID,
		InstallationID: id,
		UninstalledBy:  actor,
	}); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Publisher endpoints. Mount under tenantChain with the
// `marketplace.publisher` authz gate. The publisher concept is
// tenant-scoped: a tenant member with the publisher role can
// create / submit versions for extensions registered under their
// configured publisher slug (a per-tenant attribute kept on the
// tenant row — out of scope for B6, validated only as the
// manifest's `publisher` field matching the request URL's
// publisher path param). For v1 we do NOT enforce
// "the requesting tenant owns this publisher slug" — that
// requires a publishers ↔ tenants table which is B7's
// responsibility. The handler accepts any tenant member with
// the role for now; the manifest's publisher slug serves as
// the trust boundary at review time.
// ---------------------------------------------------------------------------

// createExtension registers a new extension shell (no versions
// yet). The publisher must subsequently call submitVersion to
// upload bundles. Status starts as `unpublished` until the
// first approved version's SetListedVersion call flips it.
func (h *marketplaceHandlers) createExtension(w http.ResponseWriter, r *http.Request) {
	var req createExtensionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	ext, err := h.store.CreateExtension(r.Context(), marketplace.CreateExtensionInput{
		Publisher:    req.Publisher,
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
	writeJSON(w, http.StatusCreated, ext)
}

// listPublisherExtensions returns extensions filtered by the
// `publisher` query parameter (the publisher's tenant uses this
// to render their dashboard).
func (h *marketplaceHandlers) listPublisherExtensions(w http.ResponseWriter, r *http.Request) {
	publisher := strings.TrimSpace(r.URL.Query().Get("publisher"))
	if publisher == "" {
		http.Error(w, "publisher query param required", http.StatusBadRequest)
		return
	}
	exts, err := h.store.ListExtensions(r.Context(), marketplace.ListExtensionsFilter{
		Publisher: publisher,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listExtensionsResponse{Items: exts})
}

// submitVersion publishes a new version row. The manifest body
// is parsed + validated by marketplace.ParseManifest; the store
// re-asserts bundle size, hash format, and unique (extension,
// version) constraints. The version starts in review_status
// `submitted`; the admin review queue advances it.
func (h *marketplaceHandlers) submitVersion(w http.ResponseWriter, r *http.Request) {
	extID, err := uuid.Parse(chi.URLParam(r, "ext_id"))
	if err != nil {
		http.Error(w, "invalid extension_id", http.StatusBadRequest)
		return
	}
	var req publishVersionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.Manifest) == 0 {
		http.Error(w, "manifest required", http.StatusBadRequest)
		return
	}
	man, err := marketplace.ParseManifest(req.Manifest)
	if err != nil {
		h.writeError(w, fmt.Errorf("%w: %w", marketplace.ErrInvalidManifest, err))
		return
	}
	ver, err := h.store.PublishVersion(r.Context(), marketplace.PublishVersionInput{
		ExtensionID:  extID,
		Manifest:     man,
		BundleHash:   req.BundleHash,
		BundleSize:   req.BundleSize,
		BundleURL:    req.BundleURL,
		ManifestJSON: req.Manifest,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ver)
}

// ---------------------------------------------------------------------------
// Admin endpoints. Mount under adminChain.
// ---------------------------------------------------------------------------

// reviewQueue returns versions in `submitted` or
// `automated_passed` review state — i.e. waiting for a human
// reviewer. Ordered FIFO (oldest first) so the queue surfaces
// long-waiting items first.
func (h *marketplaceHandlers) reviewQueue(w http.ResponseWriter, r *http.Request) {
	statusParam := strings.TrimSpace(r.URL.Query().Get("status"))
	statuses := []marketplace.ReviewStatus{
		marketplace.ReviewStatusSubmitted,
		marketplace.ReviewStatusAutomatedPassed,
		marketplace.ReviewStatusManualReview,
	}
	if statusParam != "" {
		s := marketplace.ReviewStatus(statusParam)
		if !s.Valid() {
			http.Error(w, "invalid review status", http.StatusBadRequest)
			return
		}
		statuses = []marketplace.ReviewStatus{s}
	}
	rs := h.store.Reviews()
	items := make([]reviewQueueItem, 0, 16)
	for _, s := range statuses {
		rows, err := rs.ListVersionsByReviewStatus(r.Context(), s, 500)
		if err != nil {
			h.writeError(w, err)
			return
		}
		for i := range rows {
			items = append(items, reviewStateToItem(rows[i]))
		}
	}
	writeJSON(w, http.StatusOK, reviewQueueResponse{Items: items})
}

// reviewTransition transitions a version's review state.
// Reviewer name is taken from the auth context (user id is the
// audit identity).
func (h *marketplaceHandlers) reviewTransition(w http.ResponseWriter, r *http.Request) {
	verID, err := uuid.Parse(chi.URLParam(r, "ver_id"))
	if err != nil {
		http.Error(w, "invalid version_id", http.StatusBadRequest)
		return
	}
	var req reviewTransitionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	status := marketplace.ReviewStatus(req.Status)
	if !status.Valid() {
		http.Error(w, "invalid review status", http.StatusBadRequest)
		return
	}
	reviewer := actorOrDefault(r.Context()).String()
	rs := h.store.Reviews()
	state, err := rs.UpdateReviewState(r.Context(), marketplace.UpdateReviewStateInput{
		VersionID:   verID,
		Status:      status,
		ManualNotes: req.ManualNotes,
		Reviewer:    reviewer,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviewStateToItem(*state))
}

// listExtension flips an extension to `listed` status with a
// specific version pinned as the default install target. The
// store enforces that the version is approved and non-yanked.
func (h *marketplaceHandlers) listExtension(w http.ResponseWriter, r *http.Request) {
	extID, err := uuid.Parse(chi.URLParam(r, "ext_id"))
	if err != nil {
		http.Error(w, "invalid extension_id", http.StatusBadRequest)
		return
	}
	var req listRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Version == "" {
		http.Error(w, "version required", http.StatusBadRequest)
		return
	}
	// Single-tx atomic write. The prior two-call sequence
	// (SetListedVersion + UpdateExtensionStatus) could leave
	// the catalog in a half-applied state on the rare DB-mid-
	// flight failure where the first call lands but the second
	// fails: listed_version pinned but status still
	// `unpublished`, which the tenant browse filter hides.
	// Devin Review BUG_pr-review-job-...-0001.
	if err := h.store.SetListedAndStatus(r.Context(), extID, req.Version, marketplace.ExtensionStatusListed); err != nil {
		h.writeError(w, err)
		return
	}
	ext, err := h.store.GetExtension(r.Context(), extID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ext)
}

// yankVersion marks a version as yanked (operator-initiated
// removal). Existing installs continue to function but new
// installs are blocked.
func (h *marketplaceHandlers) yankVersion(w http.ResponseWriter, r *http.Request) {
	verID, err := uuid.Parse(chi.URLParam(r, "ver_id"))
	if err != nil {
		http.Error(w, "invalid version_id", http.StatusBadRequest)
		return
	}
	var req yankRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.store.YankVersion(r.Context(), verID, req.Reason); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// validateInstallSettings runs the operator-supplied settings
// through the manifest's settings_schema. A nil schemaJSON
// (manifest declared no schema) accepts everything; a non-nil
// schema runs the bounded JSON Schema validator from the
// internal/marketplace/settings package. Returns a 400-shaped
// error message safe for direct exposure to the caller.
func validateInstallSettings(schemaJSON []byte, sval map[string]any) error {
	if len(schemaJSON) == 0 {
		return nil
	}
	v, err := settings.NewValidator(schemaJSON)
	if err != nil {
		return fmt.Errorf("settings_schema invalid: %w", err)
	}
	// A nil sval is identical to "no fields set" — pass an
	// empty map so required-field checks fire correctly.
	if sval == nil {
		sval = map[string]any{}
	}
	if err := v.Validate(sval); err != nil {
		return fmt.Errorf("settings invalid: %w", err)
	}
	return nil
}

func reviewStateToItem(s marketplace.ReviewState) reviewQueueItem {
	out := reviewQueueItem{
		VersionID:         s.ExtensionVersionID,
		Status:            s.Status,
		ManualReviewNotes: s.ManualReviewNotes,
		Reviewer:          s.Reviewer,
		CreatedAt:         s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:         s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if s.ReviewedAt != nil {
		out.ReviewedAt = s.ReviewedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

// writeError maps a sentinel error to an HTTP status + body. The
// fallthrough is 500 because every other path should map to a
// documented sentinel; if a new sentinel slips through we want a
// loud 500 rather than a silent 400.
func (h *marketplaceHandlers) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, marketplace.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, marketplace.ErrConflict),
		errors.Is(err, marketplace.ErrYanked),
		errors.Is(err, marketplace.ErrImmutableVersion),
		errors.Is(err, marketplace.ErrPublisherNotVerified):
		// ErrPublisherNotVerified is a state-precondition
		// failure (e.g. SetAutoApprovePatch refusing to enable
		// fast-path on an unverified row); 409 matches the
		// rest of the state-precondition family (yanked,
		// immutable version).
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, marketplace.ErrInvalidManifest),
		errors.Is(err, marketplace.ErrPermissionScopeUnknown),
		errors.Is(err, bundle.ErrBundleTransportInsecure):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, marketplace.ErrBundleTooLarge),
		errors.Is(err, bundle.ErrBundleExceedsLimit):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, bundle.ErrBundleMalformed),
		errors.Is(err, marketplace.ErrInvalidSignature):
		// ErrInvalidSignature: the bundle is structurally
		// well-formed but no registered publisher key validated
		// the detached signature. 422 because the request was
		// authentic and authorised but the publisher-supplied
		// signature failed cryptographic verification — the
		// caller cannot recover by reformatting their request.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, bundle.ErrBundleNotFound),
		errors.Is(err, bundle.ErrBundleFetchFailed),
		errors.Is(err, marketplace.ErrBundleHashMismatch):
		// ErrBundleHashMismatch is an upstream-integrity failure:
		// the CDN delivered bytes that don't match the catalog-
		// recorded SHA-256. Surfaces as 502 alongside the other
		// bundle-fetch failures because the operator's request
		// was well-formed — the upstream object store served bad
		// bytes.
		http.Error(w, err.Error(), http.StatusBadGateway)
	case errors.Is(err, runtime.ErrVersionMismatch):
		// Engine.Upgrade's TOCTOU guard caught a concurrent
		// upgrade or uninstall+reinstall that changed the install
		// row's extension_version_id between the caller's read
		// and the engine's in-tx commit. 409 is the right status:
		// the request itself was well-formed and authorised; the
		// precondition (from_version_id) no longer holds. The
		// caller should re-read the install row and decide
		// whether the upgrade is still wanted.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, runtime.ErrSameVersionUpgrade):
		// Upgrade to the version the install is already at is
		// rejected as a client error rather than silently no-op
		// — silently bumping updated_at and firing lifecycle
		// hooks for nothing would mislead audit consumers. 400
		// because the body is malformed (from_version_id ==
		// to_version_id is a programmer error in the caller).
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, runtime.ErrPreInstallRejected),
		errors.Is(err, runtime.ErrPreUninstallRejected),
		errors.Is(err, runtime.ErrPreUpgradeRejected):
		// Symmetric with ErrPreInstallRejected. The extension's
		// pre_uninstall webhook returned a structured rejection;
		// the engine surfaces this distinct from a generic 500
		// transport failure so operators can see the publisher
		// explicitly refused the uninstall. 422 is the right
		// status because the request itself was well-formed and
		// authorised; the publisher rejected the lifecycle
		// transition.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		// Defense-in-depth: log the full error server-side so an
		// operator triaging a 500 can grep the API log for the
		// concrete cause, but DO NOT surface the underlying
		// error string to the HTTP client. Unknown errors here
		// originate from infrastructure layers (pgx, network,
		// context cancellation, etc.) and frequently carry
		// SQL fragments, hostnames, file paths, or stack-derived
		// detail that would let an unauthenticated probe
		// fingerprint the deployment. The sentinel-mapped arms
		// above intentionally pass err.Error() through because
		// those messages are produced by the marketplace package
		// with controlled wording. Devin Review
		// BUG_pr-review-job-...-0002.
		log.Printf("api: marketplace_handlers: 500 fallthrough: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
