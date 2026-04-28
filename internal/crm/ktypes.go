// Package crm holds the canonical Phase B business KType definitions —
// the CRM (lead, contact, organization, deal, activity, quote) and Tasks
// KTypes — and a setup hook that registers them against a KType
// registry. The schemas are stored as Go-embedded JSON so they can be
// round-tripped through ktype.PGRegistry.Register unchanged. Each schema
// is intentionally verbose (fields + views + cards + workflow + permissions)
// so the web UI, KChat bridge, and agent tools can drive off a single
// source of truth (ARCHITECTURE.md §6).
package crm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// Name returns the canonical KType identifier for each Phase B object. Kept
// as constants so callers (router, agent tools, tests) don't duplicate
// string literals.
const (
	KTypeLead         = "crm.lead"
	KTypeContact      = "crm.contact"
	KTypeOrganization = "crm.organization"
	KTypeDeal         = "crm.deal"
	KTypeActivity     = "crm.activity"
	KTypeQuote        = "crm.quote"
	KTypeTask         = "tasks.task"
	KTypeCustomer     = "crm.customer"
	KTypeSupplier     = "crm.supplier"
)

// WorkflowDealPipeline is the canonical workflow name for crm.deal. Mirrored
// as a constant so the workflow engine registration, agent tools, and
// schema payloads all reference the same string.
const WorkflowDealPipeline = "crm.deal.pipeline"

// leadSchema — sales pipeline top-of-funnel. `owner` is a ref to user so
// record-owner RLS can be extended later; `contact_info` is an object so
// phone + email can be captured without prescribing a formal crm.contact
// record until qualification.
var leadSchema = []byte(`{
  "name": "crm.lead",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "source", "type": "enum", "values": ["web", "referral", "cold", "event"]},
    {"name": "owner", "type": "ref", "ktype": "user"},
    {"name": "status", "type": "enum", "values": ["new", "contacted", "qualified", "unqualified"], "default": "new"},
    {"name": "score", "type": "integer", "min": 0, "max": 100},
    {"name": "contact_info", "type": "object"}
  ],
  "views": {
    "list": {"columns": ["name", "source", "status", "score", "owner"]},
    "form": {"sections": [{"title": "Lead", "fields": ["name", "source", "status", "score", "owner", "contact_info"]}]},
    "kanban": {"group_by": "status", "card_title": "name", "card_subtitle": "source"}
  },
  "cards": {"summary": "{{name}} ({{source}}) — {{status}}"},
  "permissions": {"read": ["tenant.member"], "write": ["crm.rep", "tenant.admin"]}
}`)

var contactSchema = []byte(`{
  "name": "crm.contact",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "email", "type": "string", "pattern": "^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$"},
    {"name": "phone", "type": "string"},
    {"name": "organization", "type": "ref", "ktype": "crm.organization"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["name", "email", "phone", "organization", "owner"]},
    "form": {"sections": [{"title": "Contact", "fields": ["name", "email", "phone", "organization", "owner"]}]}
  },
  "cards": {"summary": "{{name}} — {{email}}"},
  "permissions": {"read": ["tenant.member"], "write": ["crm.rep", "tenant.admin"]}
}`)

var organizationSchema = []byte(`{
  "name": "crm.organization",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "domain", "type": "string"},
    {"name": "industry", "type": "string"},
    {"name": "size", "type": "enum", "values": ["small", "medium", "large", "enterprise"]},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["name", "domain", "industry", "size", "owner"]},
    "form": {"sections": [{"title": "Organization", "fields": ["name", "domain", "industry", "size", "owner"]}]}
  },
  "cards": {"summary": "{{name}} ({{industry}} · {{size}})"},
  "permissions": {"read": ["tenant.member"], "write": ["crm.rep", "tenant.admin"]}
}`)

// dealSchema — matches ARCHITECTURE.md §6 lines 247-336. The `workflow`
// block declares the pipeline the workflow engine registers under
// WorkflowDealPipeline; the `agent_tools` block pre-authorizes the tools
// that the tool executor can drive against this KType.
var dealSchema = []byte(`{
  "name": "crm.deal",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "stage", "type": "enum", "values": ["qualification", "proposal", "negotiation", "won", "lost"], "default": "qualification"},
    {"name": "amount", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "close_date", "type": "date"},
    {"name": "contact", "type": "ref", "ktype": "crm.contact"},
    {"name": "organization", "type": "ref", "ktype": "crm.organization"},
    {"name": "owner", "type": "ref", "ktype": "user"},
    {"name": "notes", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["name", "stage", "amount", "currency", "close_date", "owner"]},
    "form": {"sections": [
      {"title": "Deal", "fields": ["name", "stage", "amount", "currency", "close_date", "owner"]},
      {"title": "Parties", "fields": ["contact", "organization"]},
      {"title": "Notes", "fields": ["notes"]}
    ]},
    "kanban": {"group_by": "stage", "card_title": "name", "card_subtitle": "amount"}
  },
  "cards": {"summary": "{{name}} — {{amount}} {{currency}} ({{stage}})"},
  "permissions": {"read": ["tenant.member"], "write": ["crm.rep", "tenant.admin"]},
  "workflow": {
    "name": "crm.deal.pipeline",
    "initial_state": "qualification",
    "states": ["qualification", "proposal", "negotiation", "won", "lost"],
    "transitions": [
      {"from": ["qualification"], "to": "proposal", "action": "advance_to_proposal"},
      {"from": ["proposal"], "to": "negotiation", "action": "advance_to_negotiation"},
      {"from": ["negotiation"], "to": "won", "action": "mark_won", "post": ["finance.create_sales_invoice"]},
      {"from": ["qualification", "proposal", "negotiation"], "to": "lost", "action": "mark_lost"}
    ]
  },
  "agent_tools": ["crm.create_deal", "crm.advance_deal", "crm.summarize_pipeline"]
}`)

var activitySchema = []byte(`{
  "name": "crm.activity",
  "version": 1,
  "fields": [
    {"name": "type", "type": "enum", "values": ["call", "email", "meeting", "note"], "default": "note"},
    {"name": "subject", "type": "string", "required": true, "max_length": 200},
    {"name": "date", "type": "datetime"},
    {"name": "contact", "type": "ref", "ktype": "crm.contact"},
    {"name": "deal", "type": "ref", "ktype": "crm.deal"}
  ],
  "views": {
    "list": {"columns": ["date", "type", "subject", "contact", "deal"]},
    "form": {"sections": [{"title": "Activity", "fields": ["type", "subject", "date", "contact", "deal"]}]}
  },
  "cards": {"summary": "{{type}}: {{subject}}"},
  "permissions": {"read": ["tenant.member"], "write": ["crm.rep", "tenant.admin"]}
}`)

var quoteSchema = []byte(`{
  "name": "crm.quote",
  "version": 1,
  "fields": [
    {"name": "deal", "type": "ref", "ktype": "crm.deal", "required": true},
    {"name": "lines", "type": "array"},
    {"name": "total", "type": "number", "min": 0},
    {"name": "discount", "type": "number", "min": 0},
    {"name": "status", "type": "enum", "values": ["draft", "sent", "accepted", "rejected"], "default": "draft"}
  ],
  "views": {
    "list": {"columns": ["deal", "total", "status"]},
    "form": {"sections": [{"title": "Quote", "fields": ["deal", "lines", "total", "discount", "status"]}]}
  },
  "cards": {"summary": "Quote {{total}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["crm.rep", "tenant.admin"]}
}`)

var taskSchema = []byte(`{
  "name": "tasks.task",
  "version": 1,
  "fields": [
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "assignee", "type": "ref", "ktype": "user", "required": true},
    {"name": "due_date", "type": "date"},
    {"name": "status", "type": "enum", "values": ["open", "in_progress", "done", "cancelled"], "default": "open"},
    {"name": "linked_ktype", "type": "string"},
    {"name": "linked_id", "type": "string"},
    {"name": "project_id", "type": "ref", "ktype": "projects.project"},
    {"name": "milestone_id", "type": "ref", "ktype": "projects.milestone"},
    {"name": "description", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["title", "assignee", "due_date", "status"]},
    "form": {"sections": [
      {"title": "Task", "fields": ["title", "assignee", "due_date", "status", "description"]},
      {"title": "Link", "fields": ["linked_ktype", "linked_id", "project_id", "milestone_id"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "title", "card_subtitle": "assignee"}
  },
  "cards": {"summary": "{{title}} — {{status}} ({{assignee}})"},
  "permissions": {"read": ["tenant.member"], "write": ["tenant.member"]},
  "agent_tools": ["tasks.create_task"]
}`)

// customerSchema — billable counterparty used by finance.ar_invoice.
// Mirrors the ERPNext "Customer" doctype: a credit_limit ceiling, a
// payment_terms default, and an aging bucket the collections view can
// filter on. `customer_group` is a string (not an enum) so tenants can
// slice by industry, geography, channel, etc. without schema changes.
var customerSchema = []byte(`{
  "name": "crm.customer",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "customer_group", "type": "string", "max_length": 64},
    {"name": "credit_limit", "type": "number", "min": 0},
    {"name": "default_tax_code", "type": "string", "max_length": 32},
    {"name": "default_payment_terms", "type": "string", "max_length": 64},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "ar_aging_bucket", "type": "enum", "values": ["current", "30", "60", "90", "120+"], "default": "current"},
    {"name": "status", "type": "enum", "values": ["active", "on_hold", "disabled"], "default": "active"},
    {"name": "organization", "type": "ref", "ktype": "crm.organization"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["name", "customer_group", "credit_limit", "currency", "ar_aging_bucket", "status"]},
    "form": {"sections": [
      {"title": "Customer", "fields": ["name", "customer_group", "organization", "owner", "status"]},
      {"title": "Billing", "fields": ["currency", "credit_limit", "default_tax_code", "default_payment_terms", "ar_aging_bucket"]}
    ]}
  },
  "cards": {"summary": "{{name}} — {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "crm.rep", "tenant.admin"]},
  "workflow": {
    "name": "crm.customer.lifecycle",
    "initial_state": "active",
    "states": ["active", "on_hold", "disabled"],
    "transitions": [
      {"from": ["active"], "to": "on_hold", "action": "hold"},
      {"from": ["on_hold"], "to": "active", "action": "release"},
      {"from": ["active", "on_hold"], "to": "disabled", "action": "disable"}
    ]
  },
  "agent_tools": ["crm.create_customer"]
}`)

// supplierSchema — AP counterparty. Same structure as customerSchema but
// with `supplier_group` and `ap_aging_bucket` so finance.ap_bill reports
// can segment the payables side symmetrically.
var supplierSchema = []byte(`{
  "name": "crm.supplier",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "supplier_group", "type": "string", "max_length": 64},
    {"name": "default_payment_terms", "type": "string", "max_length": 64},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "ap_aging_bucket", "type": "enum", "values": ["current", "30", "60", "90", "120+"], "default": "current"},
    {"name": "status", "type": "enum", "values": ["active", "on_hold", "disabled"], "default": "active"},
    {"name": "organization", "type": "ref", "ktype": "crm.organization"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["name", "supplier_group", "currency", "ap_aging_bucket", "status"]},
    "form": {"sections": [
      {"title": "Supplier", "fields": ["name", "supplier_group", "organization", "owner", "status"]},
      {"title": "Payments", "fields": ["currency", "default_payment_terms", "ap_aging_bucket"]}
    ]}
  },
  "cards": {"summary": "{{name}} — {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "workflow": {
    "name": "crm.supplier.lifecycle",
    "initial_state": "active",
    "states": ["active", "on_hold", "disabled"],
    "transitions": [
      {"from": ["active"], "to": "on_hold", "action": "hold"},
      {"from": ["on_hold"], "to": "active", "action": "release"},
      {"from": ["active", "on_hold"], "to": "disabled", "action": "disable"}
    ]
  },
  "agent_tools": ["crm.create_supplier"]
}`)

// All returns every Phase B KType as a freshly-constructed slice. The
// schemas are validated as well-formed JSON at init time so a malformed
// literal trips tests immediately rather than at tenant-setup time.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeLead, Version: 1, Schema: leadSchema},
		{Name: KTypeContact, Version: 1, Schema: contactSchema},
		{Name: KTypeOrganization, Version: 1, Schema: organizationSchema},
		{Name: KTypeDeal, Version: 1, Schema: dealSchema},
		{Name: KTypeActivity, Version: 1, Schema: activitySchema},
		{Name: KTypeQuote, Version: 1, Schema: quoteSchema},
		{Name: KTypeTask, Version: 1, Schema: taskSchema},
		{Name: KTypeCustomer, Version: 1, Schema: customerSchema},
		{Name: KTypeSupplier, Version: 1, Schema: supplierSchema},
	}
}

// init validates every embedded schema is legal JSON. This keeps drift
// between the Go literals and the DB column from sneaking through CI.
func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("crm: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers every Phase B KType against the supplied
// registry. Idempotent: the underlying PGRegistry upserts on conflict.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("crm: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
