package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// workflowHandlers wires the workflow action endpoint. It is thin: the
// engine does the heavy lifting of validating transitions and writing
// history+events atomically. The handler's job is to (1) look up the
// run for the given record, starting one if the KType declares a
// workflow, and (2) translate errors into HTTP status codes the web
// client expects.
type workflowHandlers struct {
	engine   *workflow.Engine
	store    *record.PGStore
	registry *ktype.PGRegistry
}

type workflowActionResponse struct {
	Record *record.KRecord      `json:"record"`
	Run    *workflow.WorkflowRun `json:"run"`
}

// action handles POST /api/v1/records/{ktype}/{id}/actions/{action}.
// ARCHITECTURE.md §10 defines this as the canonical entry-point for
// triggering a workflow transition on an existing KRecord.
func (h *workflowHandlers) action(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	recordID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid record id", http.StatusBadRequest)
		return
	}
	action := chi.URLParam(r, "action")
	if action == "" {
		http.Error(w, "action required", http.StatusBadRequest)
		return
	}

	rec, err := h.store.Get(r.Context(), t.ID, recordID)
	if err != nil {
		writeRecordError(w, err)
		return
	}

	// Resolve the run. A record either already has one or we open a new
	// one if the KType declares a workflow. Opening-on-demand keeps the
	// API surface stable even for records created before a workflow was
	// attached to the KType.
	actor := actorOrDefault(r.Context())
	run, err := h.engine.GetRunByRecord(r.Context(), t.ID, recordID)
	if errors.Is(err, workflow.ErrRunNotFound) {
		workflowName, ok := h.workflowNameFor(r.Context(), rec)
		if !ok {
			http.Error(w, "no workflow attached to ktype", http.StatusBadRequest)
			return
		}
		run, err = h.engine.StartRun(r.Context(), t.ID, workflowName, recordID, "", actor)
	}
	if err != nil {
		writeWorkflowError(w, err)
		return
	}

	run, err = h.engine.Transition(r.Context(), t.ID, run.ID, action, actor)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}

	// Re-fetch the record so the client sees any mutations written by
	// post-transition hooks (sales invoice creation, etc.). Hooks live
	// in the worker service; in Phase B they may not have run yet when
	// the handler returns — the record is whatever the store has now.
	rec, err = h.store.Get(r.Context(), t.ID, recordID)
	if err != nil {
		writeRecordError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowActionResponse{Record: rec, Run: run})
}

// getRunByRecord handles GET /api/v1/records/{ktype}/{id}/workflow-run.
// The RightPane hydrates from this endpoint so it can render the
// authoritative engine state plus the full transition history timeline
// without inferring state from the record.data payload.
func (h *workflowHandlers) getRunByRecord(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	recordID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid record id", http.StatusBadRequest)
		return
	}
	run, err := h.engine.GetRunByRecord(r.Context(), t.ID, recordID)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// workflowNameFor pulls the workflow.name attribute out of a KType
// schema payload. It peeks at a small subset of the schema JSON because
// ktype.Schema (the validator-facing struct) does not expose the
// workflow block; a fuller schema parser lives in ARCHITECTURE but is
// deferred until Phase C.
//
// Returns ("", false) when the record has no registry-backed KType or
// the KType schema omits a workflow block.
func (h *workflowHandlers) workflowNameFor(ctx context.Context, rec *record.KRecord) (string, bool) {
	if h == nil || h.registry == nil || rec == nil || rec.KType == "" {
		return "", false
	}
	kt, err := h.registry.Get(ctx, rec.KType, 0)
	if err != nil || kt == nil {
		return "", false
	}
	var envelope struct {
		Workflow struct {
			Name string `json:"name"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(kt.Schema, &envelope); err != nil {
		return "", false
	}
	if envelope.Workflow.Name == "" {
		return "", false
	}
	return envelope.Workflow.Name, true
}

func writeWorkflowError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workflow.ErrWorkflowNotFound),
		errors.Is(err, workflow.ErrRunNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, workflow.ErrInvalidTransition),
		errors.Is(err, workflow.ErrTransitionFromWrong),
		errors.Is(err, workflow.ErrInvalidDefinition),
		errors.Is(err, workflow.ErrDuplicateRun),
		errors.Is(err, workflow.ErrActorNotFound):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
