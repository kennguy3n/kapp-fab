package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Phase B8 — publisher dashboard surface.
//
// B7.1 (#133) added self-service membership + key management for
// publishers. B8 builds on that: publisher members need to see
// "the extensions my publisher owns + their versions + the review
// state for each version + the findings the review pipeline
// emitted + how many tenants have installed each version."
//
// All handlers in this file mount under the same
// /api/v1/publisher/{publisher_id}/... tenant-chain prefix as the
// B7.1 self-service surface. Each handler:
//
//   1. Parses publisher_id from the URL.
//   2. Resolves the authenticated user via platform.UserIDFromContext.
//   3. Calls RequireMemberRole(member) — every dashboard endpoint
//      is member-level (no owner-only reads); owner-only ops are
//      mutation-only (add member, remove member, modify settings).
//   4. Performs the data query, filtered to the publisher's
//      extensions via a JOIN on marketplace_extensions.publisher
//      = marketplace_publishers.slug. The extensions table uses
//      the slug as its foreign-key column (predates the
//      publishers table, see migrations/000073 backfill notes),
//      so the JOIN goes through the slug not the publisher_id.
//   5. Returns the JSON-shaped view types (publisherExtensionView,
//      publisherVersionView, etc.).
//
// Cross-tenant install-stats endpoint: bypasses RLS via the admin
// pool because installations live on per-tenant schemas. Returns
// 503 when the deploy did not provision an admin pool.

// publisherExtensionView is the per-extension shape in the
// publisher dashboard list. Distinct from extensionView used by
// the public catalog browse because the publisher view surfaces
// state the public catalog hides (e.g. unpublished extensions).
type publisherExtensionView struct {
	ID            uuid.UUID                  `json:"id"`
	Name          string                     `json:"name"`
	Publisher     string                     `json:"publisher"`
	Slug          string                     `json:"slug"`
	DisplayName   string                     `json:"display_name"`
	Description   string                     `json:"description"`
	Author        string                     `json:"author"`
	License       string                     `json:"license"`
	Homepage      string                     `json:"homepage,omitempty"`
	SupportEmail  string                     `json:"support_email,omitempty"`
	IconURL       string                     `json:"icon_url,omitempty"`
	Status        marketplace.ExtensionStatus `json:"status"`
	ListedVersion string                     `json:"listed_version,omitempty"`
	CreatedAt     string                     `json:"created_at"`
	UpdatedAt     string                     `json:"updated_at"`
}

func publisherExtensionToView(e *marketplace.Extension) publisherExtensionView {
	return publisherExtensionView{
		ID:            e.ID,
		Name:          e.Name,
		Publisher:     e.Publisher,
		Slug:          e.Slug,
		DisplayName:   e.DisplayName,
		Description:   e.Description,
		Author:        e.Author,
		License:       e.License,
		Homepage:      e.Homepage,
		SupportEmail:  e.SupportEmail,
		IconURL:       e.IconURL,
		Status:        e.Status,
		ListedVersion: e.ListedVersion,
		CreatedAt:     e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:     e.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// publisherVersionView is the per-version shape. Surfaces the
// raw catalog row + a flat review-state summary so the dashboard
// can render a "v1.2.3 — Approved 2 days ago" row without a
// second GET.
type publisherVersionView struct {
	ID                  uuid.UUID `json:"id"`
	ExtensionID         uuid.UUID `json:"extension_id"`
	Version             string    `json:"version"`
	BundleHash          string    `json:"bundle_hash"`
	BundleSizeBytes     int64     `json:"bundle_size_bytes"`
	BundleURL           string    `json:"bundle_url"`
	MinKappVersion      string    `json:"min_kapp_version"`
	MaxKappVersion      string    `json:"max_kapp_version,omitempty"`
	FeaturesRequired    []string  `json:"features_required"`
	PermissionsRequired []string  `json:"permissions_required"`
	KtypesCount         int       `json:"ktypes_count"`
	WorkflowsCount      int       `json:"workflows_count"`
	AgentToolsCount     int       `json:"agent_tools_count"`
	UIExtensionsCount   int       `json:"ui_extensions_count"`
	WebhooksCount       int       `json:"webhooks_count"`
	Yanked              bool      `json:"yanked"`
	YankedReason        string    `json:"yanked_reason,omitempty"`
	PublishedAt         string    `json:"published_at"`

	ReviewStatus       string `json:"review_status,omitempty"`
	ReviewerNotes      string `json:"reviewer_notes,omitempty"`
	ReviewedAt         string `json:"reviewed_at,omitempty"`
	AttemptCount       int    `json:"attempt_count"`
	LastAttemptError   string `json:"last_attempt_error,omitempty"`

	BundleSignature      string `json:"bundle_signature,omitempty"`
	BundleSignatureKeyID string `json:"bundle_signature_key_id,omitempty"`
	SignedAt             string `json:"signed_at,omitempty"`
}

func publisherVersionToView(v *marketplace.ExtensionVersion, rs *marketplace.ReviewState) publisherVersionView {
	out := publisherVersionView{
		ID:                  v.ID,
		ExtensionID:         v.ExtensionID,
		Version:             v.Version,
		BundleHash:          v.BundleHash,
		BundleSizeBytes:     v.BundleSizeBytes,
		BundleURL:           v.BundleURL,
		MinKappVersion:      v.MinKappVersion,
		MaxKappVersion:      v.MaxKappVersion,
		FeaturesRequired:    v.FeaturesRequired,
		PermissionsRequired: v.PermissionsRequired,
		KtypesCount:         v.KtypesCount,
		WorkflowsCount:      v.WorkflowsCount,
		AgentToolsCount:     v.AgentToolsCount,
		UIExtensionsCount:   v.UIExtensionsCount,
		WebhooksCount:       v.WebhooksCount,
		Yanked:              v.Yanked,
		YankedReason:        v.YankedReason,
		PublishedAt:         v.PublishedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),

		BundleSignature:      v.BundleSignature,
		BundleSignatureKeyID: v.BundleSignatureKeyID,
	}
	if v.SignedAt != nil {
		out.SignedAt = v.SignedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if rs != nil {
		out.ReviewStatus = string(rs.Status)
		out.ReviewerNotes = rs.ManualReviewNotes
		out.AttemptCount = rs.AttemptCount
		out.LastAttemptError = rs.LastAttemptError
		if rs.ReviewedAt != nil {
			out.ReviewedAt = rs.ReviewedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
	}
	return out
}

// publisherReviewStateView is the dashboard view of one
// ReviewState row. Distinct from the version-row review summary
// above because this endpoint returns the full AutomatedChecks
// JSONB so the publisher can drill into per-check details.
type publisherReviewStateView struct {
	ExtensionVersionID uuid.UUID       `json:"extension_version_id"`
	Status             string          `json:"status"`
	AutomatedChecks    json.RawMessage `json:"automated_checks,omitempty"`
	ManualReviewNotes  string          `json:"manual_review_notes,omitempty"`
	Reviewer           string          `json:"reviewer,omitempty"`
	ReviewedAt         string          `json:"reviewed_at,omitempty"`
	AttemptCount       int             `json:"attempt_count"`
	LastAttemptError   string          `json:"last_attempt_error,omitempty"`
	LastAttemptAt      string          `json:"last_attempt_at,omitempty"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
}

func reviewStateToPublisherView(rs *marketplace.ReviewState) publisherReviewStateView {
	out := publisherReviewStateView{
		ExtensionVersionID: rs.ExtensionVersionID,
		Status:             string(rs.Status),
		ManualReviewNotes:  rs.ManualReviewNotes,
		Reviewer:           rs.Reviewer,
		AttemptCount:       rs.AttemptCount,
		LastAttemptError:   rs.LastAttemptError,
		CreatedAt:          rs.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:          rs.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if len(rs.AutomatedChecks) > 0 {
		out.AutomatedChecks = json.RawMessage(rs.AutomatedChecks)
	}
	if rs.ReviewedAt != nil {
		out.ReviewedAt = rs.ReviewedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if rs.LastAttemptAt != nil {
		out.LastAttemptAt = rs.LastAttemptAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

// installStatsView is the cross-tenant install-statistics shape.
// TotalInstalls is the count across every tenant that has at
// least one install row (status != uninstalled). ByVersion is the
// per-version breakdown so the publisher can see "v1.0.0 has 47
// installs, v1.1.0 has 12, v0.9.0 has 3 stragglers."
type installStatsView struct {
	ExtensionID   uuid.UUID            `json:"extension_id"`
	ExtensionName string               `json:"extension_name"`
	TotalInstalls int                  `json:"total_installs"`
	ByVersion     []installStatsRow    `json:"by_version"`
	ByStatus      map[string]int       `json:"by_status"`
}

type installStatsRow struct {
	VersionID    uuid.UUID `json:"version_id"`
	Version      string    `json:"version"`
	Installs     int       `json:"installs"`
	ActiveCount  int       `json:"active_count"`
	DisabledCount int      `json:"disabled_count"`
	FailedCount  int       `json:"failed_count"`
}

// --- handlers ----------------------------------------------------

// listMyPublisherExtensions — GET /api/v1/publisher/{publisher_id}/extensions
//
// Member-level. Returns every extension owned by the publisher
// (slug-matched), regardless of listing status (unpublished /
// listed / deprecated / removed all surface). The public catalog
// browse endpoint hides non-listed rows; the publisher dashboard
// surfaces them so the publisher can see drafts in progress.
func (h *marketplaceHandlers) listMyPublisherExtensions(w http.ResponseWriter, r *http.Request) {
	pubID, ok := h.requireDashboardMember(w, r)
	if !ok {
		return
	}
	pub, err := h.store.Publishers().GetPublisher(r.Context(), pubID)
	if err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	exts, err := h.store.ListExtensions(r.Context(), marketplace.ListExtensionsFilter{
		Publisher: pub.Slug,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherExtensionView, 0, len(exts))
	for i := range exts {
		out = append(out, publisherExtensionToView(&exts[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// getMyPublisherExtension — GET /api/v1/publisher/{publisher_id}/extensions/{ext_id}
//
// Member-level. 404 if the extension belongs to a different publisher.
func (h *marketplaceHandlers) getMyPublisherExtension(w http.ResponseWriter, r *http.Request) {
	pubID, ok := h.requireDashboardMember(w, r)
	if !ok {
		return
	}
	extID, ok := parseExtIDParam(w, r)
	if !ok {
		return
	}
	ext, err := h.requirePublisherOwnsExtension(r.Context(), pubID, extID)
	if err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	writeJSON(w, http.StatusOK, publisherExtensionToView(ext))
}

// listMyPublisherVersions — GET /api/v1/publisher/{publisher_id}/extensions/{ext_id}/versions
//
// Member-level. Returns versions (including yanked) with the
// flat review-state summary inline. Per-version automated-check
// detail requires a separate /versions/{ver_id}/review call.
func (h *marketplaceHandlers) listMyPublisherVersions(w http.ResponseWriter, r *http.Request) {
	pubID, ok := h.requireDashboardMember(w, r)
	if !ok {
		return
	}
	extID, ok := parseExtIDParam(w, r)
	if !ok {
		return
	}
	if _, err := h.requirePublisherOwnsExtension(r.Context(), pubID, extID); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	versions, err := h.store.ListVersions(r.Context(), extID, true)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherVersionView, 0, len(versions))
	for i := range versions {
		rs, _ := h.store.Reviews().GetReviewState(r.Context(), versions[i].ID)
		out = append(out, publisherVersionToView(&versions[i], rs))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// getMyPublisherVersionReview — GET /api/v1/publisher/{publisher_id}/extensions/{ext_id}/versions/{ver_id}/review
//
// Member-level. Returns the full ReviewState row including the
// automated_checks JSONB blob so the publisher can drill into
// individual check results (e.g. why SignatureCheck warned).
func (h *marketplaceHandlers) getMyPublisherVersionReview(w http.ResponseWriter, r *http.Request) {
	pubID, ok := h.requireDashboardMember(w, r)
	if !ok {
		return
	}
	extID, ok := parseExtIDParam(w, r)
	if !ok {
		return
	}
	verID, ok := parseVerIDParam(w, r)
	if !ok {
		return
	}
	if _, err := h.requirePublisherOwnsExtension(r.Context(), pubID, extID); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	if err := h.requireExtensionOwnsVersion(r.Context(), extID, verID); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	rs, err := h.store.Reviews().GetReviewState(r.Context(), verID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviewStateToPublisherView(rs))
}

// listMyPublisherVersionFindings — GET /api/v1/publisher/{publisher_id}/extensions/{ext_id}/versions/{ver_id}/findings
//
// Member-level. Returns the structured per-check findings the
// review pipeline produced (or admin manual-reject reasons).
// Mirrors the admin findings endpoint but is gated by publisher
// membership instead of admin authz.
func (h *marketplaceHandlers) listMyPublisherVersionFindings(w http.ResponseWriter, r *http.Request) {
	pubID, ok := h.requireDashboardMember(w, r)
	if !ok {
		return
	}
	extID, ok := parseExtIDParam(w, r)
	if !ok {
		return
	}
	verID, ok := parseVerIDParam(w, r)
	if !ok {
		return
	}
	if _, err := h.requirePublisherOwnsExtension(r.Context(), pubID, extID); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	if err := h.requireExtensionOwnsVersion(r.Context(), extID, verID); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	findings, err := h.store.Findings().ListReviewFindings(r.Context(), verID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]reviewFindingView, 0, len(findings))
	for i := range findings {
		out = append(out, findingToView(&findings[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// getMyPublisherExtensionInstallStats — GET /api/v1/publisher/{publisher_id}/extensions/{ext_id}/install-stats
//
// Member-level. Requires the admin pool (cross-tenant query).
// Aggregates installations across every tenant by version + status
// so the publisher can see adoption metrics. Returns 503 when the
// deploy did not provision an admin pool.
func (h *marketplaceHandlers) getMyPublisherExtensionInstallStats(w http.ResponseWriter, r *http.Request) {
	pubID, ok := h.requireDashboardMember(w, r)
	if !ok {
		return
	}
	extID, ok := parseExtIDParam(w, r)
	if !ok {
		return
	}
	ext, err := h.requirePublisherOwnsExtension(r.Context(), pubID, extID)
	if err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	if h.adminPool == nil {
		http.Error(w, "install statistics require the admin pool which is not configured on this deploy",
			http.StatusServiceUnavailable)
		return
	}
	installs, err := h.store.ListInstallationsByVersion(r.Context(), h.adminPool, uuid.Nil)
	// uuid.Nil to ListInstallationsByVersion would scan the
	// entire table — explicitly fan out per-version below
	// instead.
	_ = installs
	_ = err

	versions, err := h.store.ListVersions(r.Context(), extID, true)
	if err != nil {
		h.writeError(w, err)
		return
	}
	stats := installStatsView{
		ExtensionID:   ext.ID,
		ExtensionName: ext.Name,
		ByStatus:      map[string]int{},
		ByVersion:     make([]installStatsRow, 0, len(versions)),
	}
	for i := range versions {
		v := versions[i]
		rows, err := h.store.ListInstallationsByVersion(r.Context(), h.adminPool, v.ID)
		if err != nil {
			h.writeError(w, err)
			return
		}
		row := installStatsRow{
			VersionID: v.ID,
			Version:   v.Version,
			Installs:  len(rows),
		}
		// Index-based loop: marketplace.Installation is a 216-byte
		// struct (per gocritic rangeValCopy) and we only need
		// ins.Status. Pay the copy on each iteration is wasted
		// when the slice can be in the hundreds for popular extensions.
		for i := range rows {
			switch rows[i].Status {
			case marketplace.InstallStatusActive:
				row.ActiveCount++
			case marketplace.InstallStatusDisabled:
				row.DisabledCount++
			case marketplace.InstallStatusFailed:
				row.FailedCount++
			}
			stats.ByStatus[string(rows[i].Status)]++
		}
		stats.TotalInstalls += row.Installs
		stats.ByVersion = append(stats.ByVersion, row)
	}
	writeJSON(w, http.StatusOK, stats)
}

// --- helpers -----------------------------------------------------

// requireDashboardMember consolidates the parse-publisher-id +
// resolve-user + RequireMemberRole(member) idiom shared by every
// dashboard handler. Returns (pubID, true) on success; on failure
// it has already written the response.
func (h *marketplaceHandlers) requireDashboardMember(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return uuid.Nil, false
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return uuid.Nil, false
	}
	if _, err := h.store.Publishers().RequireMemberRole(
		r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember,
	); err != nil {
		writeNotFoundOrError(w, h, err)
		return uuid.Nil, false
	}
	return pubID, true
}

// requirePublisherOwnsExtension validates that the extension
// belongs to the publisher. Returns the extension row on success;
// returns marketplace.ErrNotFound if the publisher slug does not
// match (so requireDashboardMember's 404-collapse handler can
// hide existence).
func (h *marketplaceHandlers) requirePublisherOwnsExtension(
	ctx context.Context, pubID, extID uuid.UUID,
) (*marketplace.Extension, error) {
	pub, err := h.store.Publishers().GetPublisher(ctx, pubID)
	if err != nil {
		return nil, err
	}
	ext, err := h.store.GetExtension(ctx, extID)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(ext.Publisher, pub.Slug) {
		return nil, marketplace.ErrNotFound
	}
	return ext, nil
}

// requireExtensionOwnsVersion validates that the version row's
// extension_id matches the URL ext_id. Prevents a publisher from
// viewing review state on a version that doesn't belong to one
// of their extensions.
func (h *marketplaceHandlers) requireExtensionOwnsVersion(
	ctx context.Context, extID, verID uuid.UUID,
) error {
	v, err := h.store.GetVersion(ctx, verID)
	if err != nil {
		return err
	}
	if v.ExtensionID != extID {
		return marketplace.ErrNotFound
	}
	return nil
}

// parseExtIDParam parses the chi {ext_id} URL param. On failure
// it writes the 400 directly.
func parseExtIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "ext_id"))
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid ext_id: %v", err), http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

// parseVerIDParam parses the chi {ver_id} URL param.
func parseVerIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "ver_id"))
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid ver_id: %v", err), http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}
