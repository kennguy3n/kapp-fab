// Package hr — Phase M Task 4 Appraisals surface.
//
// Performance reviews ride on KRecords + the existing approval chain
// engine rather than getting their own typed tables. Two KTypes are
// registered from this file:
//
//   - hr.appraisal_template — the question/competency rubric an HR
//     admin authors once and reuses across cycles. The schema only
//     defines the shape; templates themselves are tenant data.
//
//   - hr.appraisal — one in-flight or completed review for a
//     (cycle, employee, reviewer) tuple. Carries the draft response
//     payload and the four-state workflow used by the Pending
//     Reviews dashboard tile.
//
// The workflow is deliberately linear (draft → submitted → reviewed
// → acknowledged) and one-way: there is no back-edge to draft once
// submitted, mirroring how operators expect appraisal trails to
// work for compliance. The hr.submit_appraisal agent tool opens an
// approval chain targeting the reviewer; the dashboard tile counts
// rows in the {submitted, reviewed} band so a reviewer's queue is
// visible without an extra worklist filter.
//
// Both KTypes register through hr.AppraisalKTypes() rather than
// hr.All() so a deployment that doesn't want the surface yet can
// drop the registration call in services/api/main.go without
// affecting the Phase E base catalog. Feature-gated under the
// existing `hr` flag.
package hr

import "github.com/kennguy3n/kapp-fab/internal/ktype"

// KType identifiers and workflow names for the appraisal surface.
// Exported so router, agent tools, and tests share the same strings.
const (
	KTypeAppraisalTemplate = "hr.appraisal_template"
	KTypeAppraisal         = "hr.appraisal"

	WorkflowAppraisal = "hr.appraisal.lifecycle"
)

var appraisalTemplateSchema = []byte(`{
  "name": "hr.appraisal_template",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "description", "type": "text"},
    {"name": "cycle", "type": "string", "max_length": 64},
    {"name": "questions", "type": "json"},
    {"name": "competencies", "type": "json"},
    {"name": "rating_scale", "type": "enum", "values": ["1_to_5", "1_to_10", "qualitative"], "default": "1_to_5"},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["name", "cycle", "rating_scale", "active"]},
    "form": {"sections": [
      {"title": "Template", "fields": ["name", "description", "cycle", "rating_scale", "active"]},
      {"title": "Questions & Competencies", "fields": ["questions", "competencies"]}
    ]}
  },
  "cards": {"summary": "{{name}} — {{cycle}}"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]}
}`)

var appraisalSchema = []byte(`{
  "name": "hr.appraisal",
  "version": 1,
  "fields": [
    {"name": "template_id", "type": "ref", "ktype": "hr.appraisal_template"},
    {"name": "employee_id", "type": "ref", "ktype": "hr.employee", "required": true},
    {"name": "reviewer_id", "type": "ref", "ktype": "hr.employee", "required": true},
    {"name": "cycle", "type": "string", "max_length": 64},
    {"name": "period_start", "type": "date"},
    {"name": "period_end", "type": "date"},
    {"name": "self_assessment", "type": "json"},
    {"name": "reviewer_assessment", "type": "json"},
    {"name": "overall_rating", "type": "number", "min": 0, "max": 10},
    {"name": "summary", "type": "text"},
    {"name": "submitted_at", "type": "datetime"},
    {"name": "reviewed_at", "type": "datetime"},
    {"name": "acknowledged_at", "type": "datetime"},
    {"name": "status", "type": "enum", "values": ["draft", "submitted", "reviewed", "acknowledged"], "default": "draft"}
  ],
  "views": {
    "list": {"columns": ["employee_id", "reviewer_id", "cycle", "status", "overall_rating"]},
    "form": {"sections": [
      {"title": "Subject", "fields": ["template_id", "employee_id", "reviewer_id", "cycle", "period_start", "period_end"]},
      {"title": "Assessments", "fields": ["self_assessment", "reviewer_assessment", "overall_rating", "summary"]},
      {"title": "Lifecycle", "fields": ["status", "submitted_at", "reviewed_at", "acknowledged_at"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "employee_id", "card_subtitle": "cycle"}
  },
  "cards": {"summary": "{{employee_id}} — {{cycle}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]},
  "workflow": {
    "name": "hr.appraisal.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "submitted", "reviewed", "acknowledged"],
    "transitions": [
      {"from": ["draft"], "to": "submitted", "action": "submit"},
      {"from": ["submitted"], "to": "reviewed", "action": "review"},
      {"from": ["reviewed"], "to": "acknowledged", "action": "acknowledge"}
    ]
  }
}`)

// AppraisalKTypes returns the Phase M Task 4 appraisal KTypes as a
// freshly-constructed slice so callers in services/api/main.go can
// register them without affecting the Phase E base hr.All() set.
// Kept opt-in (parallel to PayrollKTypes / ShiftKTypes) so a
// deployment that doesn't want the appraisal surface yet can drop
// the registration call without forking the catalog.
func AppraisalKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeAppraisalTemplate, Version: 1, Schema: appraisalTemplateSchema},
		{Name: KTypeAppraisal, Version: 1, Schema: appraisalSchema},
	}
}
