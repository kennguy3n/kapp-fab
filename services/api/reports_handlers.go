package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// reportsHandlers exposes saved-report CRUD and ad-hoc/saved execution
// under /api/v1/reports. The runner validates every definition before
// touching the database so a bad definition fails fast without ever
// emitting SQL.
type reportsHandlers struct {
	store  *reporting.Store
	runner *reporting.Runner
}

type createReportRequest struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Definition  reporting.Definition `json:"definition"`
}

func (h *reportsHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	saved, err := h.store.Create(r.Context(), reporting.SavedReport{
		TenantID:    t.ID,
		Name:        req.Name,
		Description: req.Description,
		Definition:  req.Definition,
		CreatedBy:   &actor,
	})
	if err != nil {
		writeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (h *reportsHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}
	var req createReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	saved, err := h.store.Update(r.Context(), reporting.SavedReport{
		TenantID:    t.ID,
		ID:          id,
		Name:        req.Name,
		Description: req.Description,
		Definition:  req.Definition,
	})
	if err != nil {
		writeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *reportsHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	reports, err := h.store.List(r.Context(), t.ID)
	if err != nil {
		writeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": reports})
}

func (h *reportsHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}
	report, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *reportsHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}
	if err := h.store.Delete(r.Context(), t.ID, id); err != nil {
		writeReportError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *reportsHandlers) runSaved(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}
	report, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeReportError(w, err)
		return
	}
	result, err := h.runner.Run(r.Context(), t.ID, report.Definition)
	if err != nil {
		writeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *reportsHandlers) runAdhoc(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var def reporting.Definition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	result, err := h.runner.Run(r.Context(), t.ID, def)
	if err != nil {
		writeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type shareReportRequest struct {
	Visibility string                 `json:"visibility"`
	SharedWith []reporting.ShareEntry `json:"shared_with"`
}

// share replaces the visibility + shared_with on a saved report.
// PATCH /api/v1/reports/{id}/share. Owner / admin enforcement lives
// in the middleware stack so this handler is purely a data write.
func (h *reportsHandlers) share(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}
	var req shareReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	saved, err := h.store.SetSharing(r.Context(), t.ID, id, req.Visibility, req.SharedWith)
	if err != nil {
		writeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func writeReportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, reporting.ErrReportNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
