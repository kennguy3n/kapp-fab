// Package projects holds the Phase M Task 5 project + milestone
// KType definitions and the setup hook that registers them against
// a KType registry. Mirrors the structure of internal/crm and
// internal/hr so a deployment that opts out can simply skip the
// RegisterKTypes call from services/api/main.go.
//
// Projects are tenant-scoped KRecords; the schemas keep the
// project ↔ milestone hierarchy explicit via a ref field so the
// Gantt UI can fan out from one project page without an extra
// reference table. Tasks (tasks.task) gain a project_id ref in
// the Phase B schema so Phase M tasks can roll up against a
// project without a separate join table.
//
// Workflow lives in the standard KType workflow block:
//
//	projects.project    : planning → active → completed → archived
//	projects.milestone  : planned → in_progress → completed → cancelled
//
// Both are linear forward except for the project's archived state
// which is a one-way terminal so audit trails of finished work
// can't be re-opened to dodge accounting close. Feature-gated
// under the new FeatureProjects flag (see internal/tenant/plans.go).
package projects

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers and workflow names. Exported so the router,
// agent tools, and tests reference the same strings.
const (
	KTypeProject   = "projects.project"
	KTypeMilestone = "projects.milestone"

	WorkflowProject   = "projects.project.lifecycle"
	WorkflowMilestone = "projects.milestone.lifecycle"
)

var projectSchema = []byte(`{
  "name": "projects.project",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "description", "type": "text"},
    {"name": "code", "type": "string", "max_length": 32},
    {"name": "owner", "type": "ref", "ktype": "user"},
    {"name": "customer_id", "type": "ref", "ktype": "crm.customer"},
    {"name": "start_date", "type": "date"},
    {"name": "end_date", "type": "date"},
    {"name": "status", "type": "enum", "values": ["planning", "active", "completed", "archived"], "default": "planning"},
    {"name": "budget", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "tags", "type": "json"}
  ],
  "views": {
    "list": {"columns": ["name", "code", "owner", "start_date", "end_date", "status"]},
    "form": {"sections": [
      {"title": "Project", "fields": ["name", "code", "description", "owner", "customer_id"]},
      {"title": "Timeline", "fields": ["start_date", "end_date", "status"]},
      {"title": "Finance", "fields": ["budget", "currency"]},
      {"title": "Meta", "fields": ["tags"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "name", "card_subtitle": "code"}
  },
  "cards": {"summary": "{{name}} — {{status}} ({{start_date}} → {{end_date}})"},
  "permissions": {"read": ["tenant.member"], "write": ["projects.admin", "tenant.admin"]},
  "workflow": {
    "name": "projects.project.lifecycle",
    "initial_state": "planning",
    "states": ["planning", "active", "completed", "archived"],
    "transitions": [
      {"from": ["planning"], "to": "active", "action": "kickoff"},
      {"from": ["active"], "to": "completed", "action": "complete"},
      {"from": ["completed"], "to": "archived", "action": "archive"}
    ]
  },
  "agent_tools": ["projects.create_project", "projects.summarize_progress"]
}`)

var milestoneSchema = []byte(`{
  "name": "projects.milestone",
  "version": 1,
  "fields": [
    {"name": "project_id", "type": "ref", "ktype": "projects.project", "required": true},
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "description", "type": "text"},
    {"name": "due_date", "type": "date"},
    {"name": "completed_at", "type": "datetime"},
    {"name": "weight", "type": "number", "min": 0, "default": 1},
    {"name": "status", "type": "enum", "values": ["planned", "in_progress", "completed", "cancelled"], "default": "planned"}
  ],
  "views": {
    "list": {"columns": ["project_id", "name", "due_date", "weight", "status"]},
    "form": {"sections": [
      {"title": "Milestone", "fields": ["project_id", "name", "description", "due_date", "weight", "status"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "name", "card_subtitle": "due_date"}
  },
  "cards": {"summary": "{{name}} — {{status}} (due {{due_date}})"},
  "permissions": {"read": ["tenant.member"], "write": ["projects.admin", "tenant.admin"]},
  "workflow": {
    "name": "projects.milestone.lifecycle",
    "initial_state": "planned",
    "states": ["planned", "in_progress", "completed", "cancelled"],
    "transitions": [
      {"from": ["planned"], "to": "in_progress", "action": "start"},
      {"from": ["in_progress"], "to": "completed", "action": "complete"},
      {"from": ["planned", "in_progress"], "to": "cancelled", "action": "cancel"}
    ]
  }
}`)

// All returns every Phase M Task 5 KType as a freshly-constructed
// slice. Kept symmetric with crm.All() / hr.All() so the API
// service and integration tests both register the same set.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeProject, Version: 1, Schema: projectSchema},
		{Name: KTypeMilestone, Version: 1, Schema: milestoneSchema},
	}
}

func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("projects: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers the project + milestone KTypes against
// the supplied registry. Idempotent: the underlying PGRegistry
// upserts on conflict so repeated calls during cell warm-up are
// safe.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("projects: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
