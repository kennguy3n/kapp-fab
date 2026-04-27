package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// platformTenant is a local alias so handlers can reference the
// tenant struct without importing internal/tenant alongside platform.
type platformTenant = tenant.Tenant

// insightsDataSourceHandlers wires CRUD + connection-test under
// /api/v1/insights/data-sources. Every handler is gated by the
// insights_external feature flag because external connections cost
// real CPU + bandwidth on the operator's backend infrastructure.
type insightsDataSourceHandlers struct {
	store    *insights.DataSourceStore
	pools    *insights.PoolManager
	features *tenant.FeatureStore
}

// dataSourceRequest is the wire format for create/update. Connection
// string and secret blob default to empty so an update can patch
// metadata without re-sending credentials (the store keeps the
// existing value when both are empty).
type dataSourceRequest struct {
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	Dialect          string `json:"dialect"`
	ConnectionString string `json:"connection_string,omitempty"`
	SecretBlob       string `json:"secret_blob,omitempty"`
	Enabled          *bool  `json:"enabled,omitempty"`
}

func (h *insightsDataSourceHandlers) requireFeature(w http.ResponseWriter, r *http.Request, t *platformTenant) bool {
	if h.features == nil {
		return true
	}
	enabled, err := h.features.IsEnabled(r.Context(), t.ID, tenant.FeatureInsightsExternal)
	if err != nil {
		http.Error(w, "feature lookup failed", http.StatusInternalServerError)
		return false
	}
	if !enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "insights_external feature disabled"})
		return false
	}
	return true
}

func (h *insightsDataSourceHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	sources, err := h.store.List(r.Context(), t.ID)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data_sources": sources})
}

func (h *insightsDataSourceHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	var req dataSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	actor := actorOrDefault(r.Context())
	out, err := h.store.Create(r.Context(), insights.DataSource{
		TenantID:         t.ID,
		Name:             req.Name,
		Description:      req.Description,
		Dialect:          req.Dialect,
		ConnectionString: req.ConnectionString,
		SecretBlob:       req.SecretBlob,
		Enabled:          enabled,
		CreatedBy:        &actor,
	})
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *insightsDataSourceHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid data source id", http.StatusBadRequest)
		return
	}
	var req dataSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	out, err := h.store.Update(r.Context(), insights.DataSource{
		TenantID:         t.ID,
		ID:               id,
		Name:             req.Name,
		Description:      req.Description,
		Dialect:          req.Dialect,
		ConnectionString: req.ConnectionString,
		SecretBlob:       req.SecretBlob,
		Enabled:          enabled,
	})
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *insightsDataSourceHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid data source id", http.StatusBadRequest)
		return
	}
	if err := h.store.Delete(r.Context(), t.ID, id); err != nil {
		writeInsightsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// connectionTestTimeout caps each /test call. Five seconds keeps the
// UI responsive without dropping legitimate slow handshakes.
const connectionTestTimeout = 5 * time.Second

// test pings the remote connection by issuing a simple SELECT 1.
// Surfaces (200 ok / 502 unreachable / 401 auth fail) so the UI can
// distinguish between a typo and a credential issue.
func (h *insightsDataSourceHandlers) test(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid data source id", http.StatusBadRequest)
		return
	}
	ds, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), connectionTestTimeout)
	defer cancel()
	pool, err := h.pools.Get(ctx, t.ID, ds.ID, ds.ConnectionString, "")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}


