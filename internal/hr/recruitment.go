// Session 16 — Recruitment module KType definitions.
//
// The four recruitment KTypes (job_opening, job_application, interview,
// offer_letter) follow the manufacturing module convention: the KType
// schema is registered primarily so generic list / form / kanban views
// and agent tools can reference the type by name, while the authoritative
// store is the dedicated typed tables in migrations/000082_recruitment.sql
// (see internal/hr/recruitment_store.go). Keeping the lifecycle (status)
// state machine in the Go store — rather than the KRecord workflow engine —
// matches manufacturing work orders, whose draft→released→… lifecycle the
// recruitment pipeline mirrors.
//
// Employee, interviewer and referrer references point at hr.employee
// KRecords (krecords table), not a typed table, so those columns are bare
// UUIDs with no SQL foreign key — the same way the rest of the HR module
// links to employees.

package hr

import (
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// Recruitment KType identifiers. Exported so the router, agent tools, the
// KChat bridge and tests all reference the same canonical strings.
const (
	KTypeJobOpening     = "hr.job_opening"
	KTypeJobApplication = "hr.job_application"
	KTypeInterview      = "hr.interview"
	KTypeOfferLetter    = "hr.offer_letter"
)

// Canonical recruitment workflow names. The application lifecycle is
// enforced by the Go state machine in recruitment_store.go; the name is
// kept here so the embedded KType workflow block and the (optional)
// engine registration stay in lock-step. WorkflowOnboarding is the
// definition AdvanceApplication looks up when an application reaches
// 'hired' — when a tenant has registered it, the freshly-created
// employee KRecord is enrolled into a run.
const (
	WorkflowJobApplication = "hr.job_application.lifecycle"
	WorkflowOnboarding     = "hr.onboarding"
)

// jobOpeningSchema — a requisition / open position. status walks
// draft → open → on_hold → closed → filled; the Go store
// (PublishJobOpening / CloseJobOpening / AdvanceApplication's
// positions_filled bump) enforces the legal moves.
var jobOpeningSchema = []byte(`{
  "name": "hr.job_opening",
  "version": 1,
  "fields": [
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "department", "type": "string", "max_length": 120},
    {"name": "description", "type": "text"},
    {"name": "requirements", "type": "text"},
    {"name": "employment_type", "type": "enum", "values": ["full_time", "part_time", "contract", "intern"], "default": "full_time"},
    {"name": "location", "type": "string", "max_length": 200},
    {"name": "salary_range_min", "type": "number", "min": 0},
    {"name": "salary_range_max", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "open", "on_hold", "closed", "filled"], "default": "draft"},
    {"name": "hiring_manager_id", "type": "ref", "ktype": "hr.employee"},
    {"name": "max_positions", "type": "integer", "min": 1, "default": 1},
    {"name": "positions_filled", "type": "integer", "min": 0, "default": 0},
    {"name": "published_at", "type": "datetime"},
    {"name": "closes_at", "type": "datetime"}
  ],
  "views": {
    "list": {"columns": ["title", "department", "employment_type", "location", "status", "positions_filled", "max_positions"]},
    "form": {"sections": [
      {"title": "Position", "fields": ["title", "department", "employment_type", "location", "hiring_manager_id"]},
      {"title": "Details", "fields": ["description", "requirements", "salary_range_min", "salary_range_max", "currency"]},
      {"title": "Pipeline", "fields": ["status", "max_positions", "positions_filled", "published_at", "closes_at"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "title", "card_subtitle": "department"}
  },
  "cards": {"summary": "{{title}} — {{department}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]},
  "agent_tools": ["hr.create_job_opening"]
}`)

// jobApplicationSchema — a candidate applying to a job_opening. status
// is the load-bearing field; AdvanceApplication validates the legal
// transitions, emits audit + events, and on 'hired' auto-creates a
// draft hr.employee KRecord.
var jobApplicationSchema = []byte(`{
  "name": "hr.job_application",
  "version": 1,
  "fields": [
    {"name": "job_opening_id", "type": "ref", "ktype": "hr.job_opening", "required": true},
    {"name": "applicant_name", "type": "string", "required": true, "max_length": 200},
    {"name": "applicant_email", "type": "string", "max_length": 320},
    {"name": "phone", "type": "string", "max_length": 40},
    {"name": "resume_file_id", "type": "ref", "ktype": "files.file"},
    {"name": "cover_letter", "type": "text"},
    {"name": "source", "type": "enum", "values": ["website", "referral", "linkedin", "agency", "other"], "default": "website"},
    {"name": "referrer_employee_id", "type": "ref", "ktype": "hr.employee"},
    {"name": "status", "type": "enum", "values": ["applied", "screening", "shortlisted", "interview", "offered", "hired", "rejected", "withdrawn"], "default": "applied"},
    {"name": "rating", "type": "integer", "min": 1, "max": 5},
    {"name": "notes", "type": "text"},
    {"name": "applied_at", "type": "datetime"}
  ],
  "views": {
    "list": {"columns": ["applicant_name", "applicant_email", "source", "status", "rating", "applied_at"]},
    "form": {"sections": [
      {"title": "Applicant", "fields": ["job_opening_id", "applicant_name", "applicant_email", "phone", "source", "referrer_employee_id"]},
      {"title": "Application", "fields": ["resume_file_id", "cover_letter", "applied_at"]},
      {"title": "Pipeline", "fields": ["status", "rating", "notes"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "applicant_name", "card_subtitle": "source"}
  },
  "cards": {"summary": "{{applicant_name}} — {{status}} (★{{rating}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]},
  "agent_tools": ["hr.advance_application", "hr.recommend_candidates"],
  "workflow": {
    "name": "hr.job_application.lifecycle",
    "initial_state": "applied",
    "states": ["applied", "screening", "shortlisted", "interview", "offered", "hired", "rejected", "withdrawn"],
    "transitions": [
      {"from": ["applied"], "to": "screening", "action": "screen"},
      {"from": ["screening"], "to": "shortlisted", "action": "shortlist"},
      {"from": ["shortlisted"], "to": "interview", "action": "interview"},
      {"from": ["interview"], "to": "offered", "action": "offer"},
      {"from": ["offered"], "to": "hired", "action": "hire"},
      {"from": ["applied", "screening", "shortlisted", "interview", "offered"], "to": "rejected", "action": "reject"},
      {"from": ["applied", "screening", "shortlisted", "interview", "offered"], "to": "withdrawn", "action": "withdraw"}
    ]
  }
}`)

// interviewSchema — a single interview round against an application.
// status walks scheduled → completed, with cancelled / no_show terminal
// states. recommendation feeds the hiring decision.
var interviewSchema = []byte(`{
  "name": "hr.interview",
  "version": 1,
  "fields": [
    {"name": "application_id", "type": "ref", "ktype": "hr.job_application", "required": true},
    {"name": "interviewer_id", "type": "ref", "ktype": "hr.employee"},
    {"name": "interview_type", "type": "enum", "values": ["phone", "video", "in_person", "panel", "technical", "cultural"], "default": "video"},
    {"name": "scheduled_at", "type": "datetime"},
    {"name": "duration_minutes", "type": "integer", "min": 0, "default": 60},
    {"name": "location", "type": "string", "max_length": 200},
    {"name": "meeting_link", "type": "string", "max_length": 500},
    {"name": "status", "type": "enum", "values": ["scheduled", "completed", "cancelled", "no_show"], "default": "scheduled"},
    {"name": "rating", "type": "integer", "min": 1, "max": 5},
    {"name": "feedback", "type": "text"},
    {"name": "recommendation", "type": "enum", "values": ["strong_yes", "yes", "neutral", "no", "strong_no"]}
  ],
  "views": {
    "list": {"columns": ["application_id", "interview_type", "interviewer_id", "scheduled_at", "status", "recommendation"]},
    "form": {"sections": [
      {"title": "Schedule", "fields": ["application_id", "interviewer_id", "interview_type", "scheduled_at", "duration_minutes", "location", "meeting_link"]},
      {"title": "Outcome", "fields": ["status", "rating", "recommendation", "feedback"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "application_id", "card_subtitle": "interview_type"}
  },
  "cards": {"summary": "{{interview_type}} interview — {{status}} ({{recommendation}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]},
  "agent_tools": ["hr.schedule_interview"]
}`)

// offerLetterSchema — an offer extended to an application's candidate.
// status walks draft → sent → accepted, with rejected / expired /
// withdrawn terminal states. The draft→sent move requires hiring-manager
// approval before the applicant email is dispatched (see SendOfferLetter).
var offerLetterSchema = []byte(`{
  "name": "hr.offer_letter",
  "version": 1,
  "fields": [
    {"name": "application_id", "type": "ref", "ktype": "hr.job_application", "required": true},
    {"name": "employee_template_id", "type": "ref", "ktype": "hr.employee"},
    {"name": "designation", "type": "string", "max_length": 200},
    {"name": "department", "type": "string", "max_length": 120},
    {"name": "salary", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "joining_date", "type": "date"},
    {"name": "probation_months", "type": "integer", "min": 0, "default": 0},
    {"name": "benefits", "type": "object"},
    {"name": "status", "type": "enum", "values": ["draft", "sent", "accepted", "rejected", "expired", "withdrawn"], "default": "draft"},
    {"name": "sent_at", "type": "datetime"},
    {"name": "responded_at", "type": "datetime"},
    {"name": "valid_until", "type": "datetime"}
  ],
  "views": {
    "list": {"columns": ["application_id", "designation", "department", "salary", "currency", "status", "valid_until"]},
    "form": {"sections": [
      {"title": "Offer", "fields": ["application_id", "designation", "department", "salary", "currency", "joining_date", "probation_months"]},
      {"title": "Terms", "fields": ["benefits", "employee_template_id"]},
      {"title": "Status", "fields": ["status", "sent_at", "responded_at", "valid_until"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "designation", "card_subtitle": "department"}
  },
  "cards": {"summary": "{{designation}} offer — {{salary}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["hr.admin", "tenant.admin"]}
}`)

// RecruitmentKTypes returns the four recruitment KTypes as a freshly
// constructed slice. Kept separate from All() so callers (and tests)
// can register the recruitment surface independently of the core HR
// KTypes if they wish.
func RecruitmentKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeJobOpening, Version: 1, Schema: jobOpeningSchema},
		{Name: KTypeJobApplication, Version: 1, Schema: jobApplicationSchema},
		{Name: KTypeInterview, Version: 1, Schema: interviewSchema},
		{Name: KTypeOfferLetter, Version: 1, Schema: offerLetterSchema},
	}
}

func init() {
	for _, kt := range RecruitmentKTypes() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("hr: embedded recruitment schema %q is not valid JSON", kt.Name))
		}
	}
}
