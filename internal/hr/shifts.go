// shifts.go holds the Phase M shift-scheduling KTypes: a reusable
// `hr.shift_type` describing recurring shift windows (e.g. Morning
// 06:00–14:00) and a per-employee `hr.shift_assignment` that links
// an employee to a shift type for a given calendar day. The data is
// intentionally normalised so the Phase M calendar UI can pivot
// across employees + days without hauling shift definitions inline,
// and so the kchat-bridge presence cross-reference can fan out a
// single shift_assignment row check rather than walking a free-text
// schedule field on the employee record.
package hr

import (
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers for the Phase M shift surface. Kept as plain
// strings (matching the rest of the package) so schemas, agent
// tools, and the kchat-bridge presence sweeper all reference the
// same canonical name.
const (
	KTypeShiftType       = "hr.shift_type"
	KTypeShiftAssignment = "hr.shift_assignment"
)

// shiftTypeSchema models a reusable named shift template. Times are
// stored as HH:MM strings (string type rather than the time type)
// because the schedule grid is timezone-agnostic at the shift-type
// layer — the tenant's timezone is applied when the assignment is
// projected onto a real calendar date. Color is the swatch the
// calendar UI uses for the row stripe.
var shiftTypeSchema = []byte(`{
  "name": "hr.shift_type",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 80},
    {"name": "start_time", "type": "string", "required": true, "max_length": 5, "pattern": "^([01][0-9]|2[0-3]):[0-5][0-9]$"},
    {"name": "end_time", "type": "string", "required": true, "max_length": 5, "pattern": "^([01][0-9]|2[0-3]):[0-5][0-9]$"},
    {"name": "color", "type": "string", "max_length": 16},
    {"name": "department", "type": "string", "max_length": 120},
    {"name": "active", "type": "boolean", "default": true},
    {"name": "notes", "type": "string", "max_length": 2000}
  ],
  "views": {
    "list": {"columns": ["name", "department", "start_time", "end_time", "active"]},
    "form": {"sections": [{"title": "Shift", "fields": ["name", "department", "start_time", "end_time", "color", "active", "notes"]}]}
  },
  "cards": {"summary": "{{name}} ({{start_time}}\u2013{{end_time}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]}
}`)

// shiftAssignmentSchema is the per-day binding between an employee
// and a shift_type. shift_date is the local calendar date the
// assignment runs on (YYYY-MM-DD); the kchat-bridge presence
// sweeper indexes on (employee_id, shift_date) to detect late
// arrivals — an employee whose presence stayed `away` past
// start_time on their assigned date triggers a tardiness signal.
//
// status starts at `scheduled` and progresses through `confirmed`
// (when the employee acknowledges) → `worked` / `missed` after the
// shift_date elapses. The agent tool `hr.assign_shift` only ever
// writes `scheduled`; downstream lifecycle changes are owned by the
// presence sweeper + manual ops.
var shiftAssignmentSchema = []byte(`{
  "name": "hr.shift_assignment",
  "version": 1,
  "fields": [
    {"name": "employee_id", "type": "ref", "ktype": "hr.employee", "required": true},
    {"name": "shift_type_id", "type": "ref", "ktype": "hr.shift_type", "required": true},
    {"name": "shift_date", "type": "date", "required": true},
    {"name": "status", "type": "enum", "values": ["scheduled", "confirmed", "worked", "missed", "cancelled"], "default": "scheduled"},
    {"name": "notes", "type": "string", "max_length": 1000}
  ],
  "views": {
    "list": {"columns": ["shift_date", "employee_id", "shift_type_id", "status"]},
    "form": {"sections": [{"title": "Assignment", "fields": ["employee_id", "shift_type_id", "shift_date", "status", "notes"]}]}
  },
  "cards": {"summary": "{{employee_id}} \u2192 {{shift_type_id}} on {{shift_date}}"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin", "hr.scheduler"]},
  "agent_tools": ["hr.assign_shift"]
}`)

// ShiftKTypes returns the Phase M shift surface as a freshly-built
// slice, mirroring PayrollKTypes' pattern. Caller registers
// alongside the rest of the HR catalog so the registry is
// idempotent + opt-in.
func ShiftKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeShiftType, Version: 1, Schema: shiftTypeSchema},
		{Name: KTypeShiftAssignment, Version: 1, Schema: shiftAssignmentSchema},
	}
}

func init() {
	for _, kt := range ShiftKTypes() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("hr: shift schema %q is not valid JSON", kt.Name))
		}
	}
}
