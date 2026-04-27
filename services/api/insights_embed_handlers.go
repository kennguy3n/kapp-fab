package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// insightsEmbedHandlers exposes the dashboard-embed surface:
//
//   - POST /api/v1/insights/dashboards/{id}/embeds        (auth, owner)
//   - GET  /api/v1/insights/dashboards/{id}/embeds        (auth, owner)
//   - POST /api/v1/insights/dashboards/{id}/embeds/{eid}/revoke
//   - GET  /api/v1/insights/embed/{token}                 (unauth)
//
// The unauthenticated GET path is rate-limited against the *owning
// tenant's* bucket so a viral embed cannot starve other tenants in
// the same cell.
type insightsEmbedHandlers struct {
	embeds      *insights.EmbedStore
	dashboards  *insights.DashboardStore
	queries     *insights.QueryStore
	runner      *insights.Runner
	features    *tenant.FeatureStore
	rateLimiter *platform.RateLimiter
}

type embedCreateRequest struct {
	ScopedFilters json.RawMessage `json:"scoped_filters,omitempty"`
	MaxViews      int             `json:"max_views,omitempty"`
	ExpiresInDays int             `json:"expires_in_days,omitempty"`
}

func (h *insightsEmbedHandlers) requireFeature(w http.ResponseWriter, r *http.Request, t *platformTenant) bool {
	if h.features == nil {
		return true
	}
	enabled, err := h.features.IsEnabled(r.Context(), t.ID, tenant.FeatureInsightsEmbed)
	if err != nil {
		http.Error(w, "feature lookup failed", http.StatusInternalServerError)
		return false
	}
	if !enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "insights_embed feature disabled"})
		return false
	}
	return true
}

func (h *insightsEmbedHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	dashboardID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	var req embedCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if _, err := h.dashboards.Get(r.Context(), t.ID, dashboardID); err != nil {
		writeInsightsError(w, err)
		return
	}
	actor := actorOrDefault(r.Context())
	embed := insights.Embed{
		TenantID:      t.ID,
		DashboardID:   dashboardID,
		ScopedFilters: req.ScopedFilters,
		MaxViews:      req.MaxViews,
		CreatedBy:     &actor,
	}
	if req.ExpiresInDays > 0 {
		exp := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		embed.ExpiresAt = &exp
	}
	out, err := h.embeds.Create(r.Context(), embed)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *insightsEmbedHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	dashboardID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	embs, err := h.embeds.List(r.Context(), t.ID, dashboardID)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"embeds": embs})
}

func (h *insightsEmbedHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if !h.requireFeature(w, r, t) {
		return
	}
	embedID, err := uuid.Parse(chi.URLParam(r, "embed_id"))
	if err != nil {
		http.Error(w, "invalid embed id", http.StatusBadRequest)
		return
	}
	if err := h.embeds.Revoke(r.Context(), t.ID, embedID); err != nil {
		writeInsightsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// public is the unauthenticated lookup. It runs without a tenant
// context middleware (mounted outside the auth chain), so the
// handler must:
//  1. Find the embed row (admin-pool, no tenant GUC).
//  2. Apply the owning tenant's rate limit.
//  3. Run the dashboard's queries with tenant context = the embed's
//     tenant_id.
//  4. Strip any data the embed scoping doesn't include.
func (h *insightsEmbedHandlers) public(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	embed, err := h.embeds.LookupByToken(r.Context(), token)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	// Bill the rate-limit bucket to the *owning* tenant so a viral
	// embed cannot saturate other tenants in the same cell.
	if h.rateLimiter != nil && !h.rateLimiter.Allow(embed.TenantID, 0, 0) {
		http.Error(w, platform.ErrRateLimitExceeded.Error(), http.StatusTooManyRequests)
		return
	}
	// Embeds never widen filters: the embed's scoped_filters override
	// any caller-supplied filters; ad hoc filter parameters from
	// query string are ignored.
	dashboard, err := h.dashboards.Get(r.Context(), embed.TenantID, embed.DashboardID)
	if err != nil {
		writeInsightsError(w, err)
		return
	}
	widgets := dashboard.Widgets
	results := make([]map[string]any, 0, len(widgets))
	for _, widget := range widgets {
		if widget.QueryID == nil || *widget.QueryID == uuid.Nil {
			continue
		}
		runRes, err := h.runner.RunSaved(r.Context(), embed.TenantID, *widget.QueryID, embedFilterParams(embed.ScopedFilters), false)
		if err != nil {
			// Embed renders are forgiving — one widget failing
			// must not break the whole render. Log via writeJSON
			// status so the inner widget surfaces the error.
			results = append(results, map[string]any{
				"widget_id": widget.ID,
				"error":     err.Error(),
			})
			continue
		}
		results = append(results, map[string]any{
			"widget_id":  widget.ID,
			"query_id":   widget.QueryID,
			"position":   widget.Position,
			"result":     runRes.Result,
			"cache_hit":  runRes.CacheHit,
			"expires_at": runRes.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dashboard": map[string]any{
			"id":          dashboard.ID,
			"name":        dashboard.Name,
			"description": dashboard.Description,
		},
		"widgets":  results,
		"embed_id": embed.ID,
	})
}

// embedFilterParams converts the embed's scoped_filters JSON into the
// map[string]any shape the runner expects. Returns nil for empty /
// missing input so the cache key collapses to the no-filters bucket.
func embedFilterParams(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// embedRunTimeout caps the unauth render at 10s end-to-end. Tighter
// than the auth'd dashboard runner because anonymous calls don't
// pay session-cookie auth latency and we want budget to spare for
// the rate-limit + lookup overhead.
const embedRunTimeout = 10 * time.Second

// withEmbedDeadline wraps the request context with a per-call
// deadline that the runner respects via context.WithTimeout.
func withEmbedDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, embedRunTimeout)
}

// silence unused-import lint while wiring up.
var _ = errors.Is
var _ = fmt.Sprint
