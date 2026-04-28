package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/projects"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// RegisterProjectTools attaches the Phase M Task 5 project tools
// to an executor:
//
//	projects.create_project       — opens a new projects.project
//	                                in "planning" status.
//	projects.summarize_progress   — counts milestones by status
//	                                for a project and returns a
//	                                weighted percent-complete.
//
// Both tools are read/write-tolerant of a nil record store so the
// kernel-only test harness still loads them; commit-mode calls
// then return a clear error rather than panicking.
func RegisterProjectTools(x *Executor) {
	x.Register(&createProjectTool{executor: x})
	x.Register(&summarizeProjectProgressTool{executor: x})
}

// ----- projects.create_project -----
//
// Lightweight create: most projects come into being from a
// "kickoff this Q" prompt where the operator only knows the name
// and the customer. Fields not supplied default to the schema
// defaults; status is forced to "planning" so the workflow guard
// at the KType layer remains the single source of truth for
// transitions.

type createProjectInput struct {
	Name        string    `json:"name"`
	Code        string    `json:"code,omitempty"`
	Description string    `json:"description,omitempty"`
	Owner       uuid.UUID `json:"owner,omitempty"`
	CustomerID  uuid.UUID `json:"customer_id,omitempty"`
	StartDate   string    `json:"start_date,omitempty"`
	EndDate     string    `json:"end_date,omitempty"`
	Budget      float64   `json:"budget,omitempty"`
	Currency    string    `json:"currency,omitempty"`
}

type createProjectTool struct{ executor *Executor }

func (t *createProjectTool) Name() string               { return "projects.create_project" }
func (t *createProjectTool) RequiresConfirmation() bool { return false }
func (t *createProjectTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createProjectInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, errors.New("projects.create_project: name required")
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	data := map[string]any{
		"name":     in.Name,
		"status":   "planning",
		"currency": in.Currency,
	}
	if in.Code != "" {
		data["code"] = in.Code
	}
	if in.Description != "" {
		data["description"] = in.Description
	}
	if in.Owner != uuid.Nil {
		data["owner"] = in.Owner.String()
	}
	if in.CustomerID != uuid.Nil {
		data["customer_id"] = in.CustomerID.String()
	}
	if in.StartDate != "" {
		data["start_date"] = in.StartDate
	}
	if in.EndDate != "" {
		data["end_date"] = in.EndDate
	}
	if in.Budget > 0 {
		data["budget"] = in.Budget
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would create project %q (planning)", in.Name),
			Preview: preview,
		}, nil
	}
	if t.executor.records == nil {
		return nil, errors.New("projects.create_project: record store not configured")
	}
	body, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		ID:        uuid.New(),
		TenantID:  inv.TenantID,
		KType:     projects.KTypeProject,
		Data:      body,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	// Best-effort: start the project lifecycle run so the generic
	// workflow surface (/api/v1/workflow/...) can drive
	// planning → active → completed → archived transitions instead
	// of letting tenants change status via direct record updates.
	// Mirrors createDealTool's pattern.
	//
	// Nil-guard the workflow engine so the kernel-only test
	// harness (which wires `agents.NewExecutor(records, nil, ...)`
	// — see phase_l_test.go) doesn't panic after a successful
	// records.Create and leave behind an orphan project record.
	// Matches the lms_tools.go nil check.
	var run *workflow.WorkflowRun
	if t.executor.workflow != nil {
		run, _ = t.executor.workflow.StartRun(
			ctx, inv.TenantID, projects.WorkflowProject,
			rec.ID, "planning", inv.ActorID,
		)
	}
	return &Result{
		Summary: fmt.Sprintf("Project %s created (planning)", rec.ID),
		Record:  rec,
		Run:     run,
	}, nil
}

// ----- projects.summarize_progress -----
//
// Aggregates milestones for a given project and returns a
// percent-complete weighted by milestone.weight. The summary is
// deliberately read-only (RequiresConfirmation=false) so the
// /project status command and dashboard-digest queries can fan
// out without an approval gate.

type summarizeProjectProgressInput struct {
	ProjectID uuid.UUID `json:"project_id"`
}

type summarizeProjectProgressTool struct{ executor *Executor }

func (t *summarizeProjectProgressTool) Name() string               { return "projects.summarize_progress" }
func (t *summarizeProjectProgressTool) RequiresConfirmation() bool { return false }
func (t *summarizeProjectProgressTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in summarizeProjectProgressInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ProjectID == uuid.Nil {
		return nil, errors.New("projects.summarize_progress: project_id required")
	}
	if t.executor.records == nil {
		return nil, errors.New("projects.summarize_progress: record store not configured")
	}
	milestones, err := t.executor.records.ListByField(ctx, inv.TenantID,
		record.ListFilter{KType: projects.KTypeMilestone},
		"project_id", in.ProjectID.String(),
	)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	var totalWeight, doneWeight float64
	for _, m := range milestones {
		var data map[string]any
		if err := json.Unmarshal(m.Data, &data); err != nil {
			continue
		}
		status, _ := data["status"].(string)
		counts[status]++
		w := 1.0
		if v, ok := data["weight"].(float64); ok && v > 0 {
			w = v
		}
		totalWeight += w
		if status == "completed" {
			doneWeight += w
		}
	}
	percent := 0.0
	if totalWeight > 0 {
		percent = (doneWeight / totalWeight) * 100
	}
	summary := fmt.Sprintf("Project %s: %d milestone(s), %.0f%% complete (weighted)",
		in.ProjectID, len(milestones), percent)
	return &Result{
		Summary: summary,
		Extra: map[string]any{
			"milestone_count": len(milestones),
			"counts_by_status": counts,
			"percent_complete": percent,
			"total_weight":     totalWeight,
			"done_weight":      doneWeight,
		},
	}, nil
}
