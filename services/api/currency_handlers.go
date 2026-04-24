package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// currencyHandlers exposes exchange-rate CRUD + conversion + unrealized
// gain/loss endpoints under /api/v1/finance/exchange-rates. Tenant
// scope is enforced by the finance router middleware.
type currencyHandlers struct {
	store *ledger.ExchangeRateStore
}

type upsertRateRequest struct {
	FromCurrency string          `json:"from_currency"`
	ToCurrency   string          `json:"to_currency"`
	RateDate     string          `json:"rate_date"`
	Rate         decimal.Decimal `json:"rate"`
	Provider     string          `json:"provider,omitempty"`
}

func (h *currencyHandlers) upsertRate(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req upsertRateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	var rateDate time.Time
	if req.RateDate != "" {
		parsed, err := time.Parse("2006-01-02", req.RateDate)
		if err != nil {
			http.Error(w, "rate_date must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		rateDate = parsed
	}
	actor := actorOrDefault(r.Context())
	rate, err := h.store.UpsertRate(r.Context(), ledger.ExchangeRate{
		TenantID:     t.ID,
		FromCurrency: req.FromCurrency,
		ToCurrency:   req.ToCurrency,
		RateDate:     rateDate,
		Rate:         req.Rate,
		Provider:     req.Provider,
		CreatedBy:    &actor,
	})
	if err != nil {
		writeCurrencyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rate)
}

func (h *currencyHandlers) listRates(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	rates, err := h.store.ListRates(r.Context(), t.ID, q.Get("from"), q.Get("to"), limit)
	if err != nil {
		writeCurrencyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rates": rates})
}

func (h *currencyHandlers) convert(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	amountStr := q.Get("amount")
	amount, err := decimal.NewFromString(amountStr)
	if err != nil {
		http.Error(w, "amount must be a decimal", http.StatusBadRequest)
		return
	}
	asOf := time.Now().UTC()
	if raw := q.Get("date"); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		asOf = parsed
	}
	converted, err := h.store.Convert(r.Context(), t.ID, amount, q.Get("from"), q.Get("to"), asOf)
	if err != nil {
		writeCurrencyError(w, err)
		return
	}
	rate, _ := h.store.GetRate(r.Context(), t.ID, q.Get("from"), q.Get("to"), asOf)
	writeJSON(w, http.StatusOK, map[string]any{
		"amount":    amount,
		"from":      q.Get("from"),
		"to":        q.Get("to"),
		"date":      asOf.Format("2006-01-02"),
		"rate":      rate,
		"converted": converted,
	})
}

type unrealizedGLRequest struct {
	ForeignAmount      decimal.Decimal `json:"foreign_amount"`
	ForeignCurrency    string          `json:"foreign_currency"`
	FunctionalCurrency string          `json:"functional_currency"`
	OriginalRate       decimal.Decimal `json:"original_rate"`
	AsOf               string          `json:"as_of,omitempty"`
}

func (h *currencyHandlers) unrealizedGL(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req unrealizedGLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	asOf := time.Now().UTC()
	if req.AsOf != "" {
		parsed, err := time.Parse("2006-01-02", req.AsOf)
		if err != nil {
			http.Error(w, "as_of must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		asOf = parsed
	}
	delta, err := h.store.UnrealizedGainLoss(r.Context(), t.ID, req.ForeignAmount, req.ForeignCurrency, req.FunctionalCurrency, req.OriginalRate, asOf)
	if err != nil {
		writeCurrencyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unrealized_gain_loss": delta})
}

func writeCurrencyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrExchangeRateNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ledger.ErrInvalidCurrency):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
