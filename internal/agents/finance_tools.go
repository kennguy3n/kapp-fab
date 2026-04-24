package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// RegisterFinanceTools attaches every Phase C finance tool to an
// executor. Mirrors RegisterCRMTools — callers wire this during
// service startup; tests can wire it around an in-memory executor.
//
// The ledger store and invoice poster are optional: tools degrade to
// record-only creation when the ledger layer is nil so Phase B tests
// that don't spin up the ledger still pass. Production callers must
// wire both.
func RegisterFinanceTools(x *Executor, ledgerStore *ledger.PGStore, poster *ledger.InvoicePoster, paymentPoster *ledger.PaymentPoster) {
	x.Register(&createSalesInvoiceTool{executor: x})
	x.Register(&createAPBillTool{executor: x})
	x.Register(&postJournalTool{executor: x, ledger: ledgerStore})
	x.Register(&postSalesInvoiceTool{executor: x, poster: poster})
	x.Register(&postAPBillTool{executor: x, poster: poster})
	x.Register(&postCreditNoteTool{executor: x, poster: poster})
	x.Register(&postDebitNoteTool{executor: x, poster: poster})
	x.Register(&recordPaymentTool{executor: x, poster: paymentPoster})
	x.Register(&createRecurringInvoiceTool{executor: x})
}

// ----- finance.create_recurring_invoice -----
//
// Creates a finance.recurring_invoice KRecord wrapping an existing
// finance.ar_invoice template. The recurring engine (Task 5) sweeps
// active rows and clones the template each cadence; this tool is the
// agent-facing entry point that authors the cursor.
type createRecurringInvoiceInput struct {
	Name               string `json:"name"`
	TemplateInvoiceID  string `json:"template_invoice_id"`
	Frequency          string `json:"frequency"`
	StartDate          string `json:"start_date"`
	EndDate            string `json:"end_date,omitempty"`
	NextGenerationDate string `json:"next_generation_date,omitempty"`
	AutoPost           bool   `json:"auto_post"`
}

type createRecurringInvoiceTool struct{ executor *Executor }

func (t *createRecurringInvoiceTool) Name() string               { return "finance.create_recurring_invoice" }
func (t *createRecurringInvoiceTool) RequiresConfirmation() bool { return true }
func (t *createRecurringInvoiceTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createRecurringInvoiceInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, errors.New("finance.create_recurring_invoice: name required")
	}
	if in.TemplateInvoiceID == "" {
		return nil, errors.New("finance.create_recurring_invoice: template_invoice_id required")
	}
	if in.Frequency == "" {
		return nil, errors.New("finance.create_recurring_invoice: frequency required")
	}
	if in.StartDate == "" {
		return nil, errors.New("finance.create_recurring_invoice: start_date required")
	}
	// next_generation_date defaults to start_date so the very next
	// scheduler tick eligible-for that tenant fires the first run.
	nextGen := in.NextGenerationDate
	if nextGen == "" {
		nextGen = in.StartDate
	}
	data := map[string]any{
		"name":                 in.Name,
		"template_invoice_id":  in.TemplateInvoiceID,
		"frequency":            in.Frequency,
		"start_date":           in.StartDate,
		"next_generation_date": nextGen,
		"auto_post":            in.AutoPost,
		"status":               finance.RecurringStatusActive,
	}
	if in.EndDate != "" {
		data["end_date"] = in.EndDate
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would create recurring invoice %q (%s, next %s)", in.Name, in.Frequency, nextGen),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     finance.KTypeRecurringInvoice,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Created recurring invoice %s (%s, next %s)", rec.ID, in.Frequency, nextGen),
		Record:  rec,
	}, nil
}

// ----- finance.create_sales_invoice -----

type createSalesInvoiceInput struct {
	CustomerID         string          `json:"customer_id"`
	DealID             string          `json:"deal_id,omitempty"`
	InvoiceNumber      string          `json:"invoice_number,omitempty"`
	IssueDate          string          `json:"issue_date,omitempty"`
	DueDate            string          `json:"due_date,omitempty"`
	Lines              json.RawMessage `json:"lines,omitempty"`
	Subtotal           decimal.Decimal `json:"subtotal,omitempty"`
	TaxCode            string          `json:"tax_code,omitempty"`
	TaxAmount          decimal.Decimal `json:"tax_amount,omitempty"`
	Total              decimal.Decimal `json:"total,omitempty"`
	Currency           string          `json:"currency,omitempty"`
	ARAccountCode      string          `json:"ar_account_code,omitempty"`
	RevenueAccountCode string          `json:"revenue_account_code,omitempty"`
	TaxAccountCode     string          `json:"tax_account_code,omitempty"`
	Owner              uuid.UUID       `json:"owner,omitempty"`
}

type createSalesInvoiceTool struct{ executor *Executor }

func (t *createSalesInvoiceTool) Name() string               { return "finance.create_sales_invoice" }
func (t *createSalesInvoiceTool) RequiresConfirmation() bool { return true }
func (t *createSalesInvoiceTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createSalesInvoiceInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.CustomerID == "" {
		return nil, errors.New("finance.create_sales_invoice: customer_id required")
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.Total.IsZero() {
		in.Total = in.Subtotal.Add(in.TaxAmount)
	}
	data := map[string]any{
		"customer_id": in.CustomerID,
		"currency":    in.Currency,
		"status":      "draft",
		"subtotal":    in.Subtotal,
		"tax_amount":  in.TaxAmount,
		"total":       in.Total,
	}
	assignIfSet(data, "deal_id", in.DealID)
	assignIfSet(data, "invoice_number", in.InvoiceNumber)
	assignIfSet(data, "issue_date", in.IssueDate)
	assignIfSet(data, "due_date", in.DueDate)
	assignIfSet(data, "tax_code", in.TaxCode)
	assignIfSet(data, "ar_account_code", in.ARAccountCode)
	assignIfSet(data, "revenue_account_code", in.RevenueAccountCode)
	assignIfSet(data, "tax_account_code", in.TaxAccountCode)
	if len(in.Lines) > 0 {
		data["lines"] = in.Lines
	}
	if in.Owner != uuid.Nil {
		data["owner"] = in.Owner.String()
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would draft invoice for customer %s — %s %s", in.CustomerID, in.Total, in.Currency),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     finance.KTypeARInvoice,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Drafted sales invoice %s (%s %s)", rec.ID, in.Total, in.Currency),
		Record:  rec,
	}, nil
}

// ----- finance.create_ap_bill -----

type createAPBillInput struct {
	SupplierID         string          `json:"supplier_id"`
	BillNumber         string          `json:"bill_number,omitempty"`
	IssueDate          string          `json:"issue_date,omitempty"`
	DueDate            string          `json:"due_date,omitempty"`
	Lines              json.RawMessage `json:"lines,omitempty"`
	Subtotal           decimal.Decimal `json:"subtotal,omitempty"`
	TaxCode            string          `json:"tax_code,omitempty"`
	TaxAmount          decimal.Decimal `json:"tax_amount,omitempty"`
	Total              decimal.Decimal `json:"total,omitempty"`
	Currency           string          `json:"currency,omitempty"`
	APAccountCode      string          `json:"ap_account_code,omitempty"`
	ExpenseAccountCode string          `json:"expense_account_code,omitempty"`
	TaxAccountCode     string          `json:"tax_account_code,omitempty"`
	Owner              uuid.UUID       `json:"owner,omitempty"`
}

type createAPBillTool struct{ executor *Executor }

func (t *createAPBillTool) Name() string               { return "finance.create_ap_bill" }
func (t *createAPBillTool) RequiresConfirmation() bool { return true }
func (t *createAPBillTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createAPBillInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.SupplierID == "" {
		return nil, errors.New("finance.create_ap_bill: supplier_id required")
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.Total.IsZero() {
		in.Total = in.Subtotal.Add(in.TaxAmount)
	}
	data := map[string]any{
		"supplier_id": in.SupplierID,
		"currency":    in.Currency,
		"status":      "draft",
		"subtotal":    in.Subtotal,
		"tax_amount":  in.TaxAmount,
		"total":       in.Total,
	}
	assignIfSet(data, "bill_number", in.BillNumber)
	assignIfSet(data, "issue_date", in.IssueDate)
	assignIfSet(data, "due_date", in.DueDate)
	assignIfSet(data, "tax_code", in.TaxCode)
	assignIfSet(data, "ap_account_code", in.APAccountCode)
	assignIfSet(data, "expense_account_code", in.ExpenseAccountCode)
	assignIfSet(data, "tax_account_code", in.TaxAccountCode)
	if len(in.Lines) > 0 {
		data["lines"] = in.Lines
	}
	if in.Owner != uuid.Nil {
		data["owner"] = in.Owner.String()
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would draft bill from supplier %s — %s %s", in.SupplierID, in.Total, in.Currency),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     finance.KTypeAPBill,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Drafted AP bill %s (%s %s)", rec.ID, in.Total, in.Currency),
		Record:  rec,
	}, nil
}

// ----- finance.post_journal -----

type postJournalLine struct {
	AccountCode string          `json:"account_code"`
	Debit       decimal.Decimal `json:"debit,omitempty"`
	Credit      decimal.Decimal `json:"credit,omitempty"`
	Currency    string          `json:"currency"`
	Memo        string          `json:"memo,omitempty"`
}

type postJournalInput struct {
	Memo        string            `json:"memo,omitempty"`
	SourceKType string            `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID        `json:"source_id,omitempty"`
	Lines       []postJournalLine `json:"lines"`
}

type postJournalTool struct {
	executor *Executor
	ledger   *ledger.PGStore
}

func (t *postJournalTool) Name() string               { return "finance.post_journal" }
func (t *postJournalTool) RequiresConfirmation() bool { return true }
func (t *postJournalTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postJournalInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if len(in.Lines) == 0 {
		return nil, errors.New("finance.post_journal: lines required")
	}
	lines := make([]ledger.JournalLine, 0, len(in.Lines))
	for _, l := range in.Lines {
		lines = append(lines, ledger.JournalLine{
			AccountCode: l.AccountCode,
			Debit:       l.Debit,
			Credit:      l.Credit,
			Currency:    l.Currency,
			Memo:        l.Memo,
		})
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would post %d-line journal entry", len(lines)),
			Preview: preview,
		}, nil
	}
	if t.ledger == nil {
		return nil, errors.New("finance.post_journal: ledger not configured")
	}
	entry, err := t.ledger.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID:    inv.TenantID,
		Memo:        in.Memo,
		SourceKType: in.SourceKType,
		SourceID:    in.SourceID,
		CreatedBy:   inv.ActorID,
		Lines:       lines,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(entry)
	return &Result{
		Summary: fmt.Sprintf("Posted journal entry %s", entry.ID),
		Preview: body,
		Extra:   map[string]any{"journal_entry_id": entry.ID},
	}, nil
}

// ----- finance.post_sales_invoice -----

type postSalesInvoiceInput struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
}

type postSalesInvoiceTool struct {
	executor *Executor
	poster   *ledger.InvoicePoster
}

func (t *postSalesInvoiceTool) Name() string               { return "finance.post_sales_invoice" }
func (t *postSalesInvoiceTool) RequiresConfirmation() bool { return true }
func (t *postSalesInvoiceTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postSalesInvoiceInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.InvoiceID == uuid.Nil {
		return nil, errors.New("finance.post_sales_invoice: invoice_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would post sales invoice %s", in.InvoiceID),
			Preview: preview,
		}, nil
	}
	if t.poster == nil {
		return nil, errors.New("finance.post_sales_invoice: poster not configured")
	}
	entry, err := t.poster.PostSalesInvoice(ctx, inv.TenantID, in.InvoiceID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(entry)
	return &Result{
		Summary: fmt.Sprintf("Posted sales invoice %s → JE %s", in.InvoiceID, entry.ID),
		Preview: body,
		Extra:   map[string]any{"journal_entry_id": entry.ID, "invoice_id": in.InvoiceID},
	}, nil
}

// ----- finance.post_ap_bill -----

type postAPBillInput struct {
	BillID uuid.UUID `json:"bill_id"`
}

type postAPBillTool struct {
	executor *Executor
	poster   *ledger.InvoicePoster
}

func (t *postAPBillTool) Name() string               { return "finance.post_ap_bill" }
func (t *postAPBillTool) RequiresConfirmation() bool { return true }
func (t *postAPBillTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postAPBillInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.BillID == uuid.Nil {
		return nil, errors.New("finance.post_ap_bill: bill_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would post AP bill %s", in.BillID),
			Preview: preview,
		}, nil
	}
	if t.poster == nil {
		return nil, errors.New("finance.post_ap_bill: poster not configured")
	}
	entry, err := t.poster.PostPurchaseBill(ctx, inv.TenantID, in.BillID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(entry)
	return &Result{
		Summary: fmt.Sprintf("Posted AP bill %s → JE %s", in.BillID, entry.ID),
		Preview: body,
		Extra:   map[string]any{"journal_entry_id": entry.ID, "bill_id": in.BillID},
	}, nil
}

// ----- finance.post_credit_note -----

type postCreditNoteInput struct {
	CreditNoteID uuid.UUID `json:"credit_note_id"`
}

type postCreditNoteTool struct {
	executor *Executor
	poster   *ledger.InvoicePoster
}

func (t *postCreditNoteTool) Name() string               { return "finance.post_credit_note" }
func (t *postCreditNoteTool) RequiresConfirmation() bool { return true }
func (t *postCreditNoteTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postCreditNoteInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.CreditNoteID == uuid.Nil {
		return nil, errors.New("finance.post_credit_note: credit_note_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would post credit note %s", in.CreditNoteID),
			Preview: preview,
		}, nil
	}
	if t.poster == nil {
		return nil, errors.New("finance.post_credit_note: poster not configured")
	}
	entry, err := t.poster.PostCreditNote(ctx, inv.TenantID, in.CreditNoteID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(entry)
	return &Result{
		Summary: fmt.Sprintf("Posted credit note %s → JE %s", in.CreditNoteID, entry.ID),
		Preview: body,
		Extra:   map[string]any{"journal_entry_id": entry.ID, "credit_note_id": in.CreditNoteID},
	}, nil
}

// ----- finance.post_debit_note -----

type postDebitNoteInput struct {
	DebitNoteID uuid.UUID `json:"debit_note_id"`
}

type postDebitNoteTool struct {
	executor *Executor
	poster   *ledger.InvoicePoster
}

func (t *postDebitNoteTool) Name() string               { return "finance.post_debit_note" }
func (t *postDebitNoteTool) RequiresConfirmation() bool { return true }
func (t *postDebitNoteTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postDebitNoteInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.DebitNoteID == uuid.Nil {
		return nil, errors.New("finance.post_debit_note: debit_note_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would post debit note %s", in.DebitNoteID),
			Preview: preview,
		}, nil
	}
	if t.poster == nil {
		return nil, errors.New("finance.post_debit_note: poster not configured")
	}
	entry, err := t.poster.PostDebitNote(ctx, inv.TenantID, in.DebitNoteID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(entry)
	return &Result{
		Summary: fmt.Sprintf("Posted debit note %s → JE %s", in.DebitNoteID, entry.ID),
		Preview: body,
		Extra:   map[string]any{"journal_entry_id": entry.ID, "debit_note_id": in.DebitNoteID},
	}, nil
}

// ----- finance.record_payment -----

type recordPaymentAllocation struct {
	InvoiceID       string          `json:"invoice_id"`
	AllocatedAmount decimal.Decimal `json:"allocated_amount"`
}

type recordPaymentInput struct {
	PaymentType   string                    `json:"payment_type"`
	PartyType     string                    `json:"party_type"`
	PartyID       string                    `json:"party_id"`
	Amount        decimal.Decimal           `json:"amount"`
	Currency      string                    `json:"currency,omitempty"`
	PaymentDate   string                    `json:"payment_date,omitempty"`
	Reference     string                    `json:"reference,omitempty"`
	BankAccount   string                    `json:"bank_account"`
	ARAccountCode string                    `json:"ar_account_code,omitempty"`
	APAccountCode string                    `json:"ap_account_code,omitempty"`
	Allocations   []recordPaymentAllocation `json:"allocations,omitempty"`
	Owner         uuid.UUID                 `json:"owner,omitempty"`
	AutoPost      bool                      `json:"auto_post,omitempty"`
}

type recordPaymentTool struct {
	executor *Executor
	poster   *ledger.PaymentPoster
}

func (t *recordPaymentTool) Name() string               { return "finance.record_payment" }
func (t *recordPaymentTool) RequiresConfirmation() bool { return true }
func (t *recordPaymentTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in recordPaymentInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.PaymentType != "receive" && in.PaymentType != "pay" {
		return nil, errors.New("finance.record_payment: payment_type must be receive or pay")
	}
	if in.PartyType != "customer" && in.PartyType != "supplier" {
		return nil, errors.New("finance.record_payment: party_type must be customer or supplier")
	}
	if in.PartyID == "" {
		return nil, errors.New("finance.record_payment: party_id required")
	}
	if !in.Amount.IsPositive() {
		return nil, errors.New("finance.record_payment: amount must be positive")
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	data := map[string]any{
		"payment_type": in.PaymentType,
		"party_type":   in.PartyType,
		"party_id":     in.PartyID,
		"amount":       in.Amount,
		"currency":     in.Currency,
		"status":       "draft",
	}
	assignIfSet(data, "payment_date", in.PaymentDate)
	assignIfSet(data, "reference", in.Reference)
	assignIfSet(data, "bank_account", in.BankAccount)
	assignIfSet(data, "ar_account_code", in.ARAccountCode)
	assignIfSet(data, "ap_account_code", in.APAccountCode)
	if in.Owner != uuid.Nil {
		data["owner"] = in.Owner.String()
	}
	if len(in.Allocations) > 0 {
		data["allocations"] = in.Allocations
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would record %s payment — %s %s", in.PaymentType, in.Amount, in.Currency),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     finance.KTypePayment,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	extra := map[string]any{"payment_id": rec.ID}
	if in.AutoPost && t.poster != nil {
		entry, postErr := t.poster.PostPayment(ctx, inv.TenantID, rec.ID, inv.ActorID)
		if postErr != nil {
			return nil, fmt.Errorf("record payment draft %s posted but poster failed: %w", rec.ID, postErr)
		}
		extra["journal_entry_id"] = entry.ID
		return &Result{
			Summary: fmt.Sprintf("Recorded payment %s → JE %s", rec.ID, entry.ID),
			Record:  rec,
			Extra:   extra,
		}, nil
	}
	return &Result{
		Summary: fmt.Sprintf("Drafted %s payment %s (%s %s)", in.PaymentType, rec.ID, in.Amount, in.Currency),
		Record:  rec,
		Extra:   extra,
	}, nil
}

// assignIfSet populates `data[key]` only when `value` is a non-empty
// string. Used to keep optional fields out of the KRecord payload so
// the schema validator does not see empty strings where it expects
// real values.
func assignIfSet(data map[string]any, key, value string) {
	if value != "" {
		data[key] = value
	}
}
