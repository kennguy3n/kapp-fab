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

// reportScheduleHandlers exposes CRUD for report_schedules under
// /api/v1/report-schedules. Scheduling cadence is enforced by the
// worker; this surface only persists the configuration.
type reportScheduleHandlers struct {
	store *reporting.ScheduleStore
}

type reportScheduleRequest struct {
	ReportID       uuid.UUID `json:"report_id"`
	Name           string    `json:"name"`
	CronExpression string    `json:"cron_expression"`
	Format         string    `json:"format"`
	Recipients     []string  `json:"recipients"`
	Enabled        bool      `json:"enabled"`
}

func (h *reportScheduleHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req reportScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	out, err := h.store.Create(r.Context(), reporting.ReportSchedule{
		TenantID:       t.ID,
		ReportID:       req.ReportID,
		Name:           req.Name,
		CronExpression: req.CronExpression,
		Format:         req.Format,
		Recipients:     req.Recipients,
		Enabled:        req.Enabled,
		CreatedBy:      &actor,
	})
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *reportScheduleHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid schedule id", http.StatusBadRequest)
		return
	}
	var req reportScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.Update(r.Context(), reporting.ReportSchedule{
		TenantID:       t.ID,
		ID:             id,
		ReportID:       req.ReportID,
		Name:           req.Name,
		CronExpression: req.CronExpression,
		Format:         req.Format,
		Recipients:     req.Recipients,
		Enabled:        req.Enabled,
	})
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *reportScheduleHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.store.List(r.Context(), t.ID)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (h *reportScheduleHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid schedule id", http.StatusBadRequest)
		return
	}
	out, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *reportScheduleHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid schedule id", http.StatusBadRequest)
		return
	}
	if err := h.store.Delete(r.Context(), t.ID, id); err != nil {
		writeScheduleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeScheduleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, reporting.ErrScheduleNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, reporting.ErrScheduleInvalidInput):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
