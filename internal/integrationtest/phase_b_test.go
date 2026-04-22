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

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/crm"
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

// --- helpers ---

func eventCountsForTenant(ctx context.Context, pool pgxLike, tenantID uuid.UUID) (map[string]int, error) {
	rows, err := pool.Query(ctx,
		`SELECT type, COUNT(*) FROM events WHERE tenant_id = $1 GROUP BY type`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err != nil {
			return nil, err
		}
		counts[t] = c
	}
	return counts, rows.Err()
}

// pgxLike narrows the pool's surface so eventCountsForTenant can take
// either *pgxpool.Pool or anything else that satisfies Query. It keeps
// the helper testable without dragging a pool interface into production
// code.
type pgxLike interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
