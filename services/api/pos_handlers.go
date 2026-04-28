package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/sales"
)

// posHandlers wires the Phase M Task 6 POS HTTP surface. The only
// non-CRUD endpoint is the finalize call that reuses the existing
// invoice + payment posters via sales.POSPoster; cart, profile,
// and price-list management ride the generic /api/v1/records
// endpoints already gated on the FeaturePOS / FeatureInventory
// flags via the dynamic feature middleware.
type posHandlers struct {
	poster *sales.POSPoster
}

// finalize promotes a draft sales.pos_invoice into a posted state:
// it creates a finance.ar_invoice + finance.payment, posts both,
// and flips the pos_invoice's status to posted with refs back.
// Idempotent on the underlying pos_invoice (status check) and on
// the HTTP request (Idempotency-Key middleware). Replays return
// the prior pos_invoice unchanged so a flaky offline-queue retry
// does not produce duplicate journal entries.
func (h *posHandlers) finalize(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid pos invoice id", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	rec, err := h.poster.PostPOSInvoice(r.Context(), t.ID, id, actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}
