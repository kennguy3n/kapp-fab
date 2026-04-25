package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// retentionHandlers exposes the per-tenant data retention surface.
//
//	GET /api/v1/tenants/{id}/retention  → list policies
//	PUT /api/v1/tenants/{id}/retention  → upsert one policy
//
// PUT body shape: {category, retention_days, enabled}. The tenant
// id comes from the URL so the operator has explicit control over
// which tenant the policy targets — no JWT-derived implicit tenant.
type retentionHandlers struct {
	store *platform.RetentionStore
}

func (h *retentionHandlers) list(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil {
		http.Error(w, "retention store unconfigured", http.StatusServiceUnavailable)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	policies, err := h.store.List(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

func (h *retentionHandlers) put(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil {
		http.Error(w, "retention store unconfigured", http.StatusServiceUnavailable)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	var body struct {
		Category      string `json:"category"`
		RetentionDays int    `json:"retention_days"`
		Enabled       *bool  `json:"enabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	policy, err := h.store.Upsert(r.Context(), platform.RetentionPolicy{
		TenantID:      id,
		Category:      body.Category,
		RetentionDays: body.RetentionDays,
		Enabled:       enabled,
	})
	if err != nil {
		// All errors from RetentionStore.Upsert are validation /
		// missing-id failures; surface them as 400.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

// isolationAuditHandlers wraps the IsolationAuditor for the
// /api/v1/admin/isolation-audit endpoint.
type isolationAuditHandlers struct {
	auditor *platform.IsolationAuditor
}

func (h *isolationAuditHandlers) get(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.auditor == nil {
		http.Error(w, "isolation auditor unconfigured", http.StatusServiceUnavailable)
		return
	}
	report, err := h.auditor.Run(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if !report.Passed {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, report)
}
