package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// helpdeskHandlers exposes SLA policy CRUD and ticket SLA-log read
// endpoints under /api/v1/helpdesk. Tickets themselves live behind the
// generic /api/v1/records/helpdesk.ticket surface.
type helpdeskHandlers struct {
	store *helpdesk.Store
}

type upsertPolicyRequest struct {
	ID                 uuid.UUID `json:"id,omitempty"`
	Name               string    `json:"name"`
	Priority           string    `json:"priority"`
	ResponseMinutes    int       `json:"response_minutes"`
	ResolutionMinutes  int       `json:"resolution_minutes"`
	Active             *bool     `json:"active,omitempty"`
}

func (h *helpdeskHandlers) upsertPolicy(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req upsertPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	actor := actorOrDefault(r.Context())
	policy, err := h.store.UpsertPolicy(r.Context(), helpdesk.SLAPolicy{
		TenantID:          t.ID,
		ID:                req.ID,
		Name:              req.Name,
		Priority:          req.Priority,
		ResponseMinutes:   req.ResponseMinutes,
		ResolutionMinutes: req.ResolutionMinutes,
		Active:            active,
		CreatedBy:         &actor,
	})
	if err != nil {
		writeHelpdeskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (h *helpdeskHandlers) listPolicies(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	policies, err := h.store.ListPolicies(r.Context(), t.ID)
	if err != nil {
		writeHelpdeskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

func (h *helpdeskHandlers) resolvePolicy(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	priority := r.URL.Query().Get("priority")
	if priority == "" {
		http.Error(w, "priority required", http.StatusBadRequest)
		return
	}
	policy, err := h.store.ResolvePolicy(r.Context(), t.ID, priority)
	if err != nil {
		writeHelpdeskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (h *helpdeskHandlers) ticketLog(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid ticket id", http.StatusBadRequest)
		return
	}
	entries, err := h.store.ListTicketLog(r.Context(), t.ID, id)
	if err != nil {
		writeHelpdeskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func writeHelpdeskError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, helpdesk.ErrPolicyNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
