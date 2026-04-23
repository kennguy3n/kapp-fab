// Payroll extends the Phase E HR catalog with the two KTypes needed to
// model a compensation structure — salary components (earnings and
// deductions) and the per-employee salary structure that references
// them. Keeping these next to the rest of hr.go means the web UI, the
// KChat bridge, and the agent-tool layer all drive off the same
// metadata file.
//
// The KTypes are intentionally declarative-only: there is no payroll
// run engine yet (that's a Phase H+ deliverable). The structure gives
// the AR / HR report builder something to aggregate against and gives
// the importer a stable target for migrating ERPNext/HRMS Salary
// Slip / Salary Structure rows into Kapp.
package hr

import "github.com/kennguy3n/kapp-fab/internal/ktype"

const (
	KTypeSalaryComponent = "hr.salary_component"
	KTypeSalaryStructure = "hr.salary_structure"
)

// salaryComponentSchema — reusable earning/deduction definition. A
// component has an `amount_type` of fixed or percentage; when
// percentage, the `amount` is interpreted as a % of base_salary on
// the referencing structure.
var salaryComponentSchema = []byte(`{
  "name": "hr.salary_component",
  "version": 1,
  "fields": [
    {"name": "code", "type": "string", "required": true, "max_length": 64},
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "type", "type": "enum", "values": ["earning", "deduction"], "required": true},
    {"name": "amount_type", "type": "enum", "values": ["fixed", "percentage"], "required": true, "default": "fixed"},
    {"name": "amount", "type": "number", "min": 0, "required": true},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "taxable", "type": "boolean", "default": true},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["code", "name", "type", "amount_type", "amount", "currency", "active"]},
    "form": {"sections": [{"title": "Component", "fields": ["code", "name", "type", "amount_type", "amount", "currency", "taxable", "active"]}]}
  },
  "cards": {"summary": "{{code}} — {{name}} ({{type}}, {{amount_type}} {{amount}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]}
}`)

// salaryStructureSchema — per-employee compensation bundle. The
// `components` array is a list of {component_id, override_amount,
// override_amount_type} entries so a structure can reuse the standard
// catalog while still allowing per-employee adjustments.
var salaryStructureSchema = []byte(`{
  "name": "hr.salary_structure",
  "version": 1,
  "fields": [
    {"name": "employee_id", "type": "ref", "ktype": "hr.employee", "required": true},
    {"name": "effective_from", "type": "date", "required": true},
    {"name": "effective_until", "type": "date"},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "base_salary", "type": "number", "min": 0, "required": true},
    {"name": "payment_frequency", "type": "enum", "values": ["monthly", "biweekly", "weekly"], "default": "monthly"},
    {"name": "components", "type": "array"},
    {"name": "status", "type": "enum", "values": ["draft", "active", "archived"], "default": "draft"}
  ],
  "views": {
    "list": {"columns": ["employee_id", "effective_from", "effective_until", "base_salary", "currency", "status"]},
    "form": {"sections": [
      {"title": "Structure", "fields": ["employee_id", "effective_from", "effective_until", "currency", "base_salary", "payment_frequency"]},
      {"title": "Components", "fields": ["components"]},
      {"title": "Lifecycle", "fields": ["status"]}
    ]}
  },
  "cards": {"summary": "{{employee_id}} — {{base_salary}} {{currency}} / {{payment_frequency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]}
}`)

// PayrollKTypes returns the payroll surface as a freshly-constructed
// slice so callers can register it alongside (or independently of) the
// Phase E HR catalog. hr.All() does NOT include these so existing
// deployments are unaffected until their registration flow opts in.
func PayrollKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeSalaryComponent, Version: 1, Schema: salaryComponentSchema},
		{Name: KTypeSalaryStructure, Version: 1, Schema: salaryStructureSchema},
	}
}
