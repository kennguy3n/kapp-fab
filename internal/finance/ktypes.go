// Package finance holds the canonical Phase C finance KType definitions
// — chart-of-accounts entries, journal-entry headers, AR invoices, and AP
// bills — and a setup hook that registers them against a KType registry.
// The schemas follow the same embedded-JSON pattern as internal/crm so the
// web UI, KChat bridge, and agent tools drive off a single source of
// truth (ARCHITECTURE.md §6).
//
// The invoice and bill KTypes are intentionally stored as KRecords (JSONB
// rows in `krecords`) because they benefit from the KType-driven UI, cards,
// and agent-tool story. The actual double-entry ledger uses the dedicated
// `accounts` / `journal_entries` / `journal_lines` typed tables that live
// behind internal/ledger — double-entry integrity is enforced at the
// relational layer, not in JSON schema.
package finance

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers. Kept as constants so the router, agent tools, and
// tests all reference the same strings.
const (
	KTypeAccount          = "finance.account"
	KTypeJournalEntry     = "finance.journal_entry"
	KTypeARInvoice        = "finance.ar_invoice"
	KTypeAPBill           = "finance.ap_bill"
	KTypeCreditNote       = "finance.credit_note"
	KTypeDebitNote        = "finance.debit_note"
	KTypeRecurringInvoice = "finance.recurring_invoice"
	// KTypePayment moved to payment.go — re-exported here via the
	// payment.go file so the registry keeps finance.payment colocated
	// with its schema.
)

// Canonical workflow names for finance KTypes. The workflow engine
// registers these per-tenant; the schemas below embed the same names so
// the definitions stay colocated with the KType they drive.
const (
	WorkflowJournalEntry = "finance.journal_entry.lifecycle"
	WorkflowARInvoice    = "finance.ar_invoice.lifecycle"
	WorkflowAPBill       = "finance.ap_bill.lifecycle"
	WorkflowCreditNote   = "finance.credit_note.lifecycle"
	WorkflowDebitNote    = "finance.debit_note.lifecycle"
)

// accountSchema — chart-of-accounts entry. `code` is the per-tenant
// unique identifier (e.g. "1100", "4000"); the typed `accounts` table
// enforces the uniqueness constraint. `type` mirrors the CHECK constraint
// in migrations/000001_initial_schema.sql lines 190-198.
var accountSchema = []byte(`{
  "name": "finance.account",
  "version": 1,
  "fields": [
    {"name": "code", "type": "string", "required": true, "max_length": 32},
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "type", "type": "enum", "values": ["asset", "liability", "equity", "revenue", "expense"], "required": true},
    {"name": "parent_code", "type": "string", "max_length": 32},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["code", "name", "type", "parent_code", "active"]},
    "form": {"sections": [{"title": "Account", "fields": ["code", "name", "type", "parent_code", "active"]}]}
  },
  "cards": {"summary": "{{code}} — {{name}} ({{type}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]}
}`)

// journalEntrySchema — journal-entry header. The authoritative record of
// the posted entry lives in `journal_entries` / `journal_lines`; this
// KRecord is the KType-driven facade that agent tools and the UI render.
// The workflow mirrors the finite-state life cycle of a journal entry:
// draft (editable) → posted (immutable) → reversed (only via a
// contra-entry, never by mutation).
var journalEntrySchema = []byte(`{
  "name": "finance.journal_entry",
  "version": 1,
  "fields": [
    {"name": "posted_at", "type": "datetime", "required": true},
    {"name": "memo", "type": "text"},
    {"name": "source_ktype", "type": "string"},
    {"name": "source_id", "type": "string"},
    {"name": "status", "type": "enum", "values": ["draft", "posted", "reversed"], "default": "draft"},
    {"name": "lines", "type": "array"},
    {"name": "total_debit", "type": "number", "min": 0},
    {"name": "total_credit", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "journal_entry_id", "type": "string"}
  ],
  "views": {
    "list": {"columns": ["posted_at", "memo", "total_debit", "currency", "status"]},
    "form": {"sections": [
      {"title": "Header", "fields": ["posted_at", "memo", "currency", "status"]},
      {"title": "Source", "fields": ["source_ktype", "source_id"]},
      {"title": "Lines", "fields": ["lines", "total_debit", "total_credit"]}
    ]}
  },
  "cards": {"summary": "JE {{memo}} — {{total_debit}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "workflow": {
    "name": "finance.journal_entry.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "posted", "reversed"],
    "transitions": [
      {"from": ["draft"], "to": "posted", "action": "post"},
      {"from": ["posted"], "to": "reversed", "action": "reverse"}
    ]
  },
  "agent_tools": ["finance.post_journal"]
}`)

// arInvoiceSchema — customer invoice. `customer_id` refs crm.organization
// so existing CRM records drive the subledger. Posting generates the
// balanced journal entry via the ledger engine and sets `journal_entry_id`.
var arInvoiceSchema = []byte(`{
  "name": "finance.ar_invoice",
  "version": 1,
  "fields": [
    {"name": "customer_id", "type": "ref", "ktype": "crm.customer", "required": true},
    {"name": "deal_id", "type": "ref", "ktype": "crm.deal"},
    {"name": "invoice_number", "type": "string", "max_length": 64},
    {"name": "issue_date", "type": "date"},
    {"name": "due_date", "type": "date"},
    {"name": "lines", "type": "array"},
    {"name": "subtotal", "type": "number", "min": 0},
    {"name": "tax_code", "type": "string", "max_length": 32},
    {"name": "tax_amount", "type": "number", "min": 0},
    {"name": "total", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "pending_approval", "posted", "paid", "cancelled"], "default": "draft"},
    {"name": "journal_entry_id", "type": "string"},
    {"name": "ar_account_code", "type": "string", "max_length": 32},
    {"name": "revenue_account_code", "type": "string", "max_length": 32},
    {"name": "tax_account_code", "type": "string", "max_length": 32},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["invoice_number", "customer_id", "total", "currency", "due_date", "status"]},
    "form": {"sections": [
      {"title": "Invoice", "fields": ["invoice_number", "customer_id", "deal_id", "issue_date", "due_date", "currency", "owner"]},
      {"title": "Lines", "fields": ["lines", "subtotal", "tax_code", "tax_amount", "total"]},
      {"title": "Accounts", "fields": ["ar_account_code", "revenue_account_code", "tax_account_code"]},
      {"title": "Posting", "fields": ["status", "journal_entry_id"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "invoice_number", "card_subtitle": "total"}
  },
  "cards": {"summary": "Invoice {{invoice_number}} — {{total}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "workflow": {
    "name": "finance.ar_invoice.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "pending_approval", "posted", "paid", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "pending_approval", "action": "submit_for_approval"},
      {"from": ["draft", "pending_approval"], "to": "posted", "action": "post"},
      {"from": ["posted"], "to": "paid", "action": "mark_paid"},
      {"from": ["draft", "pending_approval"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["finance.create_sales_invoice"]
}`)

// apBillSchema — supplier bill. Same posting life cycle as AR, but with
// Debit Expense / Credit AP instead of Debit AR / Credit Revenue.
var apBillSchema = []byte(`{
  "name": "finance.ap_bill",
  "version": 1,
  "fields": [
    {"name": "supplier_id", "type": "ref", "ktype": "crm.supplier", "required": true},
    {"name": "bill_number", "type": "string", "max_length": 64},
    {"name": "issue_date", "type": "date"},
    {"name": "due_date", "type": "date"},
    {"name": "lines", "type": "array"},
    {"name": "subtotal", "type": "number", "min": 0},
    {"name": "tax_code", "type": "string", "max_length": 32},
    {"name": "tax_amount", "type": "number", "min": 0},
    {"name": "total", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "pending_approval", "posted", "paid", "cancelled"], "default": "draft"},
    {"name": "journal_entry_id", "type": "string"},
    {"name": "ap_account_code", "type": "string", "max_length": 32},
    {"name": "expense_account_code", "type": "string", "max_length": 32},
    {"name": "tax_account_code", "type": "string", "max_length": 32},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["bill_number", "supplier_id", "total", "currency", "due_date", "status"]},
    "form": {"sections": [
      {"title": "Bill", "fields": ["bill_number", "supplier_id", "issue_date", "due_date", "currency", "owner"]},
      {"title": "Lines", "fields": ["lines", "subtotal", "tax_code", "tax_amount", "total"]},
      {"title": "Accounts", "fields": ["ap_account_code", "expense_account_code", "tax_account_code"]},
      {"title": "Posting", "fields": ["status", "journal_entry_id"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "bill_number", "card_subtitle": "total"}
  },
  "cards": {"summary": "Bill {{bill_number}} — {{total}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "workflow": {
    "name": "finance.ap_bill.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "pending_approval", "posted", "paid", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "pending_approval", "action": "submit_for_approval"},
      {"from": ["draft", "pending_approval"], "to": "posted", "action": "post"},
      {"from": ["posted"], "to": "paid", "action": "mark_paid"},
      {"from": ["draft", "pending_approval"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["finance.create_ap_bill"]
}`)

// creditNoteSchema — AR credit note. Reverses a previously-posted
// sales invoice (Dr Revenue, Cr AR) for the supplied amount. The
// `original_invoice_id` + `reason` fields drive the audit trail; the
// posting leg account codes are inherited from the referenced invoice
// at post time so a user cannot accidentally direct a credit to the
// wrong ledger account.
var creditNoteSchema = []byte(`{
  "name": "finance.credit_note",
  "version": 1,
  "fields": [
    {"name": "original_invoice_id", "type": "ref", "ktype": "finance.ar_invoice", "required": true},
    {"name": "credit_note_number", "type": "string", "max_length": 64},
    {"name": "issue_date", "type": "date"},
    {"name": "reason", "type": "text"},
    {"name": "amount", "type": "number", "min": 0, "required": true},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "posted", "cancelled"], "default": "draft"},
    {"name": "journal_entry_id", "type": "string"}
  ],
  "views": {
    "list": {"columns": ["credit_note_number", "original_invoice_id", "amount", "currency", "issue_date", "status"]},
    "form": {"sections": [
      {"title": "Credit Note", "fields": ["credit_note_number", "original_invoice_id", "issue_date", "currency"]},
      {"title": "Details", "fields": ["amount", "reason"]},
      {"title": "Posting", "fields": ["status", "journal_entry_id"]}
    ]}
  },
  "cards": {"summary": "Credit Note {{credit_note_number}} — {{amount}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "workflow": {
    "name": "finance.credit_note.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "posted", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "posted", "action": "post"},
      {"from": ["draft"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["finance.post_credit_note"]
}`)

// debitNoteSchema — AP debit note. Reverses a previously-posted
// supplier bill (Dr AP, Cr Expense) for the supplied amount.
var debitNoteSchema = []byte(`{
  "name": "finance.debit_note",
  "version": 1,
  "fields": [
    {"name": "original_bill_id", "type": "ref", "ktype": "finance.ap_bill", "required": true},
    {"name": "debit_note_number", "type": "string", "max_length": 64},
    {"name": "issue_date", "type": "date"},
    {"name": "reason", "type": "text"},
    {"name": "amount", "type": "number", "min": 0, "required": true},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "posted", "cancelled"], "default": "draft"},
    {"name": "journal_entry_id", "type": "string"}
  ],
  "views": {
    "list": {"columns": ["debit_note_number", "original_bill_id", "amount", "currency", "issue_date", "status"]},
    "form": {"sections": [
      {"title": "Debit Note", "fields": ["debit_note_number", "original_bill_id", "issue_date", "currency"]},
      {"title": "Details", "fields": ["amount", "reason"]},
      {"title": "Posting", "fields": ["status", "journal_entry_id"]}
    ]}
  },
  "cards": {"summary": "Debit Note {{debit_note_number}} — {{amount}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "workflow": {
    "name": "finance.debit_note.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "posted", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "posted", "action": "post"},
      {"from": ["draft"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["finance.post_debit_note"]
}`)

// recurringInvoiceSchema — Phase J recurring AR invoice template. The
// row stores the cadence (frequency + start/end), the next time the
// generator should fire, the template invoice to clone, and the
// auto-post toggle. Generation is driven by the scheduler; each fire
// clones template_invoice_id into a fresh draft (or posted, if
// auto_post=true) and advances next_generation_date.
var recurringInvoiceSchema = []byte(`{
  "name": "finance.recurring_invoice",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "template_invoice_id", "type": "ref", "ktype": "finance.ar_invoice", "required": true},
    {"name": "frequency", "type": "enum", "values": ["daily", "weekly", "monthly", "quarterly", "yearly"], "required": true},
    {"name": "start_date", "type": "date", "required": true},
    {"name": "end_date", "type": "date"},
    {"name": "next_generation_date", "type": "date", "required": true},
    {"name": "auto_post", "type": "boolean", "default": false},
    {"name": "last_generated_at", "type": "datetime"},
    {"name": "last_generated_invoice_id", "type": "string"},
    {"name": "status", "type": "enum", "values": ["active", "paused", "completed"], "default": "active"}
  ],
  "views": {
    "list": {"columns": ["name", "frequency", "next_generation_date", "auto_post", "status"]},
    "form": {"sections": [
      {"title": "Recurring Invoice", "fields": ["name", "template_invoice_id", "frequency", "auto_post", "status"]},
      {"title": "Schedule", "fields": ["start_date", "end_date", "next_generation_date"]},
      {"title": "Last Run", "fields": ["last_generated_at", "last_generated_invoice_id"]}
    ]}
  },
  "cards": {"summary": "{{name}} — {{frequency}} (next {{next_generation_date}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "agent_tools": ["finance.create_recurring_invoice"]
}`)

// All returns every Phase C finance KType as a freshly-constructed slice.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeAccount, Version: 1, Schema: accountSchema},
		{Name: KTypeJournalEntry, Version: 1, Schema: journalEntrySchema},
		{Name: KTypeARInvoice, Version: 1, Schema: arInvoiceSchema},
		{Name: KTypeAPBill, Version: 1, Schema: apBillSchema},
		{Name: KTypeCreditNote, Version: 1, Schema: creditNoteSchema},
		{Name: KTypeDebitNote, Version: 1, Schema: debitNoteSchema},
		{Name: KTypePayment, Version: 1, Schema: paymentSchema},
		{Name: KTypeRecurringInvoice, Version: 1, Schema: recurringInvoiceSchema},
	}
}

// init validates every embedded schema is well-formed JSON so a malformed
// literal trips tests immediately rather than at tenant-setup time.
func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("finance: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers every Phase C finance KType against the
// supplied registry. Idempotent: the underlying PGRegistry upserts on
// conflict.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("finance: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
