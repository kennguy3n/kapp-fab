// Package hr holds the Phase E HR KType definitions: employees, leave
// requests, attendance, expense claims. The Phase E starter keeps
// workflows and state projections in KRecord / workflow engine; only
// the append-only leave_ledger table has a dedicated typed home
// (migrations/000006_hr.sql) so balance projections are cheap and
// auditable.
package hr

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers. Exported so the router, agent tools, and tests
// reference the same strings.
const (
	KTypeEmployee     = "hr.employee"
	KTypeLeaveRequest = "hr.leave_request"
	KTypeAttendance   = "hr.attendance"
	KTypeExpenseClaim = "hr.expense_claim"
)

// Canonical workflow names.
const (
	WorkflowLeaveRequest = "hr.leave_request.lifecycle"
	WorkflowExpenseClaim = "hr.expense_claim.lifecycle"
)

var employeeSchema = []byte(`{
  "name": "hr.employee",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "email", "type": "string", "max_length": 320},
    {"name": "department", "type": "string", "max_length": 120},
    {"name": "designation", "type": "string", "max_length": 120},
    {"name": "date_of_joining", "type": "date"},
    {"name": "status", "type": "enum", "values": ["active", "on_leave", "terminated"], "default": "active"},
    {"name": "reporting_to", "type": "ref", "ktype": "hr.employee"}
  ],
  "views": {
    "list": {"columns": ["name", "email", "department", "designation", "status"]},
    "form": {"sections": [{"title": "Employee", "fields": ["name", "email", "department", "designation", "date_of_joining", "status", "reporting_to"]}]}
  },
  "cards": {"summary": "{{name}} — {{designation}} ({{department}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]}
}`)

var leaveRequestSchema = []byte(`{
  "name": "hr.leave_request",
  "version": 1,
  "fields": [
    {"name": "employee_id", "type": "ref", "ktype": "hr.employee", "required": true},
    {"name": "leave_type", "type": "enum", "values": ["annual", "sick", "personal", "unpaid"], "required": true},
    {"name": "from_date", "type": "date", "required": true},
    {"name": "to_date", "type": "date", "required": true},
    {"name": "days", "type": "number", "min": 0},
    {"name": "reason", "type": "text"},
    {"name": "status", "type": "enum", "values": ["draft", "pending_approval", "approved", "rejected", "cancelled"], "default": "draft"},
    {"name": "approver_id", "type": "ref", "ktype": "hr.employee"}
  ],
  "views": {
    "list": {"columns": ["employee_id", "leave_type", "from_date", "to_date", "days", "status"]},
    "form": {"sections": [
      {"title": "Request", "fields": ["employee_id", "leave_type", "from_date", "to_date", "days", "reason"]},
      {"title": "Approval", "fields": ["status", "approver_id"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "employee_id", "card_subtitle": "leave_type"}
  },
  "cards": {"summary": "{{employee_id}} — {{leave_type}} {{from_date}} → {{to_date}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]},
  "workflow": {
    "name": "hr.leave_request.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "pending_approval", "approved", "rejected", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "pending_approval", "action": "submit"},
      {"from": ["pending_approval"], "to": "approved", "action": "approve"},
      {"from": ["pending_approval"], "to": "rejected", "action": "reject"},
      {"from": ["draft", "pending_approval"], "to": "cancelled", "action": "cancel"}
    ]
  }
}`)

var attendanceSchema = []byte(`{
  "name": "hr.attendance",
  "version": 1,
  "fields": [
    {"name": "employee_id", "type": "ref", "ktype": "hr.employee", "required": true},
    {"name": "date", "type": "date", "required": true},
    {"name": "status", "type": "enum", "values": ["present", "absent", "half_day", "on_leave"], "required": true},
    {"name": "check_in", "type": "datetime"},
    {"name": "check_out", "type": "datetime"},
    {"name": "note", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["date", "employee_id", "status", "check_in", "check_out"]},
    "form": {"sections": [{"title": "Attendance", "fields": ["employee_id", "date", "status", "check_in", "check_out", "note"]}]}
  },
  "cards": {"summary": "{{employee_id}} — {{date}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]}
}`)

var expenseClaimSchema = []byte(`{
  "name": "hr.expense_claim",
  "version": 1,
  "fields": [
    {"name": "employee_id", "type": "ref", "ktype": "hr.employee", "required": true},
    {"name": "expense_date", "type": "date", "required": true},
    {"name": "category", "type": "string", "max_length": 64},
    {"name": "amount", "type": "number", "min": 0, "required": true},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "description", "type": "text"},
    {"name": "status", "type": "enum", "values": ["draft", "pending_approval", "approved", "rejected", "reimbursed"], "default": "draft"},
    {"name": "approver_id", "type": "ref", "ktype": "hr.employee"}
  ],
  "views": {
    "list": {"columns": ["expense_date", "employee_id", "category", "amount", "currency", "status"]},
    "form": {"sections": [
      {"title": "Claim", "fields": ["employee_id", "expense_date", "category", "amount", "currency", "description"]},
      {"title": "Approval", "fields": ["status", "approver_id"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "employee_id", "card_subtitle": "amount"}
  },
  "cards": {"summary": "{{employee_id}} — {{category}} {{amount}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]},
  "workflow": {
    "name": "hr.expense_claim.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "pending_approval", "approved", "rejected", "reimbursed"],
    "transitions": [
      {"from": ["draft"], "to": "pending_approval", "action": "submit"},
      {"from": ["pending_approval"], "to": "approved", "action": "approve"},
      {"from": ["pending_approval"], "to": "rejected", "action": "reject"},
      {"from": ["approved"], "to": "reimbursed", "action": "reimburse"}
    ]
  }
}`)

// All returns every Phase E HR KType as a freshly-constructed slice.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeEmployee, Version: 1, Schema: employeeSchema},
		{Name: KTypeLeaveRequest, Version: 1, Schema: leaveRequestSchema},
		{Name: KTypeAttendance, Version: 1, Schema: attendanceSchema},
		{Name: KTypeExpenseClaim, Version: 1, Schema: expenseClaimSchema},
	}
}

func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("hr: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers every Phase E HR KType against the supplied
// registry. Idempotent: the underlying PGRegistry upserts on conflict.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("hr: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
