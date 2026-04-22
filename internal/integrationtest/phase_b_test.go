//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/forms"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// newTenantWithCRM registers every Phase B KType and the deal pipeline
// workflow under a fresh tenant, returning the tenant and a workflow
// engine wired to the harness's pool/events/audit.
func newTenantWithCRM(t *testing.T, h *harness) (*tenant.Tenant, *workflow.Engine) {
	t.Helper()
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phaseb"), Name: "Phase B Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	// Register KTypes. The registry is global (not tenant-scoped) so
	// name collisions across parallel tests would bite — we suffix every
	// KType with the tenant id to keep tests hermetic while still
	// exercising the real registry path.
	suffix := tn.ID.String()[:8]
	if err := registerPhaseBKTypes(ctx, h.ktypes, suffix); err != nil {
		t.Fatalf("register ktypes: %v", err)
	}
	engine := workflow.NewEngine(h.pool, h.publisher, h.auditor)
	if err := engine.RegisterWorkflow(ctx, workflow.WorkflowDef{
		TenantID: tn.ID,
		Name:     crm.WorkflowDealPipeline,
		Version:  1,
		Definition: workflow.Definition{
			InitialState: "qualification",
			States:       []string{"qualification", "proposal", "negotiation", "won", "lost"},
			Transitions: []workflow.Transition{
				{From: []string{"qualification"}, To: "proposal", Action: "advance_to_proposal"},
				{From: []string{"proposal"}, To: "negotiation", Action: "advance_to_negotiation"},
				{From: []string{"negotiation"}, To: "won", Action: "mark_won", Post: []string{"finance.create_sales_invoice"}},
				{From: []string{"qualification", "proposal", "negotiation"}, To: "lost", Action: "mark_lost"},
			},
		},
	}); err != nil {
		t.Fatalf("register workflow: %v", err)
	}
	return tn, engine
}

// registerPhaseBKTypes writes simplified deal + task KTypes into the
// registry. We deliberately use an in-line minimal schema here instead
// of re-using crm.All() — the latter embeds rich views/workflow blocks
// that the integration harness doesn't need and would make the tests
// slower without changing what's being verified.
func registerPhaseBKTypes(ctx context.Context, reg *ktype.PGRegistry, suffix string) error {
	dealSchema := json.RawMessage(`{"fields":[
		{"name":"name","type":"string","required":true},
		{"name":"stage","type":"enum","values":["qualification","proposal","negotiation","won","lost"],"required":true},
		{"name":"amount","type":"number"},
		{"name":"currency","type":"string"}
	]}`)
	taskSchema := json.RawMessage(`{"fields":[
		{"name":"title","type":"string","required":true},
		{"name":"assignee","type":"string","required":true},
		{"name":"status","type":"enum","values":["open","in_progress","done","cancelled"],"required":true},
		{"name":"due_date","type":"date"},
		{"name":"description","type":"text"}
	]}`)
	for _, kt := range []ktype.KType{
		{Name: fmt.Sprintf("crm.deal-%s", suffix), Version: 1, Schema: dealSchema},
		{Name: fmt.Sprintf("tasks.task-%s", suffix), Version: 1, Schema: taskSchema},
	} {
		if err := reg.Register(ctx, kt); err != nil {
			return err
		}
	}
	// Also register under the canonical crm.deal / tasks.task names on
	// first run so the agent tools (which hardcode those names) can
	// operate. Using ON CONFLICT DO NOTHING in the registry means this
	// is a no-op on subsequent tenants.
	for _, kt := range []ktype.KType{
		{Name: crm.KTypeDeal, Version: 1, Schema: dealSchema},
		{Name: crm.KTypeTask, Version: 1, Schema: taskSchema},
	} {
		if err := reg.Register(ctx, kt); err != nil {
			// A duplicate-version error is expected on subsequent runs —
			// swallow it so tests are idempotent.
			continue
		}
	}
	return nil
}

// TestWorkflowPipelineEndToEnd walks a deal through the full Phase B
// pipeline: qualification → proposal → negotiation → won, and verifies
// every transition writes a workflow_runs history row, a
// workflow.transitioned event on the outbox, and an audit_log entry.
func TestWorkflowPipelineEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	actor := uuid.New()
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     crm.KTypeDeal,
		Data:      json.RawMessage(`{"name":"Acme Q1","stage":"qualification","amount":10000,"currency":"USD"}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create deal: %v", err)
	}
	run, err := engine.StartRun(ctx, tn.ID, crm.WorkflowDealPipeline, rec.ID, "", actor)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run.State != "qualification" {
		t.Fatalf("initial state = %q; want qualification", run.State)
	}

	for _, action := range []string{"advance_to_proposal", "advance_to_negotiation", "mark_won"} {
		run, err = engine.Transition(ctx, tn.ID, run.ID, action, actor)
		if err != nil {
			t.Fatalf("transition %s: %v", action, err)
		}
	}
	if run.State != "won" {
		t.Fatalf("final state = %q; want won", run.State)
	}
	if len(run.History) != 3 {
		t.Fatalf("history len = %d; want 3 (%+v)", len(run.History), run.History)
	}

	// Illegal transition: won → anything should fail.
	if _, err := engine.Transition(ctx, tn.ID, run.ID, "mark_lost", actor); err == nil {
		t.Fatalf("expected illegal-transition error from terminal state")
	}

	// Events: we want workflow.started once + 3× workflow.transitioned.
	counts, err := eventCountsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if counts["workflow.started"] != 1 {
		t.Fatalf("workflow.started count = %d; want 1 (%v)", counts["workflow.started"], counts)
	}
	if counts["workflow.transitioned"] != 3 {
		t.Fatalf("workflow.transitioned count = %d; want 3 (%v)", counts["workflow.transitioned"], counts)
	}
}

// TestApprovalChainApproveAndReject exercises both sides of the
// approvals engine: a multi-approver chain that advances on quorum, and
// a separate chain that finalizes as rejected on the first reject.
func TestApprovalChainApproveAndReject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	actor := uuid.New()
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     crm.KTypeDeal,
		Data:      json.RawMessage(`{"name":"Approval deal","stage":"qualification"}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create deal: %v", err)
	}

	alice := uuid.New()
	bob := uuid.New()
	carol := uuid.New()

	approval, err := engine.RequestApproval(ctx, tn.ID, crm.KTypeDeal, rec.ID, workflow.ApprovalChain{
		Steps: []workflow.ApprovalStep{
			// Step 0: 2-of-2 quorum (alice + bob must both approve).
			{Approvers: []uuid.UUID{alice, bob}, RequiredCount: 2},
			// Step 1: any one of {carol}.
			{Approvers: []uuid.UUID{carol}, RequiredCount: 1},
		},
	}, actor)
	if err != nil {
		t.Fatalf("request approval: %v", err)
	}

	// Non-approver cannot decide.
	if _, err := engine.Decide(ctx, tn.ID, approval.ID, workflow.DecisionApprove, actor); !errors.Is(err, workflow.ErrApprovalNotAuthorized) {
		t.Fatalf("expected ErrApprovalNotAuthorized, got %v", err)
	}

	// Alice approves → still pending (quorum = 2).
	after, err := engine.Decide(ctx, tn.ID, approval.ID, workflow.DecisionApprove, alice)
	if err != nil {
		t.Fatalf("alice approve: %v", err)
	}
	if after.State != workflow.ApprovalStatePending {
		t.Fatalf("after alice state = %q; want pending", after.State)
	}

	// Duplicate decision from alice is rejected.
	if _, err := engine.Decide(ctx, tn.ID, approval.ID, workflow.DecisionApprove, alice); !errors.Is(err, workflow.ErrApprovalDuplicate) {
		t.Fatalf("expected ErrApprovalDuplicate, got %v", err)
	}

	// Bob approves → step 0 done, advance to step 1 (carol).
	after, err = engine.Decide(ctx, tn.ID, approval.ID, workflow.DecisionApprove, bob)
	if err != nil {
		t.Fatalf("bob approve: %v", err)
	}
	if after.Chain.CurrentStep != 1 {
		t.Fatalf("step after bob = %d; want 1", after.Chain.CurrentStep)
	}

	// Carol approves → final.
	after, err = engine.Decide(ctx, tn.ID, approval.ID, workflow.DecisionApprove, carol)
	if err != nil {
		t.Fatalf("carol approve: %v", err)
	}
	if after.State != workflow.ApprovalStateApproved {
		t.Fatalf("final state = %q; want approved", after.State)
	}

	// Separate approval — test rejection path.
	reject, err := engine.RequestApproval(ctx, tn.ID, crm.KTypeDeal, rec.ID, workflow.ApprovalChain{
		Steps: []workflow.ApprovalStep{{Approvers: []uuid.UUID{alice, bob}, RequiredCount: 1}},
	}, actor)
	if err != nil {
		t.Fatalf("request reject approval: %v", err)
	}
	rejectFinal, err := engine.Decide(ctx, tn.ID, reject.ID, workflow.DecisionReject, alice)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejectFinal.State != workflow.ApprovalStateRejected {
		t.Fatalf("reject state = %q; want rejected", rejectFinal.State)
	}

	// Events: one requested per approval, one granted (for approved) + one rejected.
	counts, err := eventCountsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if counts["approval.requested"] != 2 {
		t.Fatalf("approval.requested = %d; want 2 (%v)", counts["approval.requested"], counts)
	}
	if counts["approval.granted"] != 1 {
		t.Fatalf("approval.granted = %d; want 1 (%v)", counts["approval.granted"], counts)
	}
	if counts["approval.rejected"] != 1 {
		t.Fatalf("approval.rejected = %d; want 1 (%v)", counts["approval.rejected"], counts)
	}

	// ListPendingApprovals should now return nothing for alice / bob / carol.
	for _, u := range []uuid.UUID{alice, bob, carol} {
		list, err := engine.ListPendingApprovals(ctx, tn.ID, u)
		if err != nil {
			t.Fatalf("list pending for %s: %v", u, err)
		}
		if len(list) != 0 {
			t.Fatalf("pending for %s = %d; want 0", u, len(list))
		}
	}
}

// TestFormsPublicSubmission exercises the two-pool submission flow: a
// form is created under the tenant, then submitted without any X-Tenant
// header — the store must derive tenant from the form and create the
// KRecord under the correct RLS context.
func TestFormsPublicSubmission(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping forms public submission test")
	}
	ctx := context.Background()
	tn, _ := newTenantWithCRM(t, h)

	store := forms.NewStore(h.pool, h.ktypes, h.records).WithAdminPool(h.adminPool)

	// Form 1: allows anonymous submissions.
	form, err := store.Create(ctx, tn.ID, crm.KTypeTask, forms.Config{
		AllowAnonymous: true,
		Title:          "Contact us",
	}, uuid.New())
	if err != nil {
		t.Fatalf("create form: %v", err)
	}

	got, err := store.GetPublic(ctx, form.ID)
	if err != nil {
		t.Fatalf("get public: %v", err)
	}
	if got.TenantID != tn.ID {
		t.Fatalf("form tenant mismatch: got %s want %s", got.TenantID, tn.ID)
	}

	// Anonymous submission creates a KRecord owned by the sentinel.
	rec, err := store.Submit(ctx, form.ID, map[string]any{
		"title":    "Anon lead",
		"assignee": uuid.New().String(),
		"status":   "open",
	}, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rec.TenantID != tn.ID {
		t.Fatalf("record tenant mismatch: got %s want %s", rec.TenantID, tn.ID)
	}
	if rec.CreatedBy != forms.AnonymousSubmitter {
		t.Fatalf("created_by = %s; want AnonymousSubmitter", rec.CreatedBy)
	}

	// Form 2: anonymous submission disabled.
	locked, err := store.Create(ctx, tn.ID, crm.KTypeTask, forms.Config{AllowAnonymous: false}, uuid.New())
	if err != nil {
		t.Fatalf("create locked form: %v", err)
	}
	if _, err := store.Submit(ctx, locked.ID, map[string]any{"title": "x", "assignee": uuid.New().String(), "status": "open"}, nil); err == nil {
		t.Fatalf("expected error submitting anonymously to disabled form")
	}
}

// TestAgentToolsDryRunAndCommit verifies the two Phase B execution
// modes for the create_deal tool: a dry-run returns a preview and
// leaves the DB untouched; a commit inserts a KRecord and starts a
// workflow run. It also confirms the audit log picks up the agent
// actor kind.
func TestAgentToolsDryRunAndCommit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	executor := agents.NewExecutor(h.records, engine, h.auditor)
	agents.RegisterCRMTools(executor)

	actor := uuid.New()
	inputs, _ := json.Marshal(map[string]any{
		"name":  "Agent-created deal",
		"stage": "qualification",
	})

	// Dry-run: no record should be created.
	before, err := h.records.List(ctx, tn.ID, record.ListFilter{KType: crm.KTypeDeal})
	if err != nil {
		t.Fatalf("pre-list: %v", err)
	}
	dry, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "crm.create_deal",
		Inputs: inputs, Mode: agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Record != nil {
		t.Fatalf("dry run produced a record: %+v", dry.Record)
	}
	after, err := h.records.List(ctx, tn.ID, record.ListFilter{KType: crm.KTypeDeal})
	if err != nil {
		t.Fatalf("post-list: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("dry run mutated store: before=%d after=%d", len(before), len(after))
	}

	// Commit: record is created + a run is started.
	commit, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "crm.create_deal",
		Inputs: inputs, Mode: agents.ModeCommit,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if commit.Record == nil || commit.Run == nil {
		t.Fatalf("commit missing record/run: %+v", commit)
	}

	// approvals.decide requires confirmation=true in commit mode.
	decideInputs, _ := json.Marshal(map[string]any{
		"approval_id": uuid.New(),
		"decision":    workflow.DecisionApprove,
	})
	if _, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "approvals.decide",
		Inputs: decideInputs, Mode: agents.ModeCommit,
	}); !errors.Is(err, agents.ErrConfirmationRequired) {
		t.Fatalf("expected ErrConfirmationRequired for unconfirmed decide, got %v", err)
	}

	// Audit log should contain agent.tool.* entries for both invocations.
	actions, err := auditActionsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	agentActions := 0
	for _, a := range actions {
		if a == "agent.tool.dry_run" || a == "agent.tool.commit" {
			agentActions++
		}
	}
	if agentActions < 2 {
		t.Fatalf("agent audit actions = %d; want >= 2 (%v)", agentActions, actions)
	}
}

// TestRLSDealIsolation confirms that the CRM surface is tenant-isolated
// end-to-end. Cross-tenant reads return zero rows even against the
// shared KType, which is the Phase B analog of the Phase A demo.note
// RLS test.
func TestRLSDealIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	aTN, _ := newTenantWithCRM(t, h)
	bTN, _ := newTenantWithCRM(t, h)

	actor := uuid.New()
	if _, err := h.records.Create(ctx, record.KRecord{
		TenantID: aTN.ID, KType: crm.KTypeDeal,
		Data:      json.RawMessage(`{"name":"A only","stage":"qualification"}`),
		CreatedBy: actor,
	}); err != nil {
		t.Fatalf("create A deal: %v", err)
	}

	// Tenant B sees no deals.
	list, err := h.records.List(ctx, bTN.ID, record.ListFilter{KType: crm.KTypeDeal})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	for _, r := range list {
		if r.TenantID == aTN.ID {
			t.Fatalf("RLS leak: tenant B saw tenant A row id=%s", r.ID)
		}
	}
}

// TestAdvanceDealTool covers the crm.advance_deal tool end-to-end: a
// deal is seeded via crm.create_deal (which also starts the pipeline
// run), then advance_deal is invoked in dry-run and commit modes. The
// dry-run must not mutate the run state; the commit must produce a new
// history entry and move the run to the target state.
func TestAdvanceDealTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	executor := agents.NewExecutor(h.records, engine, h.auditor)
	agents.RegisterCRMTools(executor)

	actor := uuid.New()
	createInputs, _ := json.Marshal(map[string]any{
		"name":  "Advance target",
		"stage": "qualification",
	})
	created, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "crm.create_deal",
		Inputs: createInputs, Mode: agents.ModeCommit,
	})
	if err != nil {
		t.Fatalf("create_deal commit: %v", err)
	}
	if created.Record == nil || created.Run == nil {
		t.Fatalf("create_deal missing record/run: %+v", created)
	}

	advanceInputs, _ := json.Marshal(map[string]any{
		"record_id": created.Record.ID,
		"action":    "advance_to_proposal",
	})

	// Dry-run should not mutate the run.
	dry, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "crm.advance_deal",
		Inputs: advanceInputs, Mode: agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("advance_deal dry: %v", err)
	}
	if dry.Run == nil || dry.Run.State != "qualification" {
		t.Fatalf("dry run mutated state: %+v", dry.Run)
	}
	latest, err := engine.GetRunByRecord(ctx, tn.ID, created.Record.ID)
	if err != nil {
		t.Fatalf("get run after dry: %v", err)
	}
	if latest.State != "qualification" || len(latest.History) != 0 {
		t.Fatalf("dry run persisted: state=%s history=%d", latest.State, len(latest.History))
	}

	// Commit should apply the transition.
	commit, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "crm.advance_deal",
		Inputs: advanceInputs, Mode: agents.ModeCommit,
	})
	if err != nil {
		t.Fatalf("advance_deal commit: %v", err)
	}
	if commit.Run == nil || commit.Run.State != "proposal" {
		t.Fatalf("commit state = %+v; want proposal", commit.Run)
	}
	if len(commit.Run.History) != 1 {
		t.Fatalf("commit history len = %d; want 1", len(commit.Run.History))
	}
}

// TestSummarizePipelineTool seeds three deals across three stages and
// verifies the read-only crm.summarize_pipeline tool returns accurate
// per-stage counts and totals. The tool is read-only so both modes
// must behave identically.
func TestSummarizePipelineTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	executor := agents.NewExecutor(h.records, engine, h.auditor)
	agents.RegisterCRMTools(executor)

	actor := uuid.New()
	seed := []struct {
		name   string
		stage  string
		amount float64
	}{
		{"Alpha", "qualification", 1000},
		{"Beta", "qualification", 2500},
		{"Gamma", "proposal", 4000},
	}
	for _, s := range seed {
		input, _ := json.Marshal(map[string]any{
			"name": s.name, "stage": s.stage, "amount": s.amount,
		})
		if _, err := executor.Invoke(ctx, agents.Invocation{
			TenantID: tn.ID, ActorID: actor, ToolName: "crm.create_deal",
			Inputs: input, Mode: agents.ModeCommit,
		}); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
	}

	res, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "crm.summarize_pipeline",
		Inputs: json.RawMessage(`{}`), Mode: agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	type bucket struct {
		Count int     `json:"count"`
		Total float64 `json:"total"`
	}
	var got map[string]bucket
	if err := json.Unmarshal(res.Preview, &got); err != nil {
		t.Fatalf("decode summary: %v (preview=%s)", err, res.Preview)
	}
	if got["qualification"].Count != 2 || got["qualification"].Total != 3500 {
		t.Fatalf("qualification bucket = %+v; want count=2 total=3500", got["qualification"])
	}
	if got["proposal"].Count != 1 || got["proposal"].Total != 4000 {
		t.Fatalf("proposal bucket = %+v; want count=1 total=4000", got["proposal"])
	}
}

// TestCreateTaskTool covers the tasks.create_task tool. A dry-run must
// leave the record store untouched; a commit must insert a row with
// the supplied title/assignee and default status=open.
func TestCreateTaskTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	executor := agents.NewExecutor(h.records, engine, h.auditor)
	agents.RegisterCRMTools(executor)

	actor := uuid.New()
	assignee := uuid.New()
	inputs, _ := json.Marshal(map[string]any{
		"title":    "Follow up with Acme",
		"assignee": assignee.String(),
	})

	before, err := h.records.List(ctx, tn.ID, record.ListFilter{KType: crm.KTypeTask})
	if err != nil {
		t.Fatalf("pre-list: %v", err)
	}

	dry, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "tasks.create_task",
		Inputs: inputs, Mode: agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("task dry: %v", err)
	}
	if dry.Record != nil {
		t.Fatalf("dry run produced a record: %+v", dry.Record)
	}
	after, err := h.records.List(ctx, tn.ID, record.ListFilter{KType: crm.KTypeTask})
	if err != nil {
		t.Fatalf("post-dry list: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("dry run mutated store: before=%d after=%d", len(before), len(after))
	}

	commit, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "tasks.create_task",
		Inputs: inputs, Mode: agents.ModeCommit,
	})
	if err != nil {
		t.Fatalf("task commit: %v", err)
	}
	if commit.Record == nil {
		t.Fatalf("commit missing record: %+v", commit)
	}
	var payload struct {
		Title    string `json:"title"`
		Assignee string `json:"assignee"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(commit.Record.Data, &payload); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if payload.Title != "Follow up with Acme" {
		t.Fatalf("title = %q", payload.Title)
	}
	if payload.Assignee != assignee.String() {
		t.Fatalf("assignee = %q; want %s", payload.Assignee, assignee)
	}
	if payload.Status != "open" {
		t.Fatalf("status = %q; want open", payload.Status)
	}
}

// TestRequestApprovalTool verifies the approvals.request tool: a
// dry-run must not create an approval, and a commit must create one
// that shows up in ListPendingApprovals for the step-0 approver.
func TestRequestApprovalTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	executor := agents.NewExecutor(h.records, engine, h.auditor)
	agents.RegisterCRMTools(executor)

	actor := uuid.New()
	approver := uuid.New()

	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     crm.KTypeDeal,
		Data:      json.RawMessage(`{"name":"Needs approval","stage":"qualification"}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create deal: %v", err)
	}

	chain := workflow.ApprovalChain{
		Steps: []workflow.ApprovalStep{
			{Approvers: []uuid.UUID{approver}, RequiredCount: 1},
		},
	}
	inputs, _ := json.Marshal(map[string]any{
		"record_ktype": crm.KTypeDeal,
		"record_id":    rec.ID,
		"chain":        chain,
	})

	// Dry-run: no approval created.
	if _, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "approvals.request",
		Inputs: inputs, Mode: agents.ModeDryRun,
	}); err != nil {
		t.Fatalf("approval dry: %v", err)
	}
	pending, err := engine.ListPendingApprovals(ctx, tn.ID, approver)
	if err != nil {
		t.Fatalf("list after dry: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("dry run created approval: %+v", pending)
	}

	// Commit: approval appears in the pending list.
	commit, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: actor, ToolName: "approvals.request",
		Inputs: inputs, Mode: agents.ModeCommit,
	})
	if err != nil {
		t.Fatalf("approval commit: %v", err)
	}
	if commit.Extra == nil || commit.Extra["approval_id"] == nil {
		t.Fatalf("commit missing approval_id: %+v", commit.Extra)
	}
	pending, err = engine.ListPendingApprovals(ctx, tn.ID, approver)
	if err != nil {
		t.Fatalf("list after commit: %v", err)
	}
	if len(pending) != 1 || pending[0].RecordID != rec.ID {
		t.Fatalf("pending after commit = %+v", pending)
	}
}

// TestDealLifecycleEndToEnd is the Phase B acceptance test. It stitches
// together every subsystem that a deal touches: record creation
// (mirroring the KChat /deal command), workflow run start, a full
// stage progression, and a two-step approval that both begins and
// finalizes. Events and audit actions are asserted at the end so a
// failure in any of the upstream writes surfaces a clear message.
func TestDealLifecycleEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, engine := newTenantWithCRM(t, h)

	creator := uuid.New()
	approverA := uuid.New()
	approverB := uuid.New()

	// 1. Create a deal as if /deal were invoked from KChat.
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID,
		KType:    crm.KTypeDeal,
		Data: json.RawMessage(
			`{"name":"Enterprise deal","stage":"qualification","amount":50000,"currency":"USD"}`,
		),
		CreatedBy: creator,
	})
	if err != nil {
		t.Fatalf("create deal: %v", err)
	}

	// 2. Start the workflow run (mirrors the server's workflowNameFor
	// path — the API handler would do this on the caller's behalf).
	run, err := engine.StartRun(ctx, tn.ID, crm.WorkflowDealPipeline, rec.ID, "", creator)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run.State != "qualification" {
		t.Fatalf("initial state = %q; want qualification", run.State)
	}

	// 3. Transition qualification → proposal → negotiation → won.
	for _, action := range []string{
		"advance_to_proposal", "advance_to_negotiation", "mark_won",
	} {
		run, err = engine.Transition(ctx, tn.ID, run.ID, action, creator)
		if err != nil {
			t.Fatalf("transition %s: %v", action, err)
		}
	}
	if run.State != "won" {
		t.Fatalf("final state = %q; want won", run.State)
	}

	// 4. Request a two-step approval on the won deal (e.g. finance
	// sign-off before sales invoice) and drive it to approved.
	approval, err := engine.RequestApproval(ctx, tn.ID, crm.KTypeDeal, rec.ID, workflow.ApprovalChain{
		Steps: []workflow.ApprovalStep{
			{Approvers: []uuid.UUID{approverA}, RequiredCount: 1},
			{Approvers: []uuid.UUID{approverB}, RequiredCount: 1},
		},
	}, creator)
	if err != nil {
		t.Fatalf("request approval: %v", err)
	}
	a1, err := engine.Decide(ctx, tn.ID, approval.ID, workflow.DecisionApprove, approverA)
	if err != nil {
		t.Fatalf("approve step 0: %v", err)
	}
	if a1.Chain.CurrentStep != 1 {
		t.Fatalf("current step after A = %d; want 1", a1.Chain.CurrentStep)
	}
	final, err := engine.Decide(ctx, tn.ID, approval.ID, workflow.DecisionApprove, approverB)
	if err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	if final.State != workflow.ApprovalStateApproved {
		t.Fatalf("final approval state = %q", final.State)
	}

	// 5. Assert the event and audit trail covers the full lifecycle.
	counts, err := eventCountsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if counts["krecord.created"] < 1 {
		t.Fatalf("krecord.created = %d; want >= 1 (%v)", counts["krecord.created"], counts)
	}
	if counts["workflow.started"] != 1 {
		t.Fatalf("workflow.started = %d; want 1", counts["workflow.started"])
	}
	if counts["workflow.transitioned"] != 3 {
		t.Fatalf("workflow.transitioned = %d; want 3", counts["workflow.transitioned"])
	}
	if counts["approval.requested"] != 1 {
		t.Fatalf("approval.requested = %d; want 1", counts["approval.requested"])
	}
	if counts["approval.granted"] != 1 {
		t.Fatalf("approval.granted = %d; want 1", counts["approval.granted"])
	}

	actions, err := auditActionsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	// Audit actions differ from event types: record writes use
	// "<ktype>.create" and approval decisions use "approval.<decision>".
	required := []string{
		crm.KTypeDeal + ".create",
		"workflow.started",
		"workflow.transitioned",
		"approval.requested",
		"approval." + workflow.DecisionApprove,
	}
	for _, want := range required {
		found := false
		for _, got := range actions {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing audit action %q in %v", want, actions)
		}
	}
}

// --- helpers ---

// eventCountsForTenant reads the events outbox for a tenant. It wraps
// the read in dbutil.WithTenantTx so the read succeeds against either
// the kapp_app role (with SET LOCAL app.tenant_id = tenantID) or the
// kapp_admin BYPASSRLS role.
func eventCountsForTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (map[string]int, error) {
	counts := map[string]int{}
	err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT type, COUNT(*) FROM events WHERE tenant_id = $1 GROUP BY type`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			var c int
			if err := rows.Scan(&t, &c); err != nil {
				return err
			}
			counts[t] = c
		}
		return rows.Err()
	})
	return counts, err
}
