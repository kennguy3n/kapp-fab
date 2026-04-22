package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// approvalsHandlers exposes the Phase B approvals surface. The engine
// already owns tenant isolation and audit/event emission; the handlers
// only translate HTTP into engine calls and map sentinel errors to
// status codes the web client expects.
type approvalsHandlers struct {
	engine *workflow.Engine
	store  *record.PGStore
}

// list returns the pending approvals for which the calling actor sits
// on the current step. This drives the Approvals page in the web UI
// and /approve list in KChat.
func (h *approvalsHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	approvals, err := h.engine.ListPendingApprovals(r.Context(), t.ID, actor)
	if err != nil {
		writeApprovalError(w, err)
		return
	}
	// Always return a JSON array so the web client can render an empty
	// state without special-casing nil.
	if approvals == nil {
		approvals = []workflow.Approval{}
	}
	writeJSON(w, http.StatusOK, approvals)
}

// get returns a single approval by id.
func (h *approvalsHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid approval id", http.StatusBadRequest)
		return
	}
	approval, err := h.engine.GetApproval(r.Context(), t.ID, id)
	if err != nil {
		writeApprovalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approval)
}

type createApprovalRequest struct {
	RecordKType string                 `json:"record_ktype"`
	RecordID    uuid.UUID              `json:"record_id"`
	Chain       workflow.ApprovalChain `json:"chain"`
}

// create opens a new approval on a record. The chain is normalized by
// the engine so external callers cannot skip steps by setting
// current_step themselves.
func (h *approvalsHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	approval, err := h.engine.RequestApproval(
		r.Context(), t.ID, req.RecordKType, req.RecordID, req.Chain, actorOrDefault(r.Context()),
	)
	if err != nil {
		writeApprovalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, approval)
}

type decideApprovalRequest struct {
	Decision string `json:"decision"`
}

// decide records the calling actor's decision on the current step.
// Quorum / sequencing / finalization all happen inside the engine so
// this handler stays thin.
func (h *approvalsHandlers) decide(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid approval id", http.StatusBadRequest)
		return
	}
	var req decideApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	approval, err := h.engine.Decide(
		r.Context(), t.ID, id, req.Decision, actorOrDefault(r.Context()),
	)
	if err != nil {
		writeApprovalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approval)
}

func writeApprovalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workflow.ErrApprovalNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, workflow.ErrApprovalFinalized),
		errors.Is(err, workflow.ErrApprovalDuplicate):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, workflow.ErrApprovalNotAuthorized):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, workflow.ErrApprovalInvalidAction),
		errors.Is(err, workflow.ErrApprovalInvalidChain):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
