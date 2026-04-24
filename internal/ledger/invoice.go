package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// InvoicePoster turns a finance.ar_invoice or finance.ap_bill KRecord
// into a balanced journal entry. It depends on the record store so it
// can load the source record, run the posting, and then patch the
// record with the resulting journal_entry_id + status in the same
// tenant context. The record.PGStore itself uses WithTenantTx, so each
// Update/Get opens its own transaction — the posting is still atomic
// at the ledger level (journal + event + audit share one tx); the
// record update is a follow-on tenant-scoped write.
//
// The JE insert and the record patch therefore run in separate
// transactions, which is a well-known two-phase-commit hazard. The
// poster relies on two safety nets so retries and concurrent callers
// cannot double-post:
//
//  1. The source KRecord write uses optimistic concurrency (rec.Version
//     is threaded into recordStore.Update), so two concurrent posters
//     cannot both flip status=draft → posted on the same record.
//  2. The `journal_entries_source_uniq` partial index (see
//     migrations/000004_finance_extensions.sql) enforces one JE per
//     (tenant_id, source_ktype, source_id). PostJournalEntry translates
//     the resulting 23505 into ErrDuplicateSourceEntry, and the poster
//     reloads the existing entry and patches the record against *that*
//     JE id instead of inserting a duplicate.
//
// Together these cover the retry-after-partial-failure case (prior JE
// exists, record still says draft) and the concurrent-poster race
// (two callers reach PostJournalEntry; one wins, the other reuses).
type InvoicePoster struct {
	store             *PGStore
	recordStore       *record.PGStore
	salesInvoiceHook  PostHook
	purchaseBillHook  PostHook
}

// PostHook is an opt-in callback invoked after a sales invoice or
// purchase bill has been fully posted (journal entry committed, source
// KRecord patched, lifecycle event emitted). The Phase D inventory
// wiring uses this to append goods-delivery / goods-receipt moves on
// the back of posted invoices and bills without importing the
// inventory package into the ledger; the hook signature is deliberately
// generic so unrelated consumers (notifications, webhooks, …) can
// plug in later.
//
// Hook errors are returned to the caller but do NOT roll back the
// journal entry: the ledger transaction has already committed by this
// point, so the only sane semantic is "post succeeded, side-effect
// failed" — the caller sees a 500 and the idempotency guards on the
// inventory side (partial unique index on source_id) make the retry
// safe.
type PostHook func(ctx context.Context, tenantID uuid.UUID, rec *record.KRecord, entry *JournalEntry, actorID uuid.UUID) error

// NewInvoicePoster wires the poster against an existing ledger and record
// store. Both are required; nil returns a non-functional poster.
func NewInvoicePoster(store *PGStore, recordStore *record.PGStore) *InvoicePoster {
	return &InvoicePoster{store: store, recordStore: recordStore}
}

// WithSalesInvoiceHook attaches a post-commit callback to sales
// invoice posting. Returns the receiver for chaining.
func (p *InvoicePoster) WithSalesInvoiceHook(h PostHook) *InvoicePoster {
	p.salesInvoiceHook = h
	return p
}

// WithPurchaseBillHook attaches a post-commit callback to purchase
// bill posting. Returns the receiver for chaining.
func (p *InvoicePoster) WithPurchaseBillHook(h PostHook) *InvoicePoster {
	p.purchaseBillHook = h
	return p
}

// invoiceData is the slice of finance.ar_invoice schema fields we need
// to post. Kept small + JSON-tagged so we can Unmarshal directly from
// KRecord.Data without hand-rolled map access.
type invoiceData struct {
	CustomerID         string          `json:"customer_id"`
	DealID             string          `json:"deal_id"`
	InvoiceNumber      string          `json:"invoice_number"`
	IssueDate          string          `json:"issue_date"`
	DueDate            string          `json:"due_date"`
	Subtotal           decimal.Decimal `json:"subtotal"`
	TaxCode            string          `json:"tax_code"`
	TaxAmount          decimal.Decimal `json:"tax_amount"`
	Total              decimal.Decimal `json:"total"`
	Currency           string          `json:"currency"`
	Status             string          `json:"status"`
	JournalEntryID     string          `json:"journal_entry_id"`
	ARAccountCode      string          `json:"ar_account_code"`
	RevenueAccountCode string          `json:"revenue_account_code"`
	TaxAccountCode     string          `json:"tax_account_code"`
}

// billData mirrors invoiceData for finance.ap_bill. Kept as a separate
// type so the different account-code field names (ap/expense) stay
// explicit.
type billData struct {
	SupplierID         string          `json:"supplier_id"`
	BillNumber         string          `json:"bill_number"`
	IssueDate          string          `json:"issue_date"`
	DueDate            string          `json:"due_date"`
	Subtotal           decimal.Decimal `json:"subtotal"`
	TaxCode            string          `json:"tax_code"`
	TaxAmount          decimal.Decimal `json:"tax_amount"`
	Total              decimal.Decimal `json:"total"`
	Currency           string          `json:"currency"`
	Status             string          `json:"status"`
	JournalEntryID     string          `json:"journal_entry_id"`
	APAccountCode      string          `json:"ap_account_code"`
	ExpenseAccountCode string          `json:"expense_account_code"`
	TaxAccountCode     string          `json:"tax_account_code"`
}

// PostSalesInvoice posts a finance.ar_invoice KRecord to the ledger.
//
// The generated journal has three legs at most:
//
//	Dr Accounts Receivable  total
//	Cr Revenue              subtotal
//	Cr Tax Payable          tax_amount       (omitted when tax_amount == 0)
//
// The invoice KRecord is patched with status = "posted" and
// journal_entry_id = <new JE id>. The invoice record must currently be
// in "draft" or "pending_approval" status; other statuses return
// ErrInvoiceNotPostable so replayed posts are explicit rather than
// silent.
func (p *InvoicePoster) PostSalesInvoice(ctx context.Context, tenantID, invoiceID, actorID uuid.UUID) (*JournalEntry, error) {
	if p.recordStore == nil || p.store == nil {
		return nil, errors.New("ledger: poster not fully wired")
	}
	if tenantID == uuid.Nil || invoiceID == uuid.Nil {
		return nil, errors.New("ledger: tenant id and invoice id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("ledger: actor id required")
	}

	rec, err := p.recordStore.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return nil, err
	}
	if rec.KType != "finance.ar_invoice" {
		return nil, fmt.Errorf("%w: expected finance.ar_invoice, got %s", ErrSourceMismatch, rec.KType)
	}

	var inv invoiceData
	if err := json.Unmarshal(rec.Data, &inv); err != nil {
		return nil, fmt.Errorf("ledger: decode invoice: %w", err)
	}
	if inv.Status == "posted" || inv.JournalEntryID != "" {
		return nil, ErrInvoiceAlreadyPosted
	}
	if inv.Status != "" && inv.Status != "draft" && inv.Status != "pending_approval" {
		return nil, fmt.Errorf("%w: status=%s", ErrInvoiceNotPostable, inv.Status)
	}
	if inv.ARAccountCode == "" || inv.RevenueAccountCode == "" {
		return nil, errors.New("ledger: ar_account_code and revenue_account_code required")
	}
	currency := inv.Currency
	if currency == "" {
		currency = "USD"
	}
	if inv.Total.IsZero() {
		inv.Total = inv.Subtotal.Add(inv.TaxAmount)
	}

	// Credit-limit enforcement — mirrors ERPNext's credit check.
	// Only runs when the invoice references a customer KRecord and
	// that customer has credit_limit > 0 on its data JSONB. The
	// check sums every un-settled AR invoice for the same customer
	// (status NOT IN paid/cancelled/voided) and rejects the post if
	// adding this invoice's total would push the outstanding past
	// the limit. A zero or missing credit_limit disables the check
	// so existing tenants keep posting without data migration.
	if err := p.checkCreditLimit(ctx, tenantID, invoiceID, inv); err != nil {
		return nil, err
	}

	// If a previous invocation already posted the JE but failed before
	// patching the source record, reuse that entry instead of creating
	// a duplicate. The partial unique index on (tenant_id, source_ktype,
	// source_id) is the DB-level guarantee; this is the happy-path
	// fast-check that avoids an attempted insert when we already know it
	// would 23505.
	existing, err := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.ar_invoice", invoiceID)
	if err != nil && !errors.Is(err, ErrEntryNotFound) {
		return nil, err
	}

	var entry *JournalEntry
	if existing != nil {
		entry = existing
	} else {
		lines := []JournalLine{
			{AccountCode: inv.ARAccountCode, Debit: inv.Total, Credit: decimal.Zero, Currency: currency, Memo: memoFor(inv.InvoiceNumber, "AR")},
			{AccountCode: inv.RevenueAccountCode, Debit: decimal.Zero, Credit: inv.Subtotal, Currency: currency, Memo: memoFor(inv.InvoiceNumber, "Revenue")},
		}
		if inv.TaxAmount.IsPositive() {
			if inv.TaxAccountCode == "" {
				return nil, errors.New("ledger: tax_account_code required when tax_amount > 0")
			}
			lines = append(lines, JournalLine{
				AccountCode: inv.TaxAccountCode, Debit: decimal.Zero, Credit: inv.TaxAmount,
				Currency: currency, Memo: memoFor(inv.InvoiceNumber, "Tax"),
			})
		}

		postedAt := parseInvoiceDate(inv.IssueDate, p.store.now)
		sourceID := invoiceID
		posted, postErr := p.store.PostJournalEntry(ctx, JournalEntry{
			TenantID:    tenantID,
			PostedAt:    postedAt,
			Memo:        fmt.Sprintf("Sales invoice %s", inv.InvoiceNumber),
			SourceKType: "finance.ar_invoice",
			SourceID:    &sourceID,
			CreatedBy:   actorID,
			Lines:       lines,
		})
		if postErr != nil {
			// A concurrent poster beat us to the insert. Fall back to
			// the entry it committed so the record patch completes.
			if errors.Is(postErr, ErrDuplicateSourceEntry) {
				reloaded, reloadErr := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.ar_invoice", invoiceID)
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

	// Patch the invoice KRecord with status=posted + journal_entry_id
	// + outstanding_amount=total. Seeding outstanding_amount here
	// rather than deferring to the first payment means AR dashboards
	// and the credit-limit check (see checkCreditLimit) see the full
	// balance immediately after posting. Shallow-merge keeps other
	// fields (customer_id, lines, …) intact. Threading rec.Version
	// turns the patch into a compare-and-swap so two concurrent
	// posters cannot both claim the record; the loser gets
	// record.ErrVersionConflict and the caller retries (at which
	// point the rec.Status=='posted' guard above short-circuits).
	totalF, _ := inv.Total.Float64()
	patch := map[string]any{
		"status":             "posted",
		"journal_entry_id":   entry.ID.String(),
		"outstanding_amount": totalF,
	}
	patchJSON, _ := json.Marshal(patch)
	if _, err := p.recordStore.Update(ctx, record.KRecord{
		ID:        rec.ID,
		TenantID:  tenantID,
		Version:   rec.Version,
		Data:      patchJSON,
		UpdatedBy: ptrUUID(actorID),
	}); err != nil {
		return entry, fmt.Errorf("ledger: patch invoice: %w", err)
	}

	// Lifecycle event — consumers (e.g. KChat poster, email dispatcher)
	// can hook on this rather than the generic journal.posted emission.
	if err := p.emitSourceEvent(ctx, tenantID, "finance.sales_invoice.posted", map[string]any{
		"invoice_id":       invoiceID,
		"invoice_number":   inv.InvoiceNumber,
		"journal_entry_id": entry.ID,
		"total":            inv.Total.String(),
		"currency":         currency,
		"actor":            actorID,
	}); err != nil {
		return entry, err
	}
	if p.salesInvoiceHook != nil {
		if err := p.salesInvoiceHook(ctx, tenantID, rec, entry, actorID); err != nil {
			return entry, fmt.Errorf("ledger: sales invoice hook: %w", err)
		}
	}
	return entry, nil
}

// checkCreditLimit enforces the customer's credit_limit against the
// running outstanding AR balance. The query and the customer lookup
// share a single tenant-scoped transaction so the balance is
// snapshot-consistent; RLS via dbutil.WithTenantTx blocks cross-tenant
// reads even if an attacker hand-rolls a customer_id from another
// tenant. Returns ErrCreditLimitExceeded (wrapped with the numeric
// context) when the new invoice would push outstanding past the
// limit; nil in every other case — including missing customer,
// unset/zero credit_limit, or malformed customer_id — so posting
// remains permissive by default.
func (p *InvoicePoster) checkCreditLimit(ctx context.Context, tenantID, invoiceID uuid.UUID, inv invoiceData) error {
	if inv.CustomerID == "" {
		return nil
	}
	customerID, err := uuid.Parse(inv.CustomerID)
	if err != nil {
		// Invalid UUID — schema validation will reject this at the
		// record layer, but guard here so we do not bubble up a raw
		// parse error from a credit check the caller did not ask for.
		return nil
	}
	if inv.Total.LessThanOrEqual(decimal.Zero) {
		return nil
	}
	var (
		creditLimit decimal.Decimal
		outstanding decimal.Decimal
	)
	qErr := dbutil.WithTenantTx(ctx, p.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var rawLimit *string
		if err := tx.QueryRow(ctx,
			`SELECT data->>'credit_limit'
			   FROM krecords
			  WHERE tenant_id = $1 AND id = $2 AND ktype = 'crm.customer'
			    AND deleted_at IS NULL`,
			tenantID, customerID,
		).Scan(&rawLimit); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// No customer record — posting permissive.
				return errSkipCreditCheck
			}
			return fmt.Errorf("ledger: load customer credit limit: %w", err)
		}
		if rawLimit == nil || *rawLimit == "" {
			return errSkipCreditCheck
		}
		parsedLimit, err := decimal.NewFromString(*rawLimit)
		if err != nil {
			return errSkipCreditCheck
		}
		creditLimit = parsedLimit
		if creditLimit.LessThanOrEqual(decimal.Zero) {
			return errSkipCreditCheck
		}
		// Running outstanding AR for this customer. Draft invoices
		// naturally drop out because only posted invoices seed
		// outstanding_amount (see PostSalesInvoice patch above).
		// Excludes the invoice currently being posted so its own
		// (soon-to-be-set) outstanding isn't counted twice alongside
		// inv.Total in the projection below.
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(
			          COALESCE(NULLIF(data->>'outstanding_amount','')::numeric, 0)), 0)
			   FROM krecords
			  WHERE tenant_id = $1 AND ktype = 'finance.ar_invoice'
			    AND id <> $3
			    AND data->>'customer_id' = $2
			    AND COALESCE(data->>'status','') NOT IN ('paid','cancelled','voided')
			    AND deleted_at IS NULL`,
			tenantID, customerID.String(), invoiceID,
		).Scan(&outstanding); err != nil {
			return fmt.Errorf("ledger: sum outstanding AR: %w", err)
		}
		return nil
	})
	if errors.Is(qErr, errSkipCreditCheck) {
		return nil
	}
	if qErr != nil {
		return qErr
	}
	projected := outstanding.Add(inv.Total)
	if projected.GreaterThan(creditLimit) {
		return fmt.Errorf("%w: customer=%s limit=%s outstanding=%s invoice=%s",
			ErrCreditLimitExceeded, customerID,
			creditLimit.String(), outstanding.String(), inv.Total.String())
	}
	return nil
}

// errSkipCreditCheck is an internal sentinel the credit check uses to
// short-circuit out of the WithTenantTx closure without treating the
// skip as an error at the caller level. It never leaves this file.
var errSkipCreditCheck = errors.New("ledger: credit check not applicable")

// PostPurchaseBill mirrors PostSalesInvoice for finance.ap_bill:
//
//	Dr Expense              subtotal
//	Dr Tax Receivable       tax_amount       (omitted when zero)
//	Cr Accounts Payable     total
func (p *InvoicePoster) PostPurchaseBill(ctx context.Context, tenantID, billID, actorID uuid.UUID) (*JournalEntry, error) {
	if p.recordStore == nil || p.store == nil {
		return nil, errors.New("ledger: poster not fully wired")
	}
	if tenantID == uuid.Nil || billID == uuid.Nil {
		return nil, errors.New("ledger: tenant id and bill id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("ledger: actor id required")
	}

	rec, err := p.recordStore.Get(ctx, tenantID, billID)
	if err != nil {
		return nil, err
	}
	if rec.KType != "finance.ap_bill" {
		return nil, fmt.Errorf("%w: expected finance.ap_bill, got %s", ErrSourceMismatch, rec.KType)
	}

	var bill billData
	if err := json.Unmarshal(rec.Data, &bill); err != nil {
		return nil, fmt.Errorf("ledger: decode bill: %w", err)
	}
	if bill.Status == "posted" || bill.JournalEntryID != "" {
		return nil, ErrInvoiceAlreadyPosted
	}
	if bill.Status != "" && bill.Status != "draft" && bill.Status != "pending_approval" {
		return nil, fmt.Errorf("%w: status=%s", ErrInvoiceNotPostable, bill.Status)
	}
	if bill.APAccountCode == "" || bill.ExpenseAccountCode == "" {
		return nil, errors.New("ledger: ap_account_code and expense_account_code required")
	}
	currency := bill.Currency
	if currency == "" {
		currency = "USD"
	}
	if bill.Total.IsZero() {
		bill.Total = bill.Subtotal.Add(bill.TaxAmount)
	}

	// Reuse any prior JE for this bill — see the matching comment in
	// PostSalesInvoice.
	existing, err := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.ap_bill", billID)
	if err != nil && !errors.Is(err, ErrEntryNotFound) {
		return nil, err
	}

	var entry *JournalEntry
	if existing != nil {
		entry = existing
	} else {
		lines := []JournalLine{
			{AccountCode: bill.ExpenseAccountCode, Debit: bill.Subtotal, Credit: decimal.Zero, Currency: currency, Memo: memoFor(bill.BillNumber, "Expense")},
		}
		if bill.TaxAmount.IsPositive() {
			if bill.TaxAccountCode == "" {
				return nil, errors.New("ledger: tax_account_code required when tax_amount > 0")
			}
			lines = append(lines, JournalLine{
				AccountCode: bill.TaxAccountCode, Debit: bill.TaxAmount, Credit: decimal.Zero,
				Currency: currency, Memo: memoFor(bill.BillNumber, "Tax"),
			})
		}
		lines = append(lines, JournalLine{
			AccountCode: bill.APAccountCode, Debit: decimal.Zero, Credit: bill.Total,
			Currency: currency, Memo: memoFor(bill.BillNumber, "AP"),
		})

		postedAt := parseInvoiceDate(bill.IssueDate, p.store.now)
		sourceID := billID
		posted, postErr := p.store.PostJournalEntry(ctx, JournalEntry{
			TenantID:    tenantID,
			PostedAt:    postedAt,
			Memo:        fmt.Sprintf("Purchase bill %s", bill.BillNumber),
			SourceKType: "finance.ap_bill",
			SourceID:    &sourceID,
			CreatedBy:   actorID,
			Lines:       lines,
		})
		if postErr != nil {
			if errors.Is(postErr, ErrDuplicateSourceEntry) {
				reloaded, reloadErr := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.ap_bill", billID)
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

	patch := map[string]any{
		"status":           "posted",
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
		return entry, fmt.Errorf("ledger: patch bill: %w", err)
	}

	if err := p.emitSourceEvent(ctx, tenantID, "finance.ap_bill.posted", map[string]any{
		"bill_id":          billID,
		"bill_number":      bill.BillNumber,
		"journal_entry_id": entry.ID,
		"total":            bill.Total.String(),
		"currency":         currency,
		"actor":            actorID,
	}); err != nil {
		return entry, err
	}
	if p.purchaseBillHook != nil {
		if err := p.purchaseBillHook(ctx, tenantID, rec, entry, actorID); err != nil {
			return entry, fmt.Errorf("ledger: purchase bill hook: %w", err)
		}
	}
	return entry, nil
}

// emitSourceEvent writes a lifecycle event (not the generic journal one)
// under its own short tenant-scoped transaction. Used from invoice/bill
// posting after the ledger tx already committed.
func (p *InvoicePoster) emitSourceEvent(ctx context.Context, tenantID uuid.UUID, eventType string, payload map[string]any) error {
	if p.store.publisher == nil && p.store.auditor == nil {
		return nil
	}
	body, _ := json.Marshal(payload)
	return dbutil.WithTenantTx(ctx, p.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if p.store.publisher != nil {
			if err := p.store.publisher.EmitTx(ctx, tx, events.Event{
				TenantID: tenantID, Type: eventType, Payload: body,
			}); err != nil {
				return err
			}
		}
		if p.store.auditor != nil {
			var actor *uuid.UUID
			if v, ok := payload["actor"].(uuid.UUID); ok {
				a := v
				actor = &a
			}
			if err := p.store.auditor.LogTx(ctx, tx, audit.Entry{
				TenantID:  tenantID,
				ActorID:   actor,
				ActorKind: audit.ActorUser,
				Action:    eventType,
				After:     body,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func memoFor(number, leg string) string {
	if number == "" {
		return leg
	}
	return fmt.Sprintf("%s %s", number, leg)
}

func parseInvoiceDate(raw string, now func() time.Time) time.Time {
	if raw == "" {
		return now()
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC()
	}
	return now()
}

func ptrUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// creditNoteData is the slice of finance.credit_note schema fields we
// need to post. `original_invoice_id` is required; the note inherits
// the invoice's account codes + currency so the user cannot redirect
// the reversal to the wrong ledger leg.
type creditNoteData struct {
	OriginalInvoiceID string          `json:"original_invoice_id"`
	CreditNoteNumber  string          `json:"credit_note_number"`
	IssueDate         string          `json:"issue_date"`
	Reason            string          `json:"reason"`
	Amount            decimal.Decimal `json:"amount"`
	Currency          string          `json:"currency"`
	Status            string          `json:"status"`
	JournalEntryID    string          `json:"journal_entry_id"`
}

// debitNoteData mirrors creditNoteData for finance.debit_note. The
// `original_bill_id` drives the AP→Expense reversal.
type debitNoteData struct {
	OriginalBillID  string          `json:"original_bill_id"`
	DebitNoteNumber string          `json:"debit_note_number"`
	IssueDate       string          `json:"issue_date"`
	Reason          string          `json:"reason"`
	Amount          decimal.Decimal `json:"amount"`
	Currency        string          `json:"currency"`
	Status          string          `json:"status"`
	JournalEntryID  string          `json:"journal_entry_id"`
}

// PostCreditNote posts a finance.credit_note KRecord to the ledger.
// The generated journal reverses the AR posting of the referenced
// invoice:
//
//	Dr Revenue          amount
//	Cr Accounts Receivable  amount
//
// Account codes are read off the posted source invoice so the reversal
// always targets the same AR / Revenue legs the original invoice used.
// The credit note KRecord must be in "draft" status; posting patches
// it to "posted" and stamps `journal_entry_id`. The referenced invoice
// must itself be posted (status="posted" or "paid") — you cannot credit
// an invoice that never hit the ledger.
func (p *InvoicePoster) PostCreditNote(ctx context.Context, tenantID, creditNoteID, actorID uuid.UUID) (*JournalEntry, error) {
	if p.recordStore == nil || p.store == nil {
		return nil, errors.New("ledger: poster not fully wired")
	}
	if tenantID == uuid.Nil || creditNoteID == uuid.Nil {
		return nil, errors.New("ledger: tenant id and credit note id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("ledger: actor id required")
	}

	rec, err := p.recordStore.Get(ctx, tenantID, creditNoteID)
	if err != nil {
		return nil, err
	}
	if rec.KType != "finance.credit_note" {
		return nil, fmt.Errorf("%w: expected finance.credit_note, got %s", ErrSourceMismatch, rec.KType)
	}

	var note creditNoteData
	if err := json.Unmarshal(rec.Data, &note); err != nil {
		return nil, fmt.Errorf("ledger: decode credit note: %w", err)
	}
	if note.Status == "posted" || note.JournalEntryID != "" {
		return nil, ErrCreditNoteAlreadyPosted
	}
	if note.Status != "" && note.Status != "draft" {
		return nil, fmt.Errorf("%w: status=%s", ErrCreditNoteNotPostable, note.Status)
	}
	if note.OriginalInvoiceID == "" {
		return nil, errors.New("ledger: original_invoice_id required")
	}
	if !note.Amount.IsPositive() {
		return nil, errors.New("ledger: credit note amount must be positive")
	}
	invoiceID, err := uuid.Parse(note.OriginalInvoiceID)
	if err != nil {
		return nil, fmt.Errorf("ledger: invalid original_invoice_id: %w", err)
	}

	invRec, err := p.recordStore.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("ledger: load original invoice: %w", err)
	}
	if invRec.KType != "finance.ar_invoice" {
		return nil, fmt.Errorf("%w: original must be finance.ar_invoice, got %s", ErrSourceMismatch, invRec.KType)
	}
	var inv invoiceData
	if err := json.Unmarshal(invRec.Data, &inv); err != nil {
		return nil, fmt.Errorf("ledger: decode original invoice: %w", err)
	}
	if inv.Status != "posted" && inv.Status != "paid" {
		return nil, fmt.Errorf("%w: invoice status=%s", ErrOriginalNotPosted, inv.Status)
	}
	if inv.ARAccountCode == "" || inv.RevenueAccountCode == "" {
		return nil, errors.New("ledger: original invoice missing account codes")
	}
	currency := note.Currency
	if currency == "" {
		currency = inv.Currency
	}
	if currency == "" {
		currency = "USD"
	}

	existing, err := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.credit_note", creditNoteID)
	if err != nil && !errors.Is(err, ErrEntryNotFound) {
		return nil, err
	}

	var entry *JournalEntry
	if existing != nil {
		entry = existing
	} else {
		lines := []JournalLine{
			{AccountCode: inv.RevenueAccountCode, Debit: note.Amount, Credit: decimal.Zero, Currency: currency, Memo: memoFor(note.CreditNoteNumber, "Revenue reversal")},
			{AccountCode: inv.ARAccountCode, Debit: decimal.Zero, Credit: note.Amount, Currency: currency, Memo: memoFor(note.CreditNoteNumber, "AR reversal")},
		}
		postedAt := parseInvoiceDate(note.IssueDate, p.store.now)
		sourceID := creditNoteID
		posted, postErr := p.store.PostJournalEntry(ctx, JournalEntry{
			TenantID:    tenantID,
			PostedAt:    postedAt,
			Memo:        fmt.Sprintf("Credit note %s", note.CreditNoteNumber),
			SourceKType: "finance.credit_note",
			SourceID:    &sourceID,
			CreatedBy:   actorID,
			Lines:       lines,
		})
		if postErr != nil {
			if errors.Is(postErr, ErrDuplicateSourceEntry) {
				reloaded, reloadErr := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.credit_note", creditNoteID)
				if reloadErr != nil {
					return nil, fmt.Errorf("ledger: reload duplicate credit-note entry: %w", reloadErr)
				}
				posted = reloaded
			} else {
				return nil, postErr
			}
		}
		entry = posted
	}

	patch := map[string]any{
		"status":           "posted",
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
		return entry, fmt.Errorf("ledger: patch credit note: %w", err)
	}

	if err := p.emitSourceEvent(ctx, tenantID, "finance.credit_note.posted", map[string]any{
		"credit_note_id":      creditNoteID,
		"credit_note_number":  note.CreditNoteNumber,
		"original_invoice_id": invoiceID,
		"journal_entry_id":    entry.ID,
		"amount":              note.Amount.String(),
		"currency":            currency,
		"actor":               actorID,
	}); err != nil {
		return entry, err
	}
	return entry, nil
}

// PostDebitNote posts a finance.debit_note KRecord to the ledger. The
// generated journal reverses the AP posting of the referenced bill:
//
//	Dr Accounts Payable  amount
//	Cr Expense           amount
//
// Mirrors PostCreditNote; see that method for the full contract.
func (p *InvoicePoster) PostDebitNote(ctx context.Context, tenantID, debitNoteID, actorID uuid.UUID) (*JournalEntry, error) {
	if p.recordStore == nil || p.store == nil {
		return nil, errors.New("ledger: poster not fully wired")
	}
	if tenantID == uuid.Nil || debitNoteID == uuid.Nil {
		return nil, errors.New("ledger: tenant id and debit note id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("ledger: actor id required")
	}

	rec, err := p.recordStore.Get(ctx, tenantID, debitNoteID)
	if err != nil {
		return nil, err
	}
	if rec.KType != "finance.debit_note" {
		return nil, fmt.Errorf("%w: expected finance.debit_note, got %s", ErrSourceMismatch, rec.KType)
	}

	var note debitNoteData
	if err := json.Unmarshal(rec.Data, &note); err != nil {
		return nil, fmt.Errorf("ledger: decode debit note: %w", err)
	}
	if note.Status == "posted" || note.JournalEntryID != "" {
		return nil, ErrDebitNoteAlreadyPosted
	}
	if note.Status != "" && note.Status != "draft" {
		return nil, fmt.Errorf("%w: status=%s", ErrDebitNoteNotPostable, note.Status)
	}
	if note.OriginalBillID == "" {
		return nil, errors.New("ledger: original_bill_id required")
	}
	if !note.Amount.IsPositive() {
		return nil, errors.New("ledger: debit note amount must be positive")
	}
	billID, err := uuid.Parse(note.OriginalBillID)
	if err != nil {
		return nil, fmt.Errorf("ledger: invalid original_bill_id: %w", err)
	}

	billRec, err := p.recordStore.Get(ctx, tenantID, billID)
	if err != nil {
		return nil, fmt.Errorf("ledger: load original bill: %w", err)
	}
	if billRec.KType != "finance.ap_bill" {
		return nil, fmt.Errorf("%w: original must be finance.ap_bill, got %s", ErrSourceMismatch, billRec.KType)
	}
	var bill billData
	if err := json.Unmarshal(billRec.Data, &bill); err != nil {
		return nil, fmt.Errorf("ledger: decode original bill: %w", err)
	}
	if bill.Status != "posted" && bill.Status != "paid" {
		return nil, fmt.Errorf("%w: bill status=%s", ErrOriginalNotPosted, bill.Status)
	}
	if bill.APAccountCode == "" || bill.ExpenseAccountCode == "" {
		return nil, errors.New("ledger: original bill missing account codes")
	}
	currency := note.Currency
	if currency == "" {
		currency = bill.Currency
	}
	if currency == "" {
		currency = "USD"
	}

	existing, err := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.debit_note", debitNoteID)
	if err != nil && !errors.Is(err, ErrEntryNotFound) {
		return nil, err
	}

	var entry *JournalEntry
	if existing != nil {
		entry = existing
	} else {
		lines := []JournalLine{
			{AccountCode: bill.APAccountCode, Debit: note.Amount, Credit: decimal.Zero, Currency: currency, Memo: memoFor(note.DebitNoteNumber, "AP reversal")},
			{AccountCode: bill.ExpenseAccountCode, Debit: decimal.Zero, Credit: note.Amount, Currency: currency, Memo: memoFor(note.DebitNoteNumber, "Expense reversal")},
		}
		postedAt := parseInvoiceDate(note.IssueDate, p.store.now)
		sourceID := debitNoteID
		posted, postErr := p.store.PostJournalEntry(ctx, JournalEntry{
			TenantID:    tenantID,
			PostedAt:    postedAt,
			Memo:        fmt.Sprintf("Debit note %s", note.DebitNoteNumber),
			SourceKType: "finance.debit_note",
			SourceID:    &sourceID,
			CreatedBy:   actorID,
			Lines:       lines,
		})
		if postErr != nil {
			if errors.Is(postErr, ErrDuplicateSourceEntry) {
				reloaded, reloadErr := p.store.GetJournalEntryBySource(ctx, tenantID, "finance.debit_note", debitNoteID)
				if reloadErr != nil {
					return nil, fmt.Errorf("ledger: reload duplicate debit-note entry: %w", reloadErr)
				}
				posted = reloaded
			} else {
				return nil, postErr
			}
		}
		entry = posted
	}

	patch := map[string]any{
		"status":           "posted",
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
		return entry, fmt.Errorf("ledger: patch debit note: %w", err)
	}

	if err := p.emitSourceEvent(ctx, tenantID, "finance.debit_note.posted", map[string]any{
		"debit_note_id":    debitNoteID,
		"debit_note_number": note.DebitNoteNumber,
		"original_bill_id": billID,
		"journal_entry_id": entry.ID,
		"amount":           note.Amount.String(),
		"currency":         currency,
		"actor":            actorID,
	}); err != nil {
		return entry, err
	}
	return entry, nil
}
