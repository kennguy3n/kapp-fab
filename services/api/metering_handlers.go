package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// meteringHandlers backs the /tenants/{id}/usage, /plans, and
// /tenants/{id}/plan routes. All three are control-plane
// (administrative), not tenant-scoped — the caller supplies the
// tenant id via URL parameter and the store runs every query under
// that tenant's RLS context.
type meteringHandlers struct {
	metering *tenant.MeteringStore
	tenants  *tenant.PGStore
	plans    *tenant.PlanStore
	features *tenant.FeatureStore
}

// usageResponse bundles the tenant's current-period counters with
// the plan limits they roll up against so the UI can render a
// single payload without a follow-up /plans fetch.
type usageResponse struct {
	TenantID    uuid.UUID         `json:"tenant_id"`
	Plan        string            `json:"plan"`
	PeriodStart time.Time         `json:"period_start"`
	Usage       map[string]int64  `json:"usage"`
	Limits      tenant.PlanLimits `json:"limits"`
	Rows        []tenant.UsageRow `json:"rows"`
	Features    map[string]bool   `json:"features"`
}

func (h *meteringHandlers) usage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	tn, err := h.tenants.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			http.Error(w, "tenant not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := h.metering.GetAllMetrics(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	usage := map[string]int64{}
	for _, row := range rows {
		usage[row.Metric] = row.Value
	}
	// Zero-fill canonical metrics so the UI always has a row for
	// every bar chart regardless of whether traffic has landed yet.
	for _, m := range []string{tenant.MetricAPICalls, tenant.MetricStorageBytes, tenant.MetricKRecordCount, tenant.MetricUserSeats} {
		if _, ok := usage[m]; !ok {
			usage[m] = 0
		}
	}
	limits := tenant.PlanLimits{}
	plan, err := h.plans.Get(r.Context(), tn.Plan)
	if err == nil {
		limits = plan.Limits
	}
	features, err := h.features.ListFeatures(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, usageResponse{
		TenantID:    id,
		Plan:        tn.Plan,
		PeriodStart: h.metering.CurrentPeriod(),
		Usage:       usage,
		Limits:      limits,
		Rows:        rows,
		Features:    features,
	})
}

func (h *meteringHandlers) listPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := h.plans.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

type planChangeRequest struct {
	Plan string `json:"plan"`
}

// changePlan atomically moves the tenant between plans: updates
// tenants.plan + tenants.quota and resets tenant_features to the
// new plan's defaults. The write runs inside a single
// control-plane tx so a crash mid-switch rolls every piece back.
func (h *meteringHandlers) changePlan(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	var req planChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	plan, err := h.plans.Get(r.Context(), req.Plan)
	if err != nil {
		if errors.Is(err, tenant.ErrPlanNotFound) {
			http.Error(w, "plan not found", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	quotaRaw, err := json.Marshal(plan.Limits)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.tenants.UpdatePlan(r.Context(), id, plan.Name, quotaRaw); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Reset feature flags to the new plan's defaults. Overwrites
	// existing overrides by design — a plan change is intended to
	// be a "reset to the new baseline" event.
	if err := h.features.SetFeatures(r.Context(), id, plan.Features); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": id,
		"plan":      plan,
	})
}
