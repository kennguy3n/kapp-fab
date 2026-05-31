package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// Phase B7 endpoints. The B7 publisher / findings surface lives
// almost entirely on the admin chain — v1 does not model
// per-tenant publisher membership, so key registration cannot be
// safely exposed to publishers themselves without an admin step.
// A future B7.1 will add a marketplace_publisher_members join
// table; once that lands, RegisterPublisherKey / RevokePublisherKey
// migrate to a tenant-scoped self-service surface and the admin
// endpoints stay as the operator override.
//
// Surface:
//
//	Admin (operator surface, cross-tenant; mounts under adminChain):
//	  POST   /api/v1/admin/marketplace/publishers
//	  GET    /api/v1/admin/marketplace/publishers
//	  GET    /api/v1/admin/marketplace/publishers/{publisher_id}
//	  POST   /api/v1/admin/marketplace/publishers/{publisher_id}/verify
//	  POST   /api/v1/admin/marketplace/publishers/{publisher_id}/unverify
//	  POST   /api/v1/admin/marketplace/publishers/{publisher_id}/keys
//	  DELETE /api/v1/admin/marketplace/publishers/{publisher_id}/keys/{key_id}
//	  GET    /api/v1/admin/marketplace/versions/{ver_id}/findings
//	  POST   /api/v1/admin/marketplace/versions/{ver_id}/rescan
//
//	Browse (tenant chain; read-only):
//	  GET    /api/v1/marketplace/publishers/{slug}
//
// The browse endpoint exposes only the publisher's public
// identity + verification posture; key material is admin-only.

// publisherView is the JSON shape for the admin publisher
// endpoints. Distinct from the storage type so future schema
// additions don't leak into the public API contract.
type publisherView struct {
	ID                uuid.UUID `json:"id"`
	Slug              string    `json:"slug"`
	DisplayName       string    `json:"display_name"`
	ContactEmail      string    `json:"contact_email"`
	VerifiedAt        string    `json:"verified_at,omitempty"`
	VerifiedBy        string    `json:"verified_by,omitempty"`
	VerificationNotes string    `json:"verification_notes,omitempty"`
	AutoApprovePatch  bool      `json:"auto_approve_patch"`
	CreatedAt         string    `json:"created_at"`
	UpdatedAt         string    `json:"updated_at"`
}

// publisherPublicView is the JSON shape for the tenant-browse
// publisher endpoint. Strips operator-only fields (verified_by,
// notes, auto_approve_patch) and replaces the key set with a
// boolean signal so a tenant can know whether to expect signed
// uploads from this publisher without seeing key material.
type publisherPublicView struct {
	ID          uuid.UUID `json:"id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	Verified    bool      `json:"verified"`
	HasKeys     bool      `json:"has_keys"`
}

func publisherToView(p *marketplace.Publisher) publisherView {
	v := publisherView{
		ID:                p.ID,
		Slug:              p.Slug,
		DisplayName:       p.DisplayName,
		ContactEmail:      p.ContactEmail,
		VerifiedBy:        p.VerifiedBy,
		VerificationNotes: p.VerificationNotes,
		AutoApprovePatch:  p.AutoApprovePatch,
		CreatedAt:         p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:         p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if p.VerifiedAt != nil {
		v.VerifiedAt = p.VerifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return v
}

func publisherToPublicView(p *marketplace.Publisher, hasKeys bool) publisherPublicView {
	return publisherPublicView{
		ID:          p.ID,
		Slug:        p.Slug,
		DisplayName: p.DisplayName,
		Verified:    p.VerifiedAt != nil,
		HasKeys:     hasKeys,
	}
}

// publisherKeyView is the JSON shape for publisher-key listings.
// The public_key_b64 field is intentionally surfaced (it's a
// public key — that's the point); the algorithm is included so a
// future PGP rollout can be distinguished without a re-fetch.
type publisherKeyView struct {
	ID            uuid.UUID `json:"id"`
	PublisherID   uuid.UUID `json:"publisher_id"`
	KeyID         string    `json:"key_id"`
	Algorithm     string    `json:"algorithm"`
	PublicKeyB64  string    `json:"public_key_b64"`
	Label         string    `json:"label,omitempty"`
	RevokedAt     string    `json:"revoked_at,omitempty"`
	RevokedReason string    `json:"revoked_reason,omitempty"`
	CreatedAt     string    `json:"created_at"`
}

func publisherKeyToView(k *marketplace.PublisherKey) publisherKeyView {
	v := publisherKeyView{
		ID:            k.ID,
		PublisherID:   k.PublisherID,
		KeyID:         k.KeyID,
		Algorithm:     k.Algorithm,
		PublicKeyB64:  k.PublicKeyB64,
		Label:         k.Label,
		RevokedReason: k.RevokedReason,
		CreatedAt:     k.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if k.RevokedAt != nil {
		v.RevokedAt = k.RevokedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return v
}

// reviewFindingView is the JSON shape for finding listings. The
// severity field is the string form so JSON consumers don't need
// to import the marketplace package's Severity type.
type reviewFindingView struct {
	ID                 uuid.UUID `json:"id"`
	ExtensionVersionID uuid.UUID `json:"extension_version_id"`
	CheckName          string    `json:"check_name"`
	Severity           string    `json:"severity"`
	Code               string    `json:"code"`
	Message            string    `json:"message"`
	Location           string    `json:"location,omitempty"`
	CreatedAt          string    `json:"created_at"`
}

func findingToView(f *marketplace.ReviewFinding) reviewFindingView {
	return reviewFindingView{
		ID:                 f.ID,
		ExtensionVersionID: f.ExtensionVersionID,
		CheckName:          f.CheckName,
		Severity:           string(f.Severity),
		Code:               f.Code,
		Message:            f.Message,
		Location:           f.Location,
		CreatedAt:          f.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// --- Request bodies ---

type createPublisherRequest struct {
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	ContactEmail string `json:"contact_email"`
}

type registerPublisherKeyRequest struct {
	KeyID        string `json:"key_id"`
	PublicKeyB64 string `json:"public_key_b64"`
	Label        string `json:"label,omitempty"`
}

type revokePublisherKeyRequest struct {
	Reason string `json:"reason"`
}

type verifyPublisherRequest struct {
	Notes            string `json:"notes,omitempty"`
	AutoApprovePatch bool   `json:"auto_approve_patch,omitempty"`
}

// --- Tenant browse handler ---

// getPublisherPublic is the read-only browse endpoint for the
// publisher's public identity. Used by the catalog UI to display
// a "Verified Publisher" badge next to an extension listing.
// Returns 404 if the slug is not registered. The has_keys boolean
// is true when the publisher has any non-revoked ed25519 key — a
// tenant can use that to decide whether to expect a signed bundle.
func (h *marketplaceHandlers) getPublisherPublic(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}
	pub, err := h.store.Publishers().GetPublisherBySlug(r.Context(), slug)
	if err != nil {
		h.writeError(w, err)
		return
	}
	keys, err := h.store.Publishers().ListPublisherKeys(r.Context(), pub.ID, false)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publisherToPublicView(pub, len(keys) > 0))
}

// --- Admin handlers ---

// adminCreatePublisher inserts a new publisher row.
func (h *marketplaceHandlers) adminCreatePublisher(w http.ResponseWriter, r *http.Request) {
	var req createPublisherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Slug == "" || req.DisplayName == "" {
		http.Error(w, "slug and display_name required", http.StatusBadRequest)
		return
	}
	pub, err := h.store.Publishers().CreatePublisher(r.Context(), marketplace.CreatePublisherInput{
		Slug:         req.Slug,
		DisplayName:  req.DisplayName,
		ContactEmail: req.ContactEmail,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publisherToView(pub))
}

// adminListPublishers returns every publisher row, ordered by
// slug. Used by the admin UI's publisher index.
func (h *marketplaceHandlers) adminListPublishers(w http.ResponseWriter, r *http.Request) {
	pubs, err := h.store.Publishers().ListPublishers(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherView, 0, len(pubs))
	for i := range pubs {
		out = append(out, publisherToView(&pubs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// adminGetPublisher returns the publisher row including its
// non-revoked key set. Used by the admin verification flow.
func (h *marketplaceHandlers) adminGetPublisher(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	pub, err := h.store.Publishers().GetPublisher(r.Context(), id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	keys, err := h.store.Publishers().ListPublisherKeys(r.Context(), id, true)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherKeyView, 0, len(keys))
	for i := range keys {
		out = append(out, publisherKeyToView(&keys[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"publisher": publisherToView(pub),
		"keys":      out,
	})
}

// adminVerifyPublisher stamps the publisher's verified_at /
// verified_by columns. The reviewer is taken from the auth
// context.
func (h *marketplaceHandlers) adminVerifyPublisher(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	var req verifyPublisherRequest
	// Both fields are optional — body MAY be empty. Treat io.EOF
	// (zero-byte body) as the empty-body case; surface every
	// other JSON parse error.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	reviewer := actorOrDefault(r.Context()).String()
	pub, err := h.store.Publishers().VerifyPublisher(r.Context(), marketplace.VerifyPublisherInput{
		PublisherID:      id,
		Reviewer:         reviewer,
		Notes:            req.Notes,
		AutoApprovePatch: req.AutoApprovePatch,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publisherToView(pub))
}

// adminUnverifyPublisher clears the publisher's verified_at /
// verified_by columns. The CHECK on auto_approve_requires_verified
// force-clears auto_approve_patch alongside.
func (h *marketplaceHandlers) adminUnverifyPublisher(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	if err := h.store.Publishers().UnverifyPublisher(r.Context(), id); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminRegisterPublisherKey associates a new ed25519 public key
// with a publisher row. The (publisher_id, key_id) pair is
// UNIQUE; re-registering with a fresh key_id is the normal
// rotation path.
func (h *marketplaceHandlers) adminRegisterPublisherKey(w http.ResponseWriter, r *http.Request) {
	pubID, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	var req registerPublisherKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.KeyID == "" || req.PublicKeyB64 == "" {
		http.Error(w, "key_id and public_key_b64 required", http.StatusBadRequest)
		return
	}
	key, err := h.store.Publishers().RegisterPublisherKey(r.Context(), marketplace.RegisterPublisherKeyInput{
		PublisherID:  pubID,
		KeyID:        req.KeyID,
		PublicKeyB64: req.PublicKeyB64,
		Label:        req.Label,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publisherKeyToView(key))
}

// adminRevokePublisherKey marks a key revoked. Defense-in-depth:
// verifies the key is owned by the publisher_id from the URL
// path before revoking, so a typo'd key_id can't accidentally
// revoke a key on a different publisher.
func (h *marketplaceHandlers) adminRevokePublisherKey(w http.ResponseWriter, r *http.Request) {
	pubID, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	keyID, err := uuid.Parse(chi.URLParam(r, "key_id"))
	if err != nil {
		http.Error(w, "invalid key_id", http.StatusBadRequest)
		return
	}
	var req revokePublisherKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		http.Error(w, "reason required", http.StatusBadRequest)
		return
	}
	key, err := h.store.Publishers().GetPublisherKey(r.Context(), keyID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if key.PublisherID != pubID {
		h.writeError(w, fmt.Errorf("%w: key %s does not belong to publisher %s",
			marketplace.ErrNotFound, keyID, pubID))
		return
	}
	if err := h.store.Publishers().RevokePublisherKey(r.Context(), keyID, req.Reason); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminListFindings returns every structured finding for a
// version, ordered (check, code, location).
func (h *marketplaceHandlers) adminListFindings(w http.ResponseWriter, r *http.Request) {
	verID, err := uuid.Parse(chi.URLParam(r, "ver_id"))
	if err != nil {
		http.Error(w, "invalid ver_id", http.StatusBadRequest)
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

// adminRescanVersion re-claims a version for the review worker
// by re-setting its review state to `submitted`. Used when an
// operator wants to re-run the pipeline against an existing
// version (e.g. after fixing a check). Refused on terminal-state
// rows — once approved / rejected / withdrawn, a version is
// immutable, and re-running the pipeline would void the audit
// trail. Publishers re-submit by uploading a new version.
//
// This is NOT a synchronous rescan: it nudges the queue so the
// next worker poll picks the version up. The operator polls
// /api/v1/admin/marketplace/versions/{ver_id}/findings to see the
// updated result. A synchronous variant is deferred — the
// worker's per-version timeout (90 s) is too long to hold an
// HTTP request open, and a transient CDN failure shouldn't
// bubble into a 5xx on the admin surface.
func (h *marketplaceHandlers) adminRescanVersion(w http.ResponseWriter, r *http.Request) {
	verID, err := uuid.Parse(chi.URLParam(r, "ver_id"))
	if err != nil {
		http.Error(w, "invalid ver_id", http.StatusBadRequest)
		return
	}
	state, err := h.store.Reviews().GetReviewState(r.Context(), verID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if state.Status.IsTerminal() {
		http.Error(w, "cannot rescan a version in terminal review state",
			http.StatusConflict)
		return
	}
	// Transitions BACK to submitted are not in the normal
	// reviewStatusTransitionAllowed graph (submitted is only an
	// entry state in the publisher → automated_passed → manual
	// flow). The admin rescan path bypasses the graph via a
	// dedicated Store.ResetReviewStateForRescan UPDATE that
	// also clears reviewer + reviewed_at + automated_checks so
	// the re-issued worker run starts from a clean slate.
	if err := h.store.ResetReviewStateForRescan(r.Context(), verID); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
