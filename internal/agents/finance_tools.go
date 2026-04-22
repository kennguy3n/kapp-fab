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
func RegisterFinanceTools(x *Executor, ledgerStore *ledger.PGStore, poster *ledger.InvoicePoster) {
	x.Register(&createSalesInvoiceTool{executor: x})
	x.Register(&createAPBillTool{executor: x})
	x.Register(&postJournalTool{executor: x, ledger: ledgerStore})
	x.Register(&postSalesInvoiceTool{executor: x, poster: poster})
	x.Register(&postAPBillTool{executor: x, poster: poster})
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

// assignIfSet populates `data[key]` only when `value` is a non-empty
// string. Used to keep optional fields out of the KRecord payload so
// the schema validator does not see empty strings where it expects
// real values.
func assignIfSet(data map[string]any, key, value string) {
	if value != "" {
		data[key] = value
	}
}
