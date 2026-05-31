package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Phase B7.1 — self-service publisher surface.
//
// B7 (#131) shipped admin-only publisher / key management. B7.1
// adds a tenant-chain surface that lets publisher members manage
// their own membership and ed25519 keys without operator
// involvement. The admin endpoints from B7 stay as the operator
// override (for bootstrapping the first owner and for forced
// recovery actions that bypass the ≥1-owner invariant).
//
// Surface:
//
// Self-service (mounts under tenantChain; per-publisher RBAC enforced
// by RequireMemberRole):
//   GET    /api/v1/publisher                              — list publishers I'm a member of
//   GET    /api/v1/publisher/{publisher_id}               — get one publisher (member only)
//   GET    /api/v1/publisher/{publisher_id}/members       — list members (member)
//   POST   /api/v1/publisher/{publisher_id}/members       — add member (owner)
//   PATCH  /api/v1/publisher/{publisher_id}/members/{user_id}  — change role (owner)
//   DELETE /api/v1/publisher/{publisher_id}/members/{user_id}  — remove (owner)
//   GET    /api/v1/publisher/{publisher_id}/keys          — list keys (member)
//   POST   /api/v1/publisher/{publisher_id}/keys          — register key (member)
//   POST   /api/v1/publisher/{publisher_id}/keys/{key_id}/revoke — revoke key (member)
//
// Admin bootstrap (mounts under adminChain — see
// marketplace_publisher_handlers.go for the rest of the admin
// surface):
//   POST   /api/v1/admin/marketplace/publishers/{publisher_id}/members
//   GET    /api/v1/admin/marketplace/publishers/{publisher_id}/members
//   DELETE /api/v1/admin/marketplace/publishers/{publisher_id}/members/{user_id}
//
// Why the same key-mutation endpoints live on BOTH the admin and
// the self-service chain rather than only the latter: B7's admin
// endpoints predate this work and are still useful as a recovery
// path (e.g. a publisher organisation loses access to all of its
// owner keys; the operator can register a new one without
// needing a membership row first). The self-service endpoints
// are the normal path; the admin ones are the override.

// --- Request / response shapes ----------------------------------

// publisherMemberView is the JSON shape for membership row
// responses. UserEmail + UserDisplayName are populated by the
// store via JOIN so the UI can render the row without a second
// round-trip to /users.
type publisherMemberView struct {
	PublisherID     uuid.UUID                       `json:"publisher_id"`
	UserID          uuid.UUID                       `json:"user_id"`
	Role            marketplace.PublisherMemberRole `json:"role"`
	AddedBy         *uuid.UUID                      `json:"added_by,omitempty"`
	CreatedAt       string                          `json:"created_at"`
	UpdatedAt       string                          `json:"updated_at"`
	UserEmail       string                          `json:"user_email,omitempty"`
	UserDisplayName string                          `json:"user_display_name,omitempty"`
}

func publisherMemberToView(m *marketplace.PublisherMember) publisherMemberView {
	return publisherMemberView{
		PublisherID:     m.PublisherID,
		UserID:          m.UserID,
		Role:            m.Role,
		AddedBy:         m.AddedBy,
		CreatedAt:       m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:       m.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UserEmail:       m.UserEmail,
		UserDisplayName: m.UserDisplayName,
	}
}

// publisherWithMembershipView pairs the publisher view shape
// with the caller's role on that publisher.
type publisherWithMembershipView struct {
	Publisher publisherView                   `json:"publisher"`
	Role      marketplace.PublisherMemberRole `json:"role"`
}

type addPublisherMemberRequest struct {
	UserID uuid.UUID                       `json:"user_id"`
	Role   marketplace.PublisherMemberRole `json:"role"`
}

type setPublisherMemberRoleRequest struct {
	Role marketplace.PublisherMemberRole `json:"role"`
}

// --- Self-service handlers --------------------------------------

// listMyPublishers — GET /api/v1/publisher.
//
// Returns the publishers the authenticated user is a member of,
// paired with their role. Empty list (not 404) when the user has
// no memberships — clients use this to decide whether to render
// the publisher dashboard at all.
func (h *marketplaceHandlers) listMyPublishers(w http.ResponseWriter, r *http.Request) {
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		// tenantChain stamps the user_id from claims; an empty
		// id here means the middleware was bypassed somehow
		// (e.g. test harness misconfiguration). Refuse rather
		// than silently returning the empty list a non-member
		// would see.
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	rows, err := h.store.Publishers().ListPublishersForUser(r.Context(), userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherWithMembershipView, 0, len(rows))
	for i := range rows {
		out = append(out, publisherWithMembershipView{
			Publisher: publisherToView(&rows[i].Publisher),
			Role:      rows[i].Role,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// getMyPublisher — GET /api/v1/publisher/{publisher_id}.
//
// Member-only read of one publisher row. Returns the full admin
// view (verified_by, verification_notes, auto_approve_patch)
// because publisher members manage the entity and need the same
// detail the admin sees. Non-members get 404 (not 403) to avoid
// leaking publisher existence.
func (h *marketplaceHandlers) getMyPublisher(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	pub, err := h.store.Publishers().GetPublisher(r.Context(), pubID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publisherToView(pub))
}

// listMyPublisherMembers — GET /api/v1/publisher/{publisher_id}/members.
//
// Lists every member of the publisher with their role + joined
// user identity. Member-level access (any member can see who
// else has access).
func (h *marketplaceHandlers) listMyPublisherMembers(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	members, err := h.store.Publishers().ListMembers(r.Context(), pubID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherMemberView, 0, len(members))
	for i := range members {
		out = append(out, publisherMemberToView(&members[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// addMyPublisherMember — POST /api/v1/publisher/{publisher_id}/members.
//
// Owner-only. Adds a (user, role) row to the publisher's
// membership. The added_by column captures the inviting owner
// for audit. ErrConflict if the user is already a member — the
// caller should use PATCH …/members/{user_id} to change role
// instead.
func (h *marketplaceHandlers) addMyPublisherMember(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleOwner); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	var req addPublisherMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.UserID == uuid.Nil {
		http.Error(w, "user_id required", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = marketplace.PublisherMemberRoleMember
	}
	if !req.Role.Valid() {
		http.Error(w, "role must be 'owner' or 'member'", http.StatusBadRequest)
		return
	}
	m, err := h.store.Publishers().AddMember(r.Context(), marketplace.AddPublisherMemberInput{
		PublisherID: pubID,
		UserID:      req.UserID,
		Role:        req.Role,
		AddedBy:     userID,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publisherMemberToView(m))
}

// setMyPublisherMemberRole — PATCH /api/v1/publisher/{publisher_id}/members/{user_id}.
//
// Owner-only. Changes an existing member's role.
// ErrLastOwnerRemoval (409) if demoting the last owner would
// leave other members behind. Self-demotion is allowed as long
// as the invariant holds.
func (h *marketplaceHandlers) setMyPublisherMemberRole(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	targetID, err := uuid.Parse(chi.URLParam(r, "user_id"))
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleOwner); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	var req setPublisherMemberRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !req.Role.Valid() {
		http.Error(w, "role must be 'owner' or 'member'", http.StatusBadRequest)
		return
	}
	m, err := h.store.Publishers().SetMemberRole(r.Context(), marketplace.SetMemberRoleInput{
		PublisherID: pubID,
		UserID:      targetID,
		NewRole:     req.Role,
		// Self-service surface NEVER bypasses the
		// last-owner-removal invariant. The admin surface has
		// its own endpoint with AllowLastOwnerDemotion=true.
		AllowLastOwnerDemotion: false,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publisherMemberToView(m))
}

// removeMyPublisherMember — DELETE /api/v1/publisher/{publisher_id}/members/{user_id}.
//
// Owner-only. Removes a (user, role) row. Refuses (409) if
// removing the row would leave other members behind without an
// owner. Removing the sole remaining member (any role) is
// allowed — the publisher reverts to admin-only management.
func (h *marketplaceHandlers) removeMyPublisherMember(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	targetID, err := uuid.Parse(chi.URLParam(r, "user_id"))
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleOwner); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	if err := h.store.Publishers().RemoveMember(r.Context(), marketplace.RemoveMemberInput{
		PublisherID:           pubID,
		UserID:                targetID,
		AllowLastOwnerRemoval: false,
	}); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listMyPublisherKeys — GET /api/v1/publisher/{publisher_id}/keys.
//
// Member-level. Returns every key (including revoked) so the UI
// can render rotation history. The pipeline reads only
// non-revoked keys via ListPublisherKeys(includeRevoked=false);
// the membership surface returns the full set for transparency.
func (h *marketplaceHandlers) listMyPublisherKeys(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember); err != nil {
		writeNotFoundOrError(w, h, err)
		return
	}
	keys, err := h.store.Publishers().ListPublisherKeys(r.Context(), pubID, true)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherKeyView, 0, len(keys))
	for i := range keys {
		out = append(out, publisherKeyToView(&keys[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// registerMyPublisherKey — POST /api/v1/publisher/{publisher_id}/keys.
//
// Member-level. Registers a new ed25519 public key under the
// publisher. The (publisher_id, key_id) pair is UNIQUE; the
// store returns ErrConflict on duplicate.
func (h *marketplaceHandlers) registerMyPublisherKey(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember); err != nil {
		writeNotFoundOrError(w, h, err)
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

// revokeMyPublisherKey — POST /api/v1/publisher/{publisher_id}/keys/{key_id}/revoke.
//
// Member-level. Marks a key revoked. Same defense-in-depth
// check as the admin endpoint: the key_id from the URL path must
// match the publisher_id, so a typo cannot revoke a key on a
// different publisher.
func (h *marketplaceHandlers) revokeMyPublisherKey(w http.ResponseWriter, r *http.Request) {
	pubID, ok := parsePublisherIDParam(w, r)
	if !ok {
		return
	}
	keyID, err := uuid.Parse(chi.URLParam(r, "key_id"))
	if err != nil {
		http.Error(w, "invalid key_id", http.StatusBadRequest)
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "user identity required", http.StatusUnauthorized)
		return
	}
	if _, err := h.store.Publishers().RequireMemberRole(r.Context(), pubID, userID, marketplace.PublisherMemberRoleMember); err != nil {
		writeNotFoundOrError(w, h, err)
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
		// Same wording the admin endpoint uses so the test
		// matrix can assert the wrapped sentinel without
		// branching on caller type.
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

// --- Admin bootstrap handlers -----------------------------------

// adminAddPublisherMember — POST /api/v1/admin/marketplace/publishers/{publisher_id}/members.
//
// Operator-only. Used to seed the first owner of a new publisher
// (or to forcibly add a member during recovery). The added_by
// column is NULL because the admin is acting on behalf of the
// platform, not as a publisher member themselves.
func (h *marketplaceHandlers) adminAddPublisherMember(w http.ResponseWriter, r *http.Request) {
	pubID, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	var req addPublisherMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.UserID == uuid.Nil {
		http.Error(w, "user_id required", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = marketplace.PublisherMemberRoleOwner
	}
	if !req.Role.Valid() {
		http.Error(w, "role must be 'owner' or 'member'", http.StatusBadRequest)
		return
	}
	m, err := h.store.Publishers().AddMember(r.Context(), marketplace.AddPublisherMemberInput{
		PublisherID: pubID,
		UserID:      req.UserID,
		Role:        req.Role,
		// AddedBy intentionally uuid.Nil — the admin acts on
		// behalf of the platform, not as a publisher member.
		// The audit trail for the operator's action lives in
		// the platform-side audit log, not the membership row.
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publisherMemberToView(m))
}

// adminListPublisherMembers — GET /api/v1/admin/marketplace/publishers/{publisher_id}/members.
//
// Operator-only. Same shape as the self-service endpoint but
// reachable without a membership row.
func (h *marketplaceHandlers) adminListPublisherMembers(w http.ResponseWriter, r *http.Request) {
	pubID, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	members, err := h.store.Publishers().ListMembers(r.Context(), pubID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]publisherMemberView, 0, len(members))
	for i := range members {
		out = append(out, publisherMemberToView(&members[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// adminRemovePublisherMember — DELETE /api/v1/admin/marketplace/publishers/{publisher_id}/members/{user_id}.
//
// Operator-only. Forcible member removal that bypasses the
// last-owner-removal invariant (i.e. the operator can remove the
// last owner, leaving other members behind without an owner —
// the publisher remains admin-managed afterwards). Used for
// recovery when an owner is no longer available.
func (h *marketplaceHandlers) adminRemovePublisherMember(w http.ResponseWriter, r *http.Request) {
	pubID, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return
	}
	targetID, err := uuid.Parse(chi.URLParam(r, "user_id"))
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	if err := h.store.Publishers().RemoveMember(r.Context(), marketplace.RemoveMemberInput{
		PublisherID:           pubID,
		UserID:                targetID,
		AllowLastOwnerRemoval: true,
	}); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Helpers ----------------------------------------------------

// parsePublisherIDParam parses chi's "publisher_id" URL param.
// On parse failure it writes the 400 directly and returns
// (uuid.Nil, false) so the caller can early-return without
// repeating the http.Error idiom.
func parsePublisherIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "publisher_id"))
	if err != nil {
		http.Error(w, "invalid publisher_id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

// writeNotFoundOrError collapses RequireMemberRole's ErrForbidden
// + ErrNotFound into 404 on the self-service surface. The
// rationale: a non-member trying to enumerate publishers should
// not be able to distinguish "publisher exists but I'm not a
// member" from "publisher doesn't exist" — otherwise the
// surface leaks publisher existence to anyone with a valid JWT.
// Other engine errors fall through to writeError unchanged.
//
// The admin surface DOES leak existence (404 vs 403) because the
// operator already sees the full catalog via
// adminListPublishers; there is nothing to hide.
func writeNotFoundOrError(w http.ResponseWriter, h *marketplaceHandlers, err error) {
	if errors.Is(err, marketplace.ErrForbidden) || errors.Is(err, marketplace.ErrNotFound) {
		http.Error(w, marketplace.ErrNotFound.Error(), http.StatusNotFound)
		return
	}
	h.writeError(w, err)
}
