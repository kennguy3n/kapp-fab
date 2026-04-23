package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// PaymentPoster converts a finance.payment KRecord into a balanced
// journal entry and reconciles the allocated invoices/bills by
// reducing their outstanding_amount. Receive payments debit the bank
// and credit AR; pay payments debit AP and credit bank.
//
// Like InvoicePoster, the ledger insert and KRecord patches run in
// separate tenant-scoped transactions. The partial unique index on
// (tenant_id, source_ktype, source_id) covers the retry-after-failure
// case at the DB level — a second PostPayment call will hit
// ErrDuplicateSourceEntry and we reload the prior entry instead of
// double-posting.
type PaymentPoster struct {
	store       *PGStore
	recordStore *record.PGStore
}

// NewPaymentPoster wires the poster against an existing ledger and
// record store.
func NewPaymentPoster(store *PGStore, recordStore *record.PGStore) *PaymentPoster {
	return &PaymentPoster{store: store, recordStore: recordStore}
}

// paymentData is the slice of finance.payment schema fields the poster
// reads. The allocations array is decoded separately so each entry can
// be validated against the referenced invoice/bill.
type paymentData struct {
	PaymentType    string                `json:"payment_type"`
	PartyType      string                `json:"party_type"`
	PartyID        string                `json:"party_id"`
	Amount         decimal.Decimal       `json:"amount"`
	Currency       string                `json:"currency"`
	PaymentDate    string                `json:"payment_date"`
	Reference      string                `json:"reference"`
	Allocations    []PaymentAllocation   `json:"allocations"`
	Status         string                `json:"status"`
	BankAccount    string                `json:"bank_account"`
	ARAccountCode  string                `json:"ar_account_code"`
	APAccountCode  string                `json:"ap_account_code"`
	JournalEntryID string                `json:"journal_entry_id"`
}

// PaymentAllocation is a single {invoice_id, allocated_amount} tuple on
// a payment. Exported so tests and the record-patch path share the
// same shape.
type PaymentAllocation struct {
	InvoiceID        string          `json:"invoice_id"`
	AllocatedAmount  decimal.Decimal `json:"allocated_amount"`
	// OutstandingBefore is informational, populated by PostPayment in
	// the returned patch so the UI can render a payment receipt.
	OutstandingBefore decimal.Decimal `json:"outstanding_before,omitempty"`
	OutstandingAfter  decimal.Decimal `json:"outstanding_after,omitempty"`
}

// Sentinel errors specific to payment posting.
var (
	ErrPaymentNotPostable     = errors.New("ledger: payment not postable from current status")
	ErrPaymentAlreadyPosted   = errors.New("ledger: payment already posted")
	ErrPaymentAllocationExcess = errors.New("ledger: allocation exceeds invoice outstanding")
	ErrPaymentAllocationMismatch = errors.New("ledger: total allocations exceed payment amount")
)

// PostPayment posts a finance.payment KRecord and reconciles each
// referenced invoice/bill. For a receive payment:
//
//	Dr Bank Account         amount
//	Cr Accounts Receivable  amount
//
// For a pay payment:
//
//	Dr Accounts Payable     amount
//	Cr Bank Account         amount
//
// After the JE commits, each allocation decrements the target invoice
// or bill's outstanding_amount. If the remainder hits zero the record
// also flips to status=paid.
func (p *PaymentPoster) PostPayment(ctx context.Context, tenantID, paymentID, actorID uuid.UUID) (*JournalEntry, error) {
	if p.recordStore == nil || p.store == nil {
		return nil, errors.New("ledger: payment poster not fully wired")
	}
	if tenantID == uuid.Nil || paymentID == uuid.Nil {
		return nil, errors.New("ledger: tenant id and payment id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("ledger: actor id required")
	}

	rec, err := p.recordStore.Get(ctx, tenantID, paymentID)
	if err != nil {
		return nil, err
	}
	if rec.KType != "finance.payment" {
		return nil, fmt.Errorf("%w: expected finance.payment, got %s", ErrSourceMismatch, rec.KType)
	}

	var pay paymentData
	if err := json.Unmarshal(rec.Data, &pay); err != nil {
		return nil, fmt.Errorf("ledger: decode payment: %w", err)
	}
	if pay.Status == "submitted" || pay.JournalEntryID != "" {
		return nil, ErrPaymentAlreadyPosted
	}
	if pay.Status != "" && pay.Status != "draft" {
		return nil, fmt.Errorf("%w: status=%s", ErrPaymentNotPostable, pay.Status)
	}
	if pay.PaymentType != "receive" && pay.PaymentType != "pay" {
		return nil, fmt.Errorf("ledger: payment_type must be receive or pay, got %q", pay.PaymentType)
	}
	if pay.BankAccount == "" {
		return nil, errors.New("ledger: bank_account required")
	}
	if pay.PaymentType == "receive" && pay.ARAccountCode == "" {
		return nil, errors.New("ledger: ar_account_code required for receive payment")
	}
	if pay.PaymentType == "pay" && pay.APAccountCode == "" {
		return nil, errors.New("ledger: ap_account_code required for pay payment")
	}
	if !pay.Amount.IsPositive() {
		return nil, errors.New("ledger: amount must be positive")
	}
	currency := pay.Currency
	if currency == "" {
		currency = "USD"
	}

	// Validate allocations do not sum to more than amount.
	allocTotal := decimal.Zero
	for _, a := range pay.Allocations {
		if !a.AllocatedAmount.IsPositive() {
			return nil, errors.New("ledger: allocated_amount must be positive")
		}
		allocTotal = allocTotal.Add(a.AllocatedAmount)
	}
	if allocTotal.GreaterThan(pay.Amount) {
		return nil, fmt.Errorf("%w: allocations=%s amount=%s", ErrPaymentAllocationMismatch, allocTotal, pay.Amount)
	}

	// Reuse any prior JE for this payment — see the matching comment in
	// PostSalesInvoice. Guards against retries and concurrent callers.
	existing, err := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.payment", paymentID)
	if err != nil && !errors.Is(err, ErrEntryNotFound) {
		return nil, err
	}

	var entry *JournalEntry
	if existing != nil {
		entry = existing
	} else {
		var lines []JournalLine
		memo := fmt.Sprintf("Payment %s", pay.Reference)
		if pay.PaymentType == "receive" {
			lines = []JournalLine{
				{AccountCode: pay.BankAccount, Debit: pay.Amount, Credit: decimal.Zero, Currency: currency, Memo: memoFor(pay.Reference, "Bank")},
				{AccountCode: pay.ARAccountCode, Debit: decimal.Zero, Credit: pay.Amount, Currency: currency, Memo: memoFor(pay.Reference, "AR")},
			}
		} else {
			lines = []JournalLine{
				{AccountCode: pay.APAccountCode, Debit: pay.Amount, Credit: decimal.Zero, Currency: currency, Memo: memoFor(pay.Reference, "AP")},
				{AccountCode: pay.BankAccount, Debit: decimal.Zero, Credit: pay.Amount, Currency: currency, Memo: memoFor(pay.Reference, "Bank")},
			}
		}
		postedAt := parseInvoiceDate(pay.PaymentDate, p.store.now)
		sourceID := paymentID
		posted, postErr := p.store.PostJournalEntry(ctx, JournalEntry{
			TenantID:    tenantID,
			PostedAt:    postedAt,
			Memo:        memo,
			SourceKType: "finance.payment",
			SourceID:    &sourceID,
			CreatedBy:   actorID,
			Lines:       lines,
		})
		if postErr != nil {
			if errors.Is(postErr, ErrDuplicateSourceEntry) {
				reloaded, reloadErr := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.payment", paymentID)
				if reloadErr != nil {
					return nil, fmt.Errorf("ledger: reload duplicate entry: %w", reloadErr)
				}
				posted = reloaded
			} else {
				return nil, postErr
			}
		}
		entry = posted
	}

	// Patch the payment KRecord: status=submitted + journal_entry_id.
	patch := map[string]any{
		"status":           "submitted",
		"journal_entry_id": entry.ID.String(),
	}
	patchJSON, _ := json.Marshal(patch)
	if _, err := p.recordStore.Update(ctx, record.KRecord{
		ID:        rec.ID,
		TenantID:  tenantID,
		Version:   rec.Version,
		Data:      patchJSON,
		UpdatedBy: ptrUUID(actorID),
	}); err != nil {
		return entry, fmt.Errorf("ledger: patch payment: %w", err)
	}

	// Reconcile each allocation: decrement the target record's
	// outstanding_amount and flip its status to "paid" when it hits
	// zero. We read the invoice/bill fresh for each allocation so the
	// optimistic-concurrency check catches a racing payment that might
	// have settled part of the same invoice.
	targetKType := "finance.ar_invoice"
	if pay.PaymentType == "pay" {
		targetKType = "finance.ap_bill"
	}
	for _, a := range pay.Allocations {
		invID, parseErr := uuid.Parse(a.InvoiceID)
		if parseErr != nil {
			continue
		}
		if err := p.applyAllocation(ctx, tenantID, invID, targetKType, a.AllocatedAmount, actorID); err != nil {
			return entry, err
		}
	}

	// Lifecycle event so webhook/notification consumers can hook on the
	// payment posting specifically rather than the generic journal tick.
	eventPayload := map[string]any{
		"payment_id":       paymentID,
		"payment_type":     pay.PaymentType,
		"party_type":       pay.PartyType,
		"party_id":         pay.PartyID,
		"amount":           pay.Amount.String(),
		"currency":         currency,
		"journal_entry_id": entry.ID,
		"actor":            actorID,
	}
	poster := &InvoicePoster{store: p.store}
	if err := poster.emitSourceEvent(ctx, tenantID, "finance.payment.posted", eventPayload); err != nil {
		return entry, err
	}
	return entry, nil
}

// applyAllocation patches a single invoice/bill record with a reduced
// outstanding_amount. When the remainder reaches zero the record is
// also transitioned to status=paid so the ERP view reflects full
// settlement.
func (p *PaymentPoster) applyAllocation(ctx context.Context, tenantID, recID uuid.UUID, ktype string, amount decimal.Decimal, actorID uuid.UUID) error {
	rec, err := p.recordStore.Get(ctx, tenantID, recID)
	if err != nil {
		return err
	}
	if rec.KType != ktype {
		return fmt.Errorf("%w: allocation target expected %s, got %s", ErrSourceMismatch, ktype, rec.KType)
	}
	var target struct {
		Total              decimal.Decimal `json:"total"`
		OutstandingAmount  decimal.Decimal `json:"outstanding_amount"`
		Status             string          `json:"status"`
	}
	if err := json.Unmarshal(rec.Data, &target); err != nil {
		return fmt.Errorf("ledger: decode allocation target: %w", err)
	}
	// Outstanding defaults to total when the field is missing or zero
	// and the invoice was previously posted — the first payment to
	// settle this invoice seeds the remainder.
	outstanding := target.OutstandingAmount
	if outstanding.IsZero() && target.Status == "posted" {
		outstanding = target.Total
	}
	if amount.GreaterThan(outstanding) {
		return fmt.Errorf("%w: invoice=%s outstanding=%s allocated=%s",
			ErrPaymentAllocationExcess, recID, outstanding, amount)
	}
	newOutstanding := outstanding.Sub(amount)
	patch := map[string]any{
		"outstanding_amount": newOutstanding,
	}
	if newOutstanding.IsZero() {
		patch["status"] = "paid"
	}
	patchJSON, _ := json.Marshal(patch)
	_, err = p.recordStore.Update(ctx, record.KRecord{
		ID:        rec.ID,
		TenantID:  tenantID,
		Version:   rec.Version,
		Data:      patchJSON,
		UpdatedBy: ptrUUID(actorID),
	})
	return err
}
