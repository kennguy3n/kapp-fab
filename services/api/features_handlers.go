package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// featuresHandlers backs the control-plane feature-flag endpoints.
// Both routes are control-plane: they accept a {id} path parameter
// and read/write tenant_features directly under that tenant's RLS
// context via the store, not the request's TenantMiddleware.
type featuresHandlers struct {
	features *tenant.FeatureStore
	tenants  *tenant.PGStore
}

type featuresResponse struct {
	TenantID uuid.UUID       `json:"tenant_id"`
	Features map[string]bool `json:"features"`
}

type featuresUpdateRequest struct {
	Features map[string]bool `json:"features"`
}

func (h *featuresHandlers) list(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	if _, err := h.tenants.Get(r.Context(), id); err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			http.Error(w, "tenant not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	features, err := h.features.ListFeatures(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, featuresResponse{TenantID: id, Features: features})
}

func (h *featuresHandlers) update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	var req featuresUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.Features) == 0 {
		http.Error(w, "features body required", http.StatusBadRequest)
		return
	}
	if err := h.features.SetFeatures(r.Context(), id, req.Features); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	features, err := h.features.ListFeatures(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, featuresResponse{TenantID: id, Features: features})
}
