package main

import (
	"encoding/json"
	"errors"
	"io"
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
	Name                 string                   `json:"name"`
	PresentationCurrency string                   `json:"presentation_currency"`
	MemberTenantIDs      []uuid.UUID              `json:"member_tenant_ids"`
	EliminationPairs     []ledger.EliminationPair `json:"elimination_pairs"`
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

// parseAsOf extracts the optional `as_of` override from a run
// request. Returns the zero time when the body is empty or absent
// (the run will fall back to as-of-now). chunked transfer-encoded
// clients have ContentLength == -1, so the older `> 0` guard
// silently skipped body parsing for them; this version attempts the
// decode whenever a body is present and tolerates an empty stream
// (io.EOF).
func parseAsOf(r *http.Request) (time.Time, error) {
	var body struct {
		AsOf *time.Time `json:"as_of"`
	}
	if r.Body == nil || r.ContentLength == 0 {
		return time.Time{}, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	if body.AsOf == nil {
		return time.Time{}, nil
	}
	return *body.AsOf, nil
}

// run executes a consolidation. Body carries an optional `as_of`
// override; when omitted, the call runs as-of now (UTC).
func (h *consolidationHandlers) run(w http.ResponseWriter, r *http.Request) {
	groupID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return
	}
	asOf, err := parseAsOf(r)
	if err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
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

// statements returns the full consolidated statement pack — trial
// balance, P&L, and balance sheet — for a group as-of an optional
// period end. The run is not persisted (the bare /run endpoint owns
// persistence); this is a read-style derivation over the same
// translated, eliminated rows so all three statements are
// internally consistent.
func (h *consolidationHandlers) statements(w http.ResponseWriter, r *http.Request) {
	groupID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return
	}
	asOf, err := parseAsOf(r)
	if err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	out, err := h.store.RunStatements(r.Context(), groupID, actor, ledger.ConsolidationOptions{AsOf: asOf, Persist: false})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// fxRevaluationHandlers backs the admin-only on-demand FX
// revaluation endpoint. The scheduled UnrealizedGainLossJob runs the
// same sweep automatically; this lets an operator trigger a
// period-end revaluation for a specific tenant and inspect the
// resulting per-account deltas. Mounted under /api/v1/admin so it
// inherits the control-plane admin auth, and it carries an explicit
// tenant_id because the revaluation posts into that tenant's ledger.
type fxRevaluationHandlers struct {
	ledger *ledger.PGStore
	rates  *ledger.ExchangeRateStore
}

type fxRevaluationRequest struct {
	TenantID         uuid.UUID  `json:"tenant_id"`
	AsOf             *time.Time `json:"as_of"`
	GainAccount      string     `json:"gain_account"`
	LossAccount      string     `json:"loss_account"`
	AccountAllowList []string   `json:"account_allow_list"`
}

// run revalues the tenant's open foreign-currency balances, posts the
// unrealized gain/loss adjustments, persists the run, and returns the
// result envelope.
func (h *fxRevaluationHandlers) run(w http.ResponseWriter, r *http.Request) {
	var req fxRevaluationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TenantID == uuid.Nil {
		http.Error(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	asOf := time.Time{}
	if req.AsOf != nil {
		asOf = *req.AsOf
	}
	actor := actorOrDefault(r.Context())
	runner := ledger.NewRevaluationRunner(h.ledger, h.rates, actor, ledger.RevaluationConfig{
		GainAccount: req.GainAccount,
		LossAccount: req.LossAccount,
	})
	out, err := runner.Run(r.Context(), req.TenantID, asOf, ledger.RevaluationConfig{AccountAllowList: req.AccountAllowList})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
