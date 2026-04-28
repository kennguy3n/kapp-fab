package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
)

// consolidationHandlers backs the Phase M Task 7 admin-only
// /api/v1/admin/consolidation/* routes. The store reads multiple
// tenants' trial balances through the admin pool so a single run
// crosses tenant boundaries without juggling per-tenant connection
// contexts. Mounted under /api/v1/admin which already requires
// control-plane admin auth via the surrounding middleware stack.
type consolidationHandlers struct {
	store *ledger.ConsolidationStore
}

type createConsolidationGroupRequest struct {
	Name                 string                     `json:"name"`
	PresentationCurrency string                     `json:"presentation_currency"`
	MemberTenantIDs      []uuid.UUID                `json:"member_tenant_ids"`
	EliminationPairs     []ledger.EliminationPair   `json:"elimination_pairs"`
}

// createGroup persists a new consolidation_group.
func (h *consolidationHandlers) createGroup(w http.ResponseWriter, r *http.Request) {
	var req createConsolidationGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g, err := h.store.CreateGroup(r.Context(), ledger.ConsolidationGroup{
		Name:                 req.Name,
		PresentationCurrency: req.PresentationCurrency,
		MemberTenantIDs:      req.MemberTenantIDs,
		EliminationPairs:     req.EliminationPairs,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(g)
}

// run executes a consolidation. Body carries an optional `as_of`
// override; when omitted, the call runs as-of now (UTC).
func (h *consolidationHandlers) run(w http.ResponseWriter, r *http.Request) {
	groupID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return
	}
	var body struct {
		AsOf *time.Time `json:"as_of"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	asOf := time.Time{}
	if body.AsOf != nil {
		asOf = *body.AsOf
	}
	actor := actorOrDefault(r.Context())
	out, err := h.store.RunConsolidation(r.Context(), groupID, asOf, actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
