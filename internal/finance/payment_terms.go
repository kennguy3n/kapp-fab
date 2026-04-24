package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// PaymentTermsInstallment is one row of a payment_terms row's
// `installments` JSON array. Field names mirror the schema; the
// computation only reads JSON-tagged fields so missing values
// default cleanly (zero days, zero percent, zero discount).
type PaymentTermsInstallment struct {
	DueDays         int             `json:"due_days"`
	Percentage      decimal.Decimal `json:"percentage"`
	DiscountDays    int             `json:"discount_days,omitempty"`
	DiscountPercent decimal.Decimal `json:"discount_percent,omitempty"`
	Label           string          `json:"label,omitempty"`
}

// PaymentScheduleEntry is one materialised installment line stored
// on the source invoice/bill record's `payment_schedule` array.
// Amounts are pre-computed off the invoice total at compute time so
// downstream readers do not need access to the terms row.
type PaymentScheduleEntry struct {
	Installment    int             `json:"installment"`
	Label          string          `json:"label,omitempty"`
	DueDate        string          `json:"due_date"`
	Percentage     decimal.Decimal `json:"percentage"`
	Amount         decimal.Decimal `json:"amount"`
	DiscountAmount decimal.Decimal `json:"discount_amount,omitempty"`
	DiscountUntil  string          `json:"discount_until,omitempty"`
}

// ErrInvalidPaymentTerms surfaces when a payment_terms row has
// installments that don't sum to ~100% or any other shape problem
// the poster should refuse to silently ignore.
var ErrInvalidPaymentTerms = errors.New("finance: invalid payment_terms")

// LoadPaymentTermsInstallments fetches a finance.payment_terms
// KRecord by ID and returns its installment plan. Inactive rows are
// rejected so a manually-paused template never sneaks into a fresh
// invoice's posting path.
func LoadPaymentTermsInstallments(
	ctx context.Context,
	records *record.PGStore,
	tenantID, termsID uuid.UUID,
) ([]PaymentTermsInstallment, error) {
	if records == nil {
		return nil, errors.New("finance: payment terms store nil")
	}
	rec, err := records.Get(ctx, tenantID, termsID)
	if err != nil {
		return nil, fmt.Errorf("finance: load payment_terms %s: %w", termsID, err)
	}
	if rec.KType != KTypePaymentTerms {
		return nil, fmt.Errorf("finance: %s is %s, not %s", termsID, rec.KType, KTypePaymentTerms)
	}
	var data struct {
		Active       bool                      `json:"active"`
		Installments []PaymentTermsInstallment `json:"installments"`
	}
	// `active` defaults to true in the schema but may be missing
	// from the JSON if the caller did not set it explicitly. Treat
	// "missing" as active to match the schema default.
	data.Active = true
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		return nil, fmt.Errorf("finance: decode payment_terms: %w", err)
	}
	if !data.Active {
		return nil, fmt.Errorf("%w: terms %s inactive", ErrInvalidPaymentTerms, termsID)
	}
	if len(data.Installments) == 0 {
		return nil, fmt.Errorf("%w: terms %s has no installments", ErrInvalidPaymentTerms, termsID)
	}
	return data.Installments, nil
}

// ComputePaymentSchedule materialises an installment plan against an
// invoice's issue date and total. The last installment absorbs any
// rounding remainder so the schedule's amounts always sum exactly to
// the invoice total — important so a downstream payment-application
// engine does not over- or under-allocate by a cent.
func ComputePaymentSchedule(
	issueDate time.Time,
	total decimal.Decimal,
	installments []PaymentTermsInstallment,
) ([]PaymentScheduleEntry, error) {
	if len(installments) == 0 {
		return nil, fmt.Errorf("%w: empty installments", ErrInvalidPaymentTerms)
	}
	// Sum the percentages; reject anything outside 99–101% so a
	// malformed template fails loudly rather than silently
	// dropping or doubling revenue. Tolerance accounts for
	// rounding in caller-authored floats (e.g. 33.33 + 33.33 +
	// 33.34 = 100.00 exactly, but 1/3 + 1/3 + 1/3 from the UI
	// might land at 99.99).
	pctSum := decimal.Zero
	for _, in := range installments {
		pctSum = pctSum.Add(in.Percentage)
	}
	hundred := decimal.NewFromInt(100)
	tolerance := decimal.NewFromFloat(0.5)
	if pctSum.Sub(hundred).Abs().GreaterThan(tolerance) {
		return nil, fmt.Errorf("%w: installment percentages sum to %s, expected ~100", ErrInvalidPaymentTerms, pctSum)
	}

	out := make([]PaymentScheduleEntry, 0, len(installments))
	allocated := decimal.Zero
	for i, in := range installments {
		due := issueDate.AddDate(0, 0, in.DueDays).Format(dateLayoutTerms)
		// amount = round(total * pct / 100, 2); the last row gets
		// the leftover so the sum reconciles exactly.
		var amount decimal.Decimal
		if i == len(installments)-1 {
			amount = total.Sub(allocated)
		} else {
			amount = total.Mul(in.Percentage).Div(hundred).Round(2)
			allocated = allocated.Add(amount)
		}
		entry := PaymentScheduleEntry{
			Installment: i + 1,
			Label:       in.Label,
			DueDate:     due,
			Percentage:  in.Percentage,
			Amount:      amount,
		}
		if in.DiscountDays > 0 && in.DiscountPercent.IsPositive() {
			discountAmount := amount.Mul(in.DiscountPercent).Div(hundred).Round(2)
			entry.DiscountAmount = discountAmount
			entry.DiscountUntil = issueDate.AddDate(0, 0, in.DiscountDays).Format(dateLayoutTerms)
		}
		out = append(out, entry)
	}
	return out, nil
}

const dateLayoutTerms = "2006-01-02"
