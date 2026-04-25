package main

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dashboard"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// dashboardRateAdapter narrows ledger.ExchangeRateStore to the
// dashboard.Converter interface. Convert returns ok=false when the
// rate lookup fails so the dashboard's foldToBase falls back to the
// raw foreign-currency value rather than silently zeroing it.
type dashboardRateAdapter struct {
	rates *ledger.ExchangeRateStore
}

func (a dashboardRateAdapter) Convert(ctx context.Context, tenantID uuid.UUID, amount float64, from, to string) (float64, bool) {
	if a.rates == nil {
		return amount, false
	}
	out, err := a.rates.Convert(ctx, tenantID, decimal.NewFromFloat(amount), from, to, time.Now().UTC())
	if err != nil {
		return amount, false
	}
	f, _ := out.Float64()
	return f, true
}

// dashboardHandlers renders the Phase I KPI summary surface. The
// actual aggregation lives in internal/dashboard so the same code
// powers the endpoint and the integration tests.
type dashboardHandlers struct {
	store *dashboard.Store
}

func (h *dashboardHandlers) summary(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	s, err := h.store.ComputeSummary(r.Context(), t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}
