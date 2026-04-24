package main

import (
	"net/http"

	"github.com/kennguy3n/kapp-fab/internal/dashboard"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

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
