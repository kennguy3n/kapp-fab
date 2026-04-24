package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KTypeExchangeRate is the metadata-driven KType mirror of the typed
// exchange_rates table. Exposed so the KType registry + generic record
// list page can render rates alongside other finance masters.
const KTypeExchangeRate = "finance.exchange_rate"

var exchangeRateSchema = []byte(`{
  "name": "finance.exchange_rate",
  "version": 1,
  "fields": [
    {"name": "from_currency", "type": "string", "required": true, "max_length": 3},
    {"name": "to_currency", "type": "string", "required": true, "max_length": 3},
    {"name": "rate_date", "type": "date", "required": true},
    {"name": "rate", "type": "number", "required": true, "precision": 18, "scale": 8},
    {"name": "provider", "type": "string", "max_length": 64}
  ],
  "views": {
    "list": {"columns": ["from_currency", "to_currency", "rate_date", "rate", "provider"]},
    "form": {"sections": [{"title": "Exchange rate", "fields": ["from_currency", "to_currency", "rate_date", "rate", "provider"]}]}
  },
  "cards": {"summary": "1 {{from_currency}} = {{rate}} {{to_currency}} on {{rate_date}}"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]}
}`)

// ExchangeRateKType returns the KType definition for finance.exchange_rate.
func ExchangeRateKType() ktype.KType {
	return ktype.KType{Name: KTypeExchangeRate, Version: 1, Schema: exchangeRateSchema}
}

// ExchangeRate is a single per-tenant rate between two ISO-4217
// currencies on a given date. Rates are strictly positive; inverse
// lookups divide rather than storing both directions.
type ExchangeRate struct {
	TenantID     uuid.UUID       `json:"tenant_id"`
	FromCurrency string          `json:"from_currency"`
	ToCurrency   string          `json:"to_currency"`
	RateDate     time.Time       `json:"rate_date"`
	Rate         decimal.Decimal `json:"rate"`
	Provider     string          `json:"provider,omitempty"`
	CreatedBy    *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// ExchangeRateStore persists and resolves exchange rates. The lookup
// rule mirrors ERPNext Currency Exchange: the latest rate on or before
// the requested date wins; if no direct rate exists, the inverse pair
// (to → from) is checked and inverted.
type ExchangeRateStore struct {
	pool *pgxpool.Pool
}

// NewExchangeRateStore wires an ExchangeRateStore from the shared pool.
func NewExchangeRateStore(pool *pgxpool.Pool) *ExchangeRateStore {
	return &ExchangeRateStore{pool: pool}
}

// Sentinel errors so callers can render 4xx responses.
var (
	ErrExchangeRateNotFound = errors.New("ledger: exchange rate not found")
	ErrInvalidCurrency      = errors.New("ledger: currency must be a 3-letter ISO code")
)

// UpsertRate writes or replaces a single rate for (from, to, date).
// Passing the same (from, to, date) triple twice updates the rate in
// place so external feeds can retry without duplicating rows.
func (s *ExchangeRateStore) UpsertRate(ctx context.Context, rate ExchangeRate) (*ExchangeRate, error) {
	if rate.TenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if err := validateCurrencyPair(rate.FromCurrency, rate.ToCurrency); err != nil {
		return nil, err
	}
	if !rate.Rate.IsPositive() {
		return nil, errors.New("ledger: rate must be positive")
	}
	if rate.RateDate.IsZero() {
		rate.RateDate = time.Now().UTC()
	}
	rate.RateDate = rate.RateDate.UTC().Truncate(24 * time.Hour)

	out := rate
	err := dbutil.WithTenantTx(ctx, s.pool, rate.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if rate.CreatedBy != nil {
			createdBy = *rate.CreatedBy
		}
		var provider any
		if rate.Provider != "" {
			provider = rate.Provider
		}
		return tx.QueryRow(ctx,
			`INSERT INTO exchange_rates
			     (tenant_id, from_currency, to_currency, rate_date, rate, provider, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, from_currency, to_currency, rate_date) DO UPDATE SET
			     rate = EXCLUDED.rate,
			     provider = EXCLUDED.provider,
			     updated_at = now()
			 RETURNING created_at, updated_at`,
			rate.TenantID, rate.FromCurrency, rate.ToCurrency, rate.RateDate,
			rate.Rate, provider, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: upsert exchange rate: %w", err)
	}
	return &out, nil
}

// GetRate resolves the effective rate for (from, to) on or before the
// requested date. When the pair is identical (USD → USD) the function
// short-circuits to 1 without hitting the database. When no direct row
// exists, the inverse pair is tried and inverted; when neither exists
// the caller receives ErrExchangeRateNotFound so they can decide
// whether to reject the posting or fall back to a default.
func (s *ExchangeRateStore) GetRate(ctx context.Context, tenantID uuid.UUID, from, to string, asOf time.Time) (decimal.Decimal, error) {
	if tenantID == uuid.Nil {
		return decimal.Zero, errors.New("ledger: tenant id required")
	}
	if from == to {
		if len(from) != 3 {
			return decimal.Zero, ErrInvalidCurrency
		}
		return decimal.NewFromInt(1), nil
	}
	if err := validateCurrencyPair(from, to); err != nil {
		return decimal.Zero, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	asOf = asOf.UTC().Truncate(24 * time.Hour)

	var rate decimal.Decimal
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Direct pair.
		err := tx.QueryRow(ctx,
			`SELECT rate FROM exchange_rates
			 WHERE tenant_id = $1
			   AND from_currency = $2
			   AND to_currency = $3
			   AND rate_date <= $4
			 ORDER BY rate_date DESC
			 LIMIT 1`,
			tenantID, from, to, asOf,
		).Scan(&rate)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Inverse pair — divide 1 by the inverse rate.
		var inverse decimal.Decimal
		err = tx.QueryRow(ctx,
			`SELECT rate FROM exchange_rates
			 WHERE tenant_id = $1
			   AND from_currency = $2
			   AND to_currency = $3
			   AND rate_date <= $4
			 ORDER BY rate_date DESC
			 LIMIT 1`,
			tenantID, to, from, asOf,
		).Scan(&inverse)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrExchangeRateNotFound
			}
			return err
		}
		if !inverse.IsPositive() {
			return ErrExchangeRateNotFound
		}
		rate = decimal.NewFromInt(1).Div(inverse)
		return nil
	})
	if err != nil {
		return decimal.Zero, err
	}
	return rate, nil
}

// Convert multiplies `amount` by the effective (from → to) rate on the
// requested date. Equal currencies short-circuit to the input value.
func (s *ExchangeRateStore) Convert(ctx context.Context, tenantID uuid.UUID, amount decimal.Decimal, from, to string, asOf time.Time) (decimal.Decimal, error) {
	if from == to {
		return amount, nil
	}
	rate, err := s.GetRate(ctx, tenantID, from, to, asOf)
	if err != nil {
		return decimal.Zero, err
	}
	return amount.Mul(rate), nil
}

// ListRates returns a tenant's rate history, optionally filtered to a
// single pair. Ordered newest first so the UI can render the latest
// without an extra sort.
func (s *ExchangeRateStore) ListRates(ctx context.Context, tenantID uuid.UUID, from, to string, limit int) ([]ExchangeRate, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := make([]ExchangeRate, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			rows pgx.Rows
			err  error
		)
		if from != "" || to != "" {
			if err := validateCurrencyPair(from, to); err != nil {
				return err
			}
			rows, err = tx.Query(ctx,
				`SELECT tenant_id, from_currency, to_currency, rate_date, rate,
				        COALESCE(provider, ''), created_by, created_at, updated_at
				 FROM exchange_rates
				 WHERE tenant_id = $1 AND from_currency = $2 AND to_currency = $3
				 ORDER BY rate_date DESC
				 LIMIT $4`,
				tenantID, from, to, limit,
			)
		} else {
			rows, err = tx.Query(ctx,
				`SELECT tenant_id, from_currency, to_currency, rate_date, rate,
				        COALESCE(provider, ''), created_by, created_at, updated_at
				 FROM exchange_rates
				 WHERE tenant_id = $1
				 ORDER BY rate_date DESC
				 LIMIT $2`,
				tenantID, limit,
			)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				r         ExchangeRate
				createdBy *uuid.UUID
			)
			if err := rows.Scan(
				&r.TenantID, &r.FromCurrency, &r.ToCurrency, &r.RateDate, &r.Rate,
				&r.Provider, &createdBy, &r.CreatedAt, &r.UpdatedAt,
			); err != nil {
				return err
			}
			r.CreatedBy = createdBy
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: list exchange rates: %w", err)
	}
	return out, nil
}

// UnrealizedGainLoss computes the revaluation delta for a foreign-
// currency balance: the difference between the balance converted at
// the current rate and at the original rate. A positive result means
// the balance has gained value in the functional currency.
//
// This is intentionally a pure calculation (no DB writes) so the
// reporting layer can compose it with any balance query without
// double-posting to the ledger — revaluation journal entries are an
// explicit caller decision, not a side effect.
func (s *ExchangeRateStore) UnrealizedGainLoss(ctx context.Context, tenantID uuid.UUID, foreignAmount decimal.Decimal, foreignCurrency, functionalCurrency string, originalRate decimal.Decimal, asOf time.Time) (decimal.Decimal, error) {
	if !originalRate.IsPositive() {
		return decimal.Zero, errors.New("ledger: original rate must be positive")
	}
	currentRate, err := s.GetRate(ctx, tenantID, foreignCurrency, functionalCurrency, asOf)
	if err != nil {
		return decimal.Zero, err
	}
	currentValue := foreignAmount.Mul(currentRate)
	originalValue := foreignAmount.Mul(originalRate)
	return currentValue.Sub(originalValue), nil
}

// validateCurrencyPair enforces the 3-letter ISO-4217 shape and rejects
// identical from/to pairs so the store never stores a rate that's
// logically always 1.
func validateCurrencyPair(from, to string) error {
	if len(from) != 3 || len(to) != 3 {
		return ErrInvalidCurrency
	}
	if from == to {
		return errors.New("ledger: from and to currencies must differ")
	}
	return nil
}
