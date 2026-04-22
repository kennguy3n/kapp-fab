package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// UpsertTaxCode creates or updates a tax-code registry entry. Rate is a
// percentage (e.g. 10.00 for 10%). The typed CHECK constraint in
// migrations/000004_finance_extensions.sql rejects out-of-range rates
// and unknown types as defence in depth.
func (s *PGStore) UpsertTaxCode(ctx context.Context, tc TaxCode) (*TaxCode, error) {
	if tc.TenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if tc.Code == "" || tc.Name == "" {
		return nil, errors.New("ledger: tax code and name required")
	}
	if tc.Type != TaxTypeInclusive && tc.Type != TaxTypeExclusive {
		return nil, fmt.Errorf("ledger: invalid tax type %q", tc.Type)
	}
	if tc.Rate.IsNegative() {
		return nil, errors.New("ledger: tax rate cannot be negative")
	}
	out := tc
	err := dbutil.WithTenantTx(ctx, s.pool, tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tax_codes (tenant_id, code, name, rate, type, active)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, code) DO UPDATE SET
			     name = EXCLUDED.name,
			     rate = EXCLUDED.rate,
			     type = EXCLUDED.type,
			     active = EXCLUDED.active`,
			tc.TenantID, tc.Code, tc.Name, tc.Rate, tc.Type, tc.Active,
		)
		if err != nil {
			return fmt.Errorf("ledger: upsert tax code: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTaxCode loads a single code. Returns ErrTaxCodeNotFound when absent
// so callers can fall back to a zero-rate calculation.
func (s *PGStore) GetTaxCode(ctx context.Context, tenantID uuid.UUID, code string) (*TaxCode, error) {
	if tenantID == uuid.Nil || code == "" {
		return nil, errors.New("ledger: tenant id and code required")
	}
	var out TaxCode
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, code, name, rate, type, active
			 FROM tax_codes WHERE tenant_id = $1 AND code = $2`,
			tenantID, code,
		).Scan(&out.TenantID, &out.Code, &out.Name, &out.Rate, &out.Type, &out.Active)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTaxCodeNotFound
			}
			return fmt.Errorf("ledger: load tax code: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTaxCodes returns every tax code for the tenant ordered by code.
func (s *PGStore) ListTaxCodes(ctx context.Context, tenantID uuid.UUID) ([]TaxCode, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	out := make([]TaxCode, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, code, name, rate, type, active
			 FROM tax_codes WHERE tenant_id = $1
			 ORDER BY code`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("ledger: list tax codes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var tc TaxCode
			if err := rows.Scan(&tc.TenantID, &tc.Code, &tc.Name, &tc.Rate, &tc.Type, &tc.Active); err != nil {
				return fmt.Errorf("ledger: scan tax code: %w", err)
			}
			out = append(out, tc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CalculateTax returns the tax amount and the net/gross split for the
// supplied subtotal. For "exclusive" codes the tax is added on top of
// subtotal; for "inclusive" codes the subtotal already contains tax.
//
//	net    — amount excluding tax (revenue / expense base)
//	tax    — tax amount
//	gross  — amount the customer pays (net + tax for exclusive, or
//	          subtotal unchanged for inclusive)
func (tc TaxCode) CalculateTax(subtotal decimal.Decimal) (net, tax, gross decimal.Decimal) {
	if tc.Rate.IsZero() {
		return subtotal, decimal.Zero, subtotal
	}
	rate := tc.Rate.Div(decimal.NewFromInt(100))
	switch tc.Type {
	case TaxTypeExclusive:
		tax = subtotal.Mul(rate)
		return subtotal, tax, subtotal.Add(tax)
	case TaxTypeInclusive:
		denom := decimal.NewFromInt(1).Add(rate)
		net = subtotal.Div(denom)
		tax = subtotal.Sub(net)
		return net, tax, subtotal
	default:
		return subtotal, decimal.Zero, subtotal
	}
}

// jsonMustMarshal is an internal helper for the finance package — the
// payloads are built from constant-shaped maps so json.Marshal errors
// would indicate a programming error rather than runtime data. Uses
// the empty object on failure so the caller still gets a valid payload.
func jsonMustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
