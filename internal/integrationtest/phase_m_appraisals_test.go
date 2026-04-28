//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/dashboard"
	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// TestAppraisalToolsAndDashboardTile is the Phase M Task 4
// regression: it exercises the create_appraisal + submit_appraisal
// agent tools end-to-end against the real KType registry and
// asserts the Pending Reviews dashboard tile reflects the
// resulting status changes.
//
// Coverage:
//   - hr.AppraisalKTypes() registers cleanly into ktype.PGRegistry;
//   - hr.create_appraisal lands a status="draft" KRecord;
//   - hr.submit_appraisal flips status to "submitted" and stamps
//     submitted_at;
//   - dashboard.Summary.PendingReviews counts {submitted, reviewed}
//     and ignores draft + acknowledged so the tile is exactly the
//     reviewer/employee work-in-flight band;
//   - submitting an already-submitted appraisal errors instead of
//     silently double-counting.
func TestAppraisalToolsAndDashboardTile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _ := newTenantForHR(t, h)

	for _, kt := range hr.AppraisalKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register appraisal ktype %s: %v", kt.Name, err)
		}
	}

	wfEngine := workflow.NewEngine(h.pool, h.publisher, h.auditor)
	executor := agents.NewExecutor(h.records, wfEngine, h.auditor)
	agents.RegisterHRTools(executor, hr.NewStore(h.pool))

	actor := uuid.New()
	employee := uuid.New()
	reviewer := uuid.New()

	// 1. Create a draft via the agent tool.
	createInput, _ := json.Marshal(map[string]any{
		"employee_id": employee,
		"reviewer_id": reviewer,
		"cycle":       "2026-Q2",
	})
	createRes, err := executor.Invoke(ctx, agents.Invocation{
		ToolName:  "hr.create_appraisal",
		TenantID:  tn.ID,
		ActorID:   actor,
		Mode:      agents.ModeCommit,
		Confirmed: true,
		Inputs:    createInput,
	})
	if err != nil {
		t.Fatalf("hr.create_appraisal: %v", err)
	}
	if createRes.Record == nil {
		t.Fatalf("create result missing record: %+v", createRes)
	}
	apprID := createRes.Record.ID
	var initial map[string]any
	if err := json.Unmarshal(createRes.Record.Data, &initial); err != nil {
		t.Fatalf("decode create payload: %v", err)
	}
	if status, _ := initial["status"].(string); status != "draft" {
		t.Fatalf("create status = %q; want draft", status)
	}

	// 2. Pending tile is zero while the appraisal is still draft.
	store := dashboard.NewStore(h.pool)
	preSummary, err := store.ComputeSummary(ctx, tn.ID)
	if err != nil {
		t.Fatalf("dashboard pre: %v", err)
	}
	if preSummary.PendingReviews != 0 {
		t.Fatalf("pre-submit PendingReviews = %d; want 0", preSummary.PendingReviews)
	}

	// 3. Submit the appraisal — status flips to submitted, tile increments.
	submitInput, _ := json.Marshal(map[string]any{
		"appraisal_id": apprID,
	})
	submitRes, err := executor.Invoke(ctx, agents.Invocation{
		ToolName:  "hr.submit_appraisal",
		TenantID:  tn.ID,
		ActorID:   actor,
		Mode:      agents.ModeCommit,
		Confirmed: true,
		Inputs:    submitInput,
	})
	if err != nil {
		t.Fatalf("hr.submit_appraisal: %v", err)
	}
	var submitted map[string]any
	if err := json.Unmarshal(submitRes.Record.Data, &submitted); err != nil {
		t.Fatalf("decode submit payload: %v", err)
	}
	if status, _ := submitted["status"].(string); status != "submitted" {
		t.Fatalf("submit status = %q; want submitted", status)
	}
	if _, ok := submitted["submitted_at"].(string); !ok {
		t.Fatalf("submitted_at missing on payload: %+v", submitted)
	}

	postSummary, err := store.ComputeSummary(ctx, tn.ID)
	if err != nil {
		t.Fatalf("dashboard post: %v", err)
	}
	if postSummary.PendingReviews != 1 {
		t.Fatalf("post-submit PendingReviews = %d; want 1", postSummary.PendingReviews)
	}

	// 4. Re-submitting an already-submitted appraisal errors. The
	//    tool guards against the double-submit footgun so the tile
	//    can't be inflated by a retried prompt.
	if _, err := executor.Invoke(ctx, agents.Invocation{
		ToolName:  "hr.submit_appraisal",
		TenantID:  tn.ID,
		ActorID:   actor,
		Mode:      agents.ModeCommit,
		Confirmed: true,
		Inputs:    submitInput,
	}); err == nil {
		t.Fatalf("expected re-submit to error; got nil")
	}
}
