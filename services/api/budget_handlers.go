package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// budgetHandlers exposes the Phase N5 budget HTTP surface. Tenant
// scope is enforced by the middleware stack wired in routes.go;
// the handlers translate HTTP into BudgetStore calls and map
// sentinel errors to the status codes the web client expects.
type budgetHandlers struct {
	store *finance.BudgetStore
}

// ---------------------------------------------------------------------------
// Budget header CRUD.
// ---------------------------------------------------------------------------

type createBudgetRequest struct {
	Name              string           `json:"name"`
	FiscalYear        int              `json:"fiscal_year"`
	Status            string           `json:"status,omitempty"`
	CostCenter        string           `json:"cost_center,omitempty"`
	Notes             string           `json:"notes,omitempty"`
	VarianceThreshold *decimal.Decimal `json:"variance_threshold,omitempty"`
}

type updateBudgetRequest struct {
	Name              string           `json:"name"`
	Status            string           `json:"status"`
	CostCenter        string           `json:"cost_center,omitempty"`
	Notes             string           `json:"notes,omitempty"`
	VarianceThreshold *decimal.Decimal `json:"variance_threshold,omitempty"`
}

func (h *budgetHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	in := finance.Budget{
		TenantID:          t.ID,
		Name:              req.Name,
		FiscalYear:        req.FiscalYear,
		Status:            req.Status,
		CostCenter:        req.CostCenter,
		Notes:             req.Notes,
		VarianceThreshold: req.VarianceThreshold,
	}
	if actor != uuid.Nil {
		in.CreatedBy = &actor
	}
	out, err := h.store.CreateBudget(r.Context(), in)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *budgetHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.store.ListBudgets(r.Context(), t.ID)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *budgetHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid budget id", http.StatusBadRequest)
		return
	}
	out, err := h.store.GetBudget(r.Context(), t.ID, id)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *budgetHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid budget id", http.StatusBadRequest)
		return
	}
	var req updateBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.UpdateBudget(r.Context(), finance.Budget{
		TenantID:          t.ID,
		ID:                id,
		Name:              req.Name,
		Status:            req.Status,
		CostCenter:        req.CostCenter,
		Notes:             req.Notes,
		VarianceThreshold: req.VarianceThreshold,
	})
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *budgetHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid budget id", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteBudget(r.Context(), t.ID, id); err != nil {
		writeBudgetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Budget line CRUD.
// ---------------------------------------------------------------------------

type budgetLineRequest struct {
	ID          string              `json:"id,omitempty"`
	AccountCode string              `json:"account_code"`
	CostCenter  string              `json:"cost_center,omitempty"`
	Months      []decimal.Decimal   `json:"months"`
}

func (h *budgetHandlers) listLines(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	budgetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid budget id", http.StatusBadRequest)
		return
	}
	out, err := h.store.ListBudgetLines(r.Context(), t.ID, budgetID)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *budgetHandlers) upsertLine(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	budgetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid budget id", http.StatusBadRequest)
		return
	}
	var req budgetLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.Months) != 12 {
		http.Error(w, "months must contain exactly 12 entries", http.StatusBadRequest)
		return
	}
	var months [12]decimal.Decimal
	copy(months[:], req.Months)
	line := finance.BudgetLine{
		TenantID:    t.ID,
		BudgetID:    budgetID,
		AccountCode: req.AccountCode,
		CostCenter:  req.CostCenter,
		Months:      months,
	}
	if req.ID != "" {
		id, err := uuid.Parse(req.ID)
		if err != nil {
			http.Error(w, "invalid line id", http.StatusBadRequest)
			return
		}
		line.ID = id
	}
	out, err := h.store.UpsertBudgetLine(r.Context(), line)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *budgetHandlers) deleteLine(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	// REST URL: /budgets/{id}/lines/{lineID} — pass both IDs through
	// to the store so deleting a line whose parent is a different
	// budget produces a 404 rather than silently succeeding.
	budgetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid budget id", http.StatusBadRequest)
		return
	}
	lineID, err := uuid.Parse(chi.URLParam(r, "lineID"))
	if err != nil {
		http.Error(w, "invalid line id", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteBudgetLine(r.Context(), t.ID, budgetID, lineID); err != nil {
		writeBudgetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Variance report.
// ---------------------------------------------------------------------------

func (h *budgetHandlers) varianceReport(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	budgetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid budget id", http.StatusBadRequest)
		return
	}
	q := finance.VarianceQuery{BudgetID: budgetID}
	if v := r.URL.Query().Get("from"); v != "" {
		from, err := time.Parse("2006-01-02", v)
		if err != nil {
			http.Error(w, "invalid from date (want YYYY-MM-DD)", http.StatusBadRequest)
			return
		}
		q.From = from
	}
	if v := r.URL.Query().Get("to"); v != "" {
		to, err := time.Parse("2006-01-02", v)
		if err != nil {
			http.Error(w, "invalid to date (want YYYY-MM-DD)", http.StatusBadRequest)
			return
		}
		// Advance to end-of-day so journal entries posted any time
		// during the supplied calendar day are included. Matches the
		// default To semantics in BudgetVsActual (Dec 31 23:59:59)
		// and the agent tool's parseAgentDateEnd path.
		q.To = endOfDay(to)
	}
	out, err := h.store.BudgetVsActual(r.Context(), t.ID, q)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// endOfDay returns the supplied date advanced to the last
// representable instant on that calendar day (23:59:59.999999999 in
// the date's own location). The variance endpoint parses
// ?to=YYYY-MM-DD as a midnight in the caller's location, which
// would exclude every journal_line posted later that day under the
// SQL filter `je.posted_at <= $3`. Advancing to the final
// nanosecond — not 23:59:59.0 — keeps the API contract ("include
// the supplied end date") tight against pgx's `timestamp with time
// zone` which preserves microsecond precision; any entry posted in
// the final sub-second of the day is included rather than silently
// dropped.
//
// int(time.Second-1) = 999_999_999 nanoseconds. Three other surfaces
// (services/api/finance_handlers.go::endOfDayUTC,
// internal/agents/finance_tools.go::parseAgentDateEnd,
// internal/finance/budget.go::BudgetVsActual default `To`) use the
// identical constant so every <= bound across the finance API
// observes the same day-inclusivity semantics. The expression is
// kept as `int(time.Second-1)` rather than the equivalent
// `int(time.Second/time.Nanosecond)-1` so all four call-sites
// read identically.
func endOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, int(time.Second-1), t.Location())
}

// writeBudgetError translates BudgetStore sentinel errors into HTTP
// statuses the web client keys off for user-facing messaging.
func writeBudgetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, finance.ErrBudgetNotFound),
		errors.Is(err, finance.ErrBudgetLineNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, finance.ErrInvalidBudget),
		errors.Is(err, finance.ErrInvalidBudgetLine):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
