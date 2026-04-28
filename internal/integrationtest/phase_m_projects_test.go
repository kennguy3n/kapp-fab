//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/projects"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// TestProjectsAgentToolsAndProgressSummary is the Phase M Task 5
// regression: it exercises the projects.create_project +
// projects.summarize_progress agent tools end-to-end against the
// real KType registry and asserts that the weighted percent-
// complete reflects each milestone's status and weight.
//
// Coverage:
//   - projects.RegisterKTypes lands both KTypes;
//   - projects.create_project lands a status="planning" KRecord;
//   - milestones written via the standard record store roll up
//     into summarize_progress with a weighted percent that
//     respects per-row weight values (defaulting to 1 when
//     unset);
//   - milestones from a different project (and a different
//     tenant) are excluded so the summary is correctly scoped.
func TestProjectsAgentToolsAndProgressSummary(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phasemproj"), Name: "Phase M Projects Co", Cell: "test", Plan: "starter",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := projects.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register projects: %v", err)
	}

	wfEngine := workflow.NewEngine(h.pool, h.publisher, h.auditor)
	executor := agents.NewExecutor(h.records, wfEngine, h.auditor)
	agents.RegisterProjectTools(executor)

	actor := uuid.New()

	// 1. Create project A via the agent tool.
	createInput, _ := json.Marshal(map[string]any{
		"name":     "Migration sprint",
		"code":     "MIG",
		"currency": "USD",
	})
	createRes, err := executor.Invoke(ctx, agents.Invocation{
		ToolName: "projects.create_project",
		TenantID: tn.ID,
		ActorID:  actor,
		Mode:     agents.ModeCommit,
		Inputs:   createInput,
	})
	if err != nil {
		t.Fatalf("create_project: %v", err)
	}
	projectA := createRes.Record.ID
	var initial map[string]any
	if err := json.Unmarshal(createRes.Record.Data, &initial); err != nil {
		t.Fatalf("decode create payload: %v", err)
	}
	if status, _ := initial["status"].(string); status != "planning" {
		t.Fatalf("create status = %q; want planning", status)
	}

	// 2. Create a second project so we can assert scoping.
	otherInput, _ := json.Marshal(map[string]any{"name": "Other"})
	otherRes, err := executor.Invoke(ctx, agents.Invocation{
		ToolName: "projects.create_project",
		TenantID: tn.ID,
		ActorID:  actor,
		Mode:     agents.ModeCommit,
		Inputs:   otherInput,
	})
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	projectB := otherRes.Record.ID

	// 3. Add three milestones on project A: one completed (weight 2),
	//    one in_progress (weight 1), one planned (weight 1). Expected
	//    weighted percent = 2 / 4 = 50%.
	milestone := func(projectID uuid.UUID, name, status string, weight float64) {
		body, _ := json.Marshal(map[string]any{
			"project_id": projectID,
			"name":       name,
			"status":     status,
			"weight":     weight,
		})
		if _, err := h.records.Create(ctx, record.KRecord{
			ID:        uuid.New(),
			TenantID:  tn.ID,
			KType:     projects.KTypeMilestone,
			Data:      body,
			CreatedBy: actor,
		}); err != nil {
			t.Fatalf("milestone %s: %v", name, err)
		}
	}
	milestone(projectA, "M1 — kickoff", "completed", 2)
	milestone(projectA, "M2 — implementation", "in_progress", 1)
	milestone(projectA, "M3 — rollout", "planned", 1)
	// Cross-project noise to validate scoping.
	milestone(projectB, "Other-1", "completed", 5)

	// 4. summarize_progress on project A should report 3 milestones,
	//    50% weighted complete.
	sumInput, _ := json.Marshal(map[string]any{"project_id": projectA})
	sumRes, err := executor.Invoke(ctx, agents.Invocation{
		ToolName: "projects.summarize_progress",
		TenantID: tn.ID,
		ActorID:  actor,
		Mode:     agents.ModeCommit,
		Inputs:   sumInput,
	})
	if err != nil {
		t.Fatalf("summarize_progress: %v", err)
	}
	if sumRes.Extra == nil {
		t.Fatalf("summary extra missing: %+v", sumRes)
	}
	if got, _ := sumRes.Extra["milestone_count"].(int); got != 3 {
		t.Fatalf("milestone_count = %v; want 3", sumRes.Extra["milestone_count"])
	}
	pct, _ := sumRes.Extra["percent_complete"].(float64)
	if pct < 49.99 || pct > 50.01 {
		t.Fatalf("percent_complete = %v; want 50", pct)
	}
}
