package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// insightsHandlers exposes Phase L BI surfaces under
// /api/v1/insights/. The runner enforces per-tenant statement_timeout
// + cache awareness so a saved-query replay never burns a full 30s
// HTTP slot on the same SELECT.
type insightsHandlers struct {
	queries    *insights.QueryStore
	dashboards *insights.DashboardStore
	runner     *insights.Runner
}

// ---------- Queries ----------

type insightsQueryRequest struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description,omitempty"`
	Definition  insights.QueryDefinition `json:"definition"`
	// Pointer so the JSON-decoder can distinguish "field omitted"
	// (nil → server applies the default 300s) from "0" (disable
	// caching). A plain int conflated the two and silently coerced
	// real-time queries back to 5-minute caching.
	CacheTTLSeconds *int `json:"cache_ttl_seconds,omitempty"`
}

func (h *insightsHandlers) listQueries(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	queries, err := h.queries.List(r.Context(), t.ID)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queries": queries})
}

func (h *insightsHandlers) createQuery(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req insightsQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	saved, err := h.queries.Create(r.Context(), insights.Query{
		TenantID:        t.ID,
		Name:            req.Name,
		Description:     req.Description,
		Definition:      req.Definition,
		CacheTTLSeconds: req.CacheTTLSeconds,
		CreatedBy:       &actor,
	})
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (h *insightsHandlers) getQuery(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid query id", http.StatusBadRequest)
		return
	}
	q, err := h.queries.Get(r.Context(), t.ID, id)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (h *insightsHandlers) updateQuery(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid query id", http.StatusBadRequest)
		return
	}
	var req insightsQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	saved, err := h.queries.Update(r.Context(), insights.Query{
		TenantID:        t.ID,
		ID:              id,
		Name:            req.Name,
		Description:     req.Description,
		Definition:      req.Definition,
		CacheTTLSeconds: req.CacheTTLSeconds,
	})
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *insightsHandlers) deleteQuery(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid query id", http.StatusBadRequest)
		return
	}
	if err := h.queries.Delete(r.Context(), t.ID, id); err != nil {
		writeInsightsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type runQueryRequest struct {
	FilterParams map[string]any `json:"filter_params,omitempty"`
	BypassCache  bool           `json:"bypass_cache,omitempty"`
}

func (h *insightsHandlers) runQuery(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid query id", http.StatusBadRequest)
		return
	}
	var req runQueryRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	out, err := h.runner.RunSaved(r.Context(), t.ID, id, req.FilterParams, req.BypassCache)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- Dashboards ----------

type insightsDashboardRequest struct {
	Name               string          `json:"name"`
	Description        string          `json:"description,omitempty"`
	Layout             json.RawMessage `json:"layout,omitempty"`
	AutoRefreshSeconds int             `json:"auto_refresh_seconds,omitempty"`
}

func (h *insightsHandlers) listDashboards(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	dashboards, err := h.dashboards.List(r.Context(), t.ID)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dashboards": dashboards})
}

func (h *insightsHandlers) createDashboard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req insightsDashboardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	saved, err := h.dashboards.Create(r.Context(), insights.Dashboard{
		TenantID:           t.ID,
		Name:               req.Name,
		Description:        req.Description,
		Layout:             req.Layout,
		AutoRefreshSeconds: req.AutoRefreshSeconds,
		CreatedBy:          &actor,
	})
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// getDashboard resolves the dashboard, lists its widgets, and runs every
// widget's saved query (cache-first) so the response is a bundled
// payload the frontend can render without a fan-out.
func (h *insightsHandlers) getDashboard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	d, err := h.dashboards.Get(r.Context(), t.ID, id)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	widgets, err := h.dashboards.ListWidgets(r.Context(), t.ID, id)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	d.Widgets = widgets

	results := make(map[string]*insights.RunResult, len(widgets))
	for _, w := range widgets {
		if w.QueryID == nil {
			continue
		}
		out, err := h.runner.RunSaved(r.Context(), t.ID, *w.QueryID, nil, false)
		if err != nil {
			// Per-widget failures must not poison the entire
			// dashboard payload — the frontend renders an
			// error state per widget instead.
			results[w.ID.String()] = nil
			continue
		}
		results[w.ID.String()] = out
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dashboard":      d,
		"widget_results": results,
	})
}

func (h *insightsHandlers) updateDashboard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	var req insightsDashboardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	saved, err := h.dashboards.Update(r.Context(), insights.Dashboard{
		TenantID:           t.ID,
		ID:                 id,
		Name:               req.Name,
		Description:        req.Description,
		Layout:             req.Layout,
		AutoRefreshSeconds: req.AutoRefreshSeconds,
	})
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *insightsHandlers) deleteDashboard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	if err := h.dashboards.Delete(r.Context(), t.ID, id); err != nil {
		writeInsightsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Dashboard widgets ----------

type widgetRequest struct {
	ID       *uuid.UUID      `json:"id,omitempty"`
	QueryID  *uuid.UUID      `json:"query_id,omitempty"`
	VizType  string          `json:"viz_type"`
	Position json.RawMessage `json:"position,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`
}

func (h *insightsHandlers) upsertWidget(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	dashboardID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	var req widgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	widget := insights.DashboardWidget{
		TenantID:    t.ID,
		DashboardID: dashboardID,
		QueryID:     req.QueryID,
		VizType:     req.VizType,
		Position:    req.Position,
		Config:      req.Config,
	}
	if req.ID != nil {
		widget.ID = *req.ID
	}
	saved, err := h.dashboards.UpsertWidget(r.Context(), widget)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *insightsHandlers) deleteWidget(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	dashboardID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	widgetID, err := uuid.Parse(chi.URLParam(r, "widgetID"))
	if err != nil {
		http.Error(w, "invalid widget id", http.StatusBadRequest)
		return
	}
	if err := h.dashboards.DeleteWidget(r.Context(), t.ID, dashboardID, widgetID); err != nil {
		writeInsightsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Sharing ----------

type shareRequest struct {
	GranteeType string `json:"grantee_type"`
	Grantee     string `json:"grantee"`
	Permission  string `json:"permission,omitempty"`
}

func (h *insightsHandlers) shareDashboard(w http.ResponseWriter, r *http.Request) {
	h.shareResource(w, r, insights.ResourceDashboard)
}

func (h *insightsHandlers) shareQuery(w http.ResponseWriter, r *http.Request) {
	h.shareResource(w, r, insights.ResourceQuery)
}

func (h *insightsHandlers) shareResource(w http.ResponseWriter, r *http.Request, resourceType string) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid resource id", http.StatusBadRequest)
		return
	}
	var req shareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	saved, err := h.dashboards.CreateShare(r.Context(), insights.Share{
		TenantID:     t.ID,
		ResourceType: resourceType,
		ResourceID:   id,
		GranteeType:  req.GranteeType,
		Grantee:      req.Grantee,
		Permission:   req.Permission,
	})
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// listQueryShares + listDashboardShares are bound directly inside the
// respective sub-routers because chi's Route() consumes the path
// prefix — a parent-level /{resource}/{id}/shares route never sees
// requests under /queries or /dashboards.
func (h *insightsHandlers) listQueryShares(w http.ResponseWriter, r *http.Request) {
	h.listSharesFor(w, r, insights.ResourceQuery)
}

func (h *insightsHandlers) listDashboardShares(w http.ResponseWriter, r *http.Request) {
	h.listSharesFor(w, r, insights.ResourceDashboard)
}

func (h *insightsHandlers) deleteQueryShare(w http.ResponseWriter, r *http.Request) {
	h.deleteShareFor(w, r, insights.ResourceQuery)
}

func (h *insightsHandlers) deleteDashboardShare(w http.ResponseWriter, r *http.Request) {
	h.deleteShareFor(w, r, insights.ResourceDashboard)
}

// deleteShareFor removes a single share grant scoped to the parent
// resource on the URL. Both the parent {id} and the share row's
// resource columns must match — otherwise the path parents would be
// advisory only and a caller that knows any share id could remove
// it via any /queries/* or /dashboards/* parent.
func (h *insightsHandlers) deleteShareFor(w http.ResponseWriter, r *http.Request, resourceType string) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	resourceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid resource id", http.StatusBadRequest)
		return
	}
	shareID, err := uuid.Parse(chi.URLParam(r, "shareID"))
	if err != nil {
		http.Error(w, "invalid share id", http.StatusBadRequest)
		return
	}
	if err := h.dashboards.DeleteShare(r.Context(), t.ID, resourceType, resourceID, shareID); err != nil {
		writeInsightsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *insightsHandlers) listSharesFor(w http.ResponseWriter, r *http.Request, resourceType string) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid resource id", http.StatusBadRequest)
		return
	}
	shares, err := h.dashboards.ListShares(r.Context(), t.ID, resourceType, id)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": shares})
}

// ---------- error mapping ----------

func writeInsightsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, insights.ErrQueryNotFound),
		errors.Is(err, insights.ErrDashboardNotFound),
		errors.Is(err, insights.ErrWidgetNotFound),
		errors.Is(err, insights.ErrShareNotFound),
		errors.Is(err, insights.ErrDataSourceNotFound),
		errors.Is(err, insights.ErrEmbedNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, insights.ErrEmbedRevoked),
		errors.Is(err, insights.ErrEmbedExpired),
		errors.Is(err, insights.ErrEmbedExceeded):
		// Token is no longer servable but was once valid — 410 Gone
		// is the right semantic for revoked / expired / exhausted
		// resources, matching how the WebShare and similar specs
		// treat an exhausted token.
		http.Error(w, err.Error(), http.StatusGone)
	case errors.Is(err, insights.ErrValidation):
		// User-input failures from QueryStore / DashboardStore /
		// CacheStore validation paths and from QueryDefinition.
		// Validate() — surface as 400 so clients can correct their
		// payload instead of seeing a misleading 500.
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// Statement timeout / Go context cancellation — DB-side
		// budget exhausted, surface as 504 so a retry-with-tighter-
		// filter UI can react distinctly from generic 5xx.
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
	default:
		// Unknown error → 500. Validation / shape errors from the
		// handlers themselves are returned via http.Error directly
		// (see decode failures and uuid.Parse branches), so reaching
		// the default arm means an unexpected server-side fault
		// (DB connection, marshalling, etc.) rather than client input.
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
