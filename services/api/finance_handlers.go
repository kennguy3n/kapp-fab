package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// financeHandlers exposes the Phase C finance HTTP surface: chart of
// accounts, manual + source-driven journal entries, period lockout,
// and the four canonical reports (trial balance, AR/AP aging, income
// statement). Tenant scope is enforced by the middleware stack wired
// in main.go; the handlers translate HTTP into ledger calls and map
// sentinel errors to status codes the web client expects.
type financeHandlers struct {
	store   *ledger.PGStore
	poster  *ledger.InvoicePoster
}

// ---------------------------------------------------------------------------
// Chart of accounts
// ---------------------------------------------------------------------------

type createAccountRequest struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ParentCode string `json:"parent_code"`
	Active     *bool  `json:"active"`
}

func (h *financeHandlers) createAccount(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	acct, err := h.store.CreateAccount(r.Context(), ledger.Account{
		TenantID:   t.ID,
		Code:       req.Code,
		Name:       req.Name,
		Type:       req.Type,
		ParentCode: req.ParentCode,
		Active:     active,
	})
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, acct)
}

func (h *financeHandlers) listAccounts(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	filter := ledger.AccountFilter{
		Type:   r.URL.Query().Get("type"),
		Limit:  limit,
		Offset: offset,
	}
	if raw := r.URL.Query().Get("active"); raw != "" {
		if v, err := strconv.ParseBool(raw); err == nil {
			filter.Active = &v
		}
	}
	accts, err := h.store.ListAccounts(r.Context(), t.ID, filter)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accts)
}

func (h *financeHandlers) getAccount(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	code := chi.URLParam(r, "code")
	if code == "" {
		http.Error(w, "account code required", http.StatusBadRequest)
		return
	}
	acct, err := h.store.GetAccount(r.Context(), t.ID, code)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

// ---------------------------------------------------------------------------
// Journal entries
// ---------------------------------------------------------------------------

type journalLineRequest struct {
	AccountCode string          `json:"account_code"`
	Debit       decimal.Decimal `json:"debit"`
	Credit      decimal.Decimal `json:"credit"`
	Currency    string          `json:"currency"`
	Memo        string          `json:"memo"`
}

type postJournalEntryRequest struct {
	PostedAt    *time.Time           `json:"posted_at"`
	Memo        string               `json:"memo"`
	SourceKType string               `json:"source_ktype"`
	SourceID    *uuid.UUID           `json:"source_id"`
	Lines       []journalLineRequest `json:"lines"`
}

func (h *financeHandlers) postJournalEntry(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req postJournalEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	lines := make([]ledger.JournalLine, 0, len(req.Lines))
	for _, l := range req.Lines {
		lines = append(lines, ledger.JournalLine{
			AccountCode: l.AccountCode,
			Debit:       l.Debit,
			Credit:      l.Credit,
			Currency:    l.Currency,
			Memo:        l.Memo,
		})
	}
	entry := ledger.JournalEntry{
		TenantID:    t.ID,
		Memo:        req.Memo,
		SourceKType: req.SourceKType,
		SourceID:    req.SourceID,
		CreatedBy:   actorOrDefault(r.Context()),
		Lines:       lines,
	}
	if req.PostedAt != nil {
		entry.PostedAt = *req.PostedAt
	}
	posted, err := h.store.PostJournalEntry(r.Context(), entry)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, posted)
}

func (h *financeHandlers) listJournalEntries(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	filter := ledger.JournalEntryFilter{
		SourceKType: r.URL.Query().Get("source_ktype"),
		AccountCode: r.URL.Query().Get("account_code"),
		Limit:       limit,
		Offset:      offset,
	}
	if raw := r.URL.Query().Get("from"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			filter.From = &t
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			filter.To = &t
		}
	}
	if raw := r.URL.Query().Get("source_id"); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			filter.SourceID = &id
		}
	}
	entries, err := h.store.ListJournalEntries(r.Context(), t.ID, filter)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *financeHandlers) getJournalEntry(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid entry id", http.StatusBadRequest)
		return
	}
	entry, err := h.store.GetJournalEntry(r.Context(), t.ID, id)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// ---------------------------------------------------------------------------
// Invoice / bill posting
// ---------------------------------------------------------------------------

func (h *financeHandlers) postInvoice(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid invoice id", http.StatusBadRequest)
		return
	}
	entry, err := h.poster.PostSalesInvoice(r.Context(), t.ID, id, actorOrDefault(r.Context()))
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *financeHandlers) postBill(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid bill id", http.StatusBadRequest)
		return
	}
	entry, err := h.poster.PostPurchaseBill(r.Context(), t.ID, id, actorOrDefault(r.Context()))
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// ---------------------------------------------------------------------------
// Period lockout
// ---------------------------------------------------------------------------

type lockPeriodRequest struct {
	PeriodStart string `json:"period_start"` // YYYY-MM-DD
	PeriodEnd   string `json:"period_end"`   // optional; omit to lock existing period
}

func (h *financeHandlers) lockPeriod(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req lockPeriodRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	start, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		http.Error(w, "invalid period_start (want YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	// Caller may upsert the period before locking it so the engine has
	// something to flip; an empty period_end leaves the existing row
	// untouched and LockPeriod below returns ErrPeriodNotFound if no
	// matching row exists.
	if req.PeriodEnd != "" {
		end, err := time.Parse("2006-01-02", req.PeriodEnd)
		if err != nil {
			http.Error(w, "invalid period_end (want YYYY-MM-DD)", http.StatusBadRequest)
			return
		}
		if _, err := h.store.UpsertPeriod(r.Context(), ledger.FiscalPeriod{
			TenantID:    t.ID,
			PeriodStart: start,
			PeriodEnd:   end,
		}); err != nil {
			writeFinanceError(w, err)
			return
		}
	}
	period, err := h.store.LockPeriod(r.Context(), t.ID, start, actor)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, period)
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

func (h *financeHandlers) trialBalance(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	asOf := parseEndOfDayParam(r.URL.Query().Get("as_of"), time.Now().UTC())
	report, err := h.store.TrialBalance(r.Context(), t.ID, asOf)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *financeHandlers) arAging(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	asOf := parseEndOfDayParam(r.URL.Query().Get("as_of"), time.Now().UTC())
	report, err := h.store.ARAgingReport(r.Context(), t.ID, asOf)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *financeHandlers) apAging(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	asOf := parseEndOfDayParam(r.URL.Query().Get("as_of"), time.Now().UTC())
	report, err := h.store.APAgingReport(r.Context(), t.ID, asOf)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *financeHandlers) incomeStatement(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	fromRaw := r.URL.Query().Get("from")
	toRaw := r.URL.Query().Get("to")
	if fromRaw == "" || toRaw == "" {
		http.Error(w, "from and to required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	from, err := time.Parse("2006-01-02", fromRaw)
	if err != nil {
		http.Error(w, "invalid from", http.StatusBadRequest)
		return
	}
	to, err := time.Parse("2006-01-02", toRaw)
	if err != nil {
		http.Error(w, "invalid to", http.StatusBadRequest)
		return
	}
	// Bump `to` to end-of-day so BETWEEN includes entries posted later
	// on the same calendar date (parse returns midnight UTC).
	to = endOfDayUTC(to)
	report, err := h.store.IncomeStatement(r.Context(), t.ID, from, to)
	if err != nil {
		writeFinanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseEndOfDayParam is the inclusive-upper-bound date parser used by
// "as_of" / "to" style filters. A bare YYYY-MM-DD parses to midnight
// UTC, which would exclude entries posted later the same day from a
// `posted_at <= as_of` predicate; bumping the bound to
// 23:59:59.999999999 UTC makes the comparison calendar-date-inclusive.
// RFC3339 values carry an explicit timestamp and are used verbatim.
// Empty or malformed input falls back to the supplied default.
func parseEndOfDayParam(raw string, fallback time.Time) time.Time {
	if raw == "" {
		return fallback
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return endOfDayUTC(t)
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC()
	}
	return fallback
}

// endOfDayUTC returns the last representable instant of the supplied
// day in UTC (23:59:59.999999999). Used for inclusive date ranges in
// reports.
func endOfDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
}

// writeFinanceError translates ledger sentinel errors into the HTTP
// status codes the web client keys off for user-facing messaging.
func writeFinanceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound),
		errors.Is(err, ledger.ErrEntryNotFound),
		errors.Is(err, ledger.ErrPeriodNotFound),
		errors.Is(err, ledger.ErrTaxCodeNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ledger.ErrPeriodLocked),
		errors.Is(err, ledger.ErrInvoiceAlreadyPosted):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ledger.ErrUnbalancedEntry),
		errors.Is(err, ledger.ErrEmptyEntry),
		errors.Is(err, ledger.ErrInvalidLine),
		errors.Is(err, ledger.ErrInactiveAccount),
		errors.Is(err, ledger.ErrCurrencyMismatch),
		errors.Is(err, ledger.ErrSourceMismatch),
		errors.Is(err, ledger.ErrInvoiceNotPostable):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeRecordError(w, err)
	}
}
