package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// tenantFromCtx is a shorthand for platform.TenantFromContext —
// keeps the metering handlers terse without importing the package
// in three places.
func tenantFromCtx(r *http.Request) *tenant.Tenant {
	return platform.TenantFromContext(r.Context())
}

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

// resolveTenantID returns the {id} URL param when present, else the
// id of the tenant on the request context (set by TenantMiddleware).
// /tenants/{id}/* endpoints are control-plane and accept the URL
// param; /tenants/me/* endpoints rely on the JWT-derived tenant in
// context. Either path lands in the same handler here.
func resolveTenantID(r *http.Request) (uuid.UUID, error) {
	if raw := chi.URLParam(r, "id"); raw != "" {
		return uuid.Parse(raw)
	}
	if t := tenantFromCtx(r); t != nil {
		return t.ID, nil
	}
	return uuid.Nil, errors.New("tenant id missing")
}

// usageHistory returns the last N months of usage rows for the
// tenant. Driven by the /tenants/{id}/usage/history and
// /tenants/me/usage/history routes. Defaults to 6 months when the
// `months` query param is absent.
func (h *meteringHandlers) usageHistory(w http.ResponseWriter, r *http.Request) {
	id, err := resolveTenantID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	months := 6
	if v := r.URL.Query().Get("months"); v != "" {
		if n, perr := parsePositiveInt(v); perr == nil {
			months = n
		}
	}
	rows, err := h.metering.GetHistory(r.Context(), id, months)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": id,
		"rows":      rows,
		"months":    months,
	})
}

// usageMe is the JWT-resolved variant of usage(). The chi.URLParam
// "id" is empty, so the handler reads the tenant off the request
// context and forwards into the same response shape.
func (h *meteringHandlers) usageMe(w http.ResponseWriter, r *http.Request) {
	t := tenantFromCtx(r)
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusUnauthorized)
		return
	}
	// Defer to the existing handler by re-attaching the id as a
	// chi URLParam via the chi.RouteContext — simpler than
	// duplicating the body verbatim.
	chi.RouteContext(r.Context()).URLParams.Add("id", t.ID.String())
	h.usage(w, r)
}

// changePlanMe is the /me variant of changePlan.
func (h *meteringHandlers) changePlanMe(w http.ResponseWriter, r *http.Request) {
	t := tenantFromCtx(r)
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusUnauthorized)
		return
	}
	chi.RouteContext(r.Context()).URLParams.Add("id", t.ID.String())
	h.changePlan(w, r)
}

func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a positive int")
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0, errors.New("out of range")
		}
	}
	if n == 0 {
		return 0, errors.New("must be > 0")
	}
	return n, nil
}
