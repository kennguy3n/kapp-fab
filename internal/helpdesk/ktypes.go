// Package helpdesk defines the Phase I helpdesk KTypes — ticket and
// sla_policy — and the registration hook wired from services/api.
//
// Reference architecture: frappe/helpdesk ticket + SLA + agent routing.
// The schemas carry the full workflow, views, cards, permissions, and
// agent_tools block so the web UI, KChat bridge, and agent executor
// drive off a single source of truth (ARCHITECTURE.md §6).
package helpdesk

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers re-used across handlers, agent tools, and tests so
// no string literals are duplicated.
const (
	KTypeTicket    = "helpdesk.ticket"
	KTypeSLAPolicy = "helpdesk.sla_policy"
)

// WorkflowTicketLifecycle is the canonical workflow name that the
// workflow engine registers under when helpdesk.ticket is loaded.
const WorkflowTicketLifecycle = "helpdesk.ticket.lifecycle"

// ticketSchema — status + priority + SLA fields mirror ERPNext-style
// helpdesk tickets. The workflow covers open → in_progress → waiting →
// resolved → closed; reopens jump from resolved back to in_progress so
// a misfiled ticket can be picked back up without losing its history.
var ticketSchema = []byte(`{
  "name": "helpdesk.ticket",
  "version": 1,
  "fields": [
    {"name": "subject", "type": "string", "required": true, "max_length": 200},
    {"name": "description", "type": "text"},
    {"name": "status", "type": "enum", "values": ["open", "in_progress", "waiting", "resolved", "closed"], "default": "open"},
    {"name": "priority", "type": "enum", "values": ["low", "medium", "high", "urgent"], "default": "medium"},
    {"name": "channel", "type": "enum", "values": ["email", "chat", "portal", "phone"], "default": "chat"},
    {"name": "customer_id", "type": "ref", "ktype": "crm.customer"},
    {"name": "assigned_to", "type": "ref", "ktype": "user"},
    {"name": "sla_policy_id", "type": "string"},
    {"name": "sla_response_by", "type": "datetime"},
    {"name": "sla_resolution_by", "type": "datetime"},
    {"name": "first_responded_at", "type": "datetime"},
    {"name": "resolved_at", "type": "datetime"},
    {"name": "tags", "type": "array"},
    {"name": "thread_id", "type": "string", "max_length": 120},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["subject", "status", "priority", "channel", "customer_id", "assigned_to", "sla_resolution_by"]},
    "form": {"sections": [
      {"title": "Ticket", "fields": ["subject", "description", "status", "priority", "channel", "tags"]},
      {"title": "Parties", "fields": ["customer_id", "assigned_to", "owner"]},
      {"title": "SLA", "fields": ["sla_policy_id", "sla_response_by", "sla_resolution_by", "first_responded_at", "resolved_at"]},
      {"title": "Linkage", "fields": ["thread_id"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "subject", "card_subtitle": "priority"}
  },
  "cards": {"summary": "{{subject}} — {{status}} ({{priority}})"},
  "permissions": {"read": ["tenant.member"], "write": ["helpdesk.agent", "tenant.admin"]},
  "workflow": {
    "name": "helpdesk.ticket.lifecycle",
    "initial_state": "open",
    "states": ["open", "in_progress", "waiting", "resolved", "closed"],
    "transitions": [
      {"from": ["open"], "to": "in_progress", "action": "start"},
      {"from": ["in_progress"], "to": "waiting", "action": "wait_on_customer"},
      {"from": ["waiting"], "to": "in_progress", "action": "resume"},
      {"from": ["in_progress", "waiting"], "to": "resolved", "action": "resolve"},
      {"from": ["resolved"], "to": "in_progress", "action": "reopen"},
      {"from": ["resolved"], "to": "closed", "action": "close"}
    ]
  },
  "agent_tools": ["helpdesk.create_ticket", "helpdesk.assign_ticket", "helpdesk.resolve_ticket"]
}`)

// slaPolicySchema — per-priority response/resolution targets. The
// policy KRecord mirrors the sla_policies table so the UI can drive
// CRUD through the generic RecordFormPage while the evaluator reads
// from the typed table for lookups.
var slaPolicySchema = []byte(`{
  "name": "helpdesk.sla_policy",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 120},
    {"name": "priority", "type": "enum", "values": ["low", "medium", "high", "urgent"], "required": true},
    {"name": "response_minutes", "type": "integer", "min": 1, "required": true},
    {"name": "resolution_minutes", "type": "integer", "min": 1, "required": true},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["name", "priority", "response_minutes", "resolution_minutes", "active"]},
    "form": {"sections": [{"title": "SLA policy", "fields": ["name", "priority", "response_minutes", "resolution_minutes", "active"]}]}
  },
  "cards": {"summary": "{{name}} — {{priority}} ({{response_minutes}}m / {{resolution_minutes}}m)"},
  "permissions": {"read": ["tenant.member"], "write": ["helpdesk.admin", "tenant.admin"]}
}`)

// All returns every helpdesk KType as a freshly-constructed slice.
// Matches the crm.All shape so the main registration loop in
// services/api/main.go can treat every domain uniformly.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeTicket, Version: 1, Schema: ticketSchema},
		{Name: KTypeSLAPolicy, Version: 1, Schema: slaPolicySchema},
	}
}

// init validates every embedded schema is syntactically valid JSON so
// a malformed literal trips in tests rather than at tenant-setup time.
func init() {
	for _, s := range [][]byte{ticketSchema, slaPolicySchema} {
		if !json.Valid(s) {
			panic("helpdesk: embedded schema is not valid JSON")
		}
	}
}

// RegisterKTypes upserts every helpdesk KType against the registry.
// Matches the finance/crm/inventory pattern so services/api/main.go
// can call it alongside the other domain registrations.
func RegisterKTypes(ctx context.Context, registry *ktype.PGRegistry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("helpdesk: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
