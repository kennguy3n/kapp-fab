//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

func newTenantForHR(t *testing.T, h *harness) (*tenant.Tenant, *hr.Store) {
	t.Helper()
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phaseehr"), Name: "Phase E HR Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := hr.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register hr ktypes: %v", err)
	}
	return tn, hr.NewStore(h.pool)
}

func newTenantForLMS(t *testing.T, h *harness) (*tenant.Tenant, *lms.Store) {
	t.Helper()
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phaseelms"), Name: "Phase E LMS Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := lms.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register lms ktypes: %v", err)
	}
	return tn, lms.NewStore(h.pool)
}

// TestLeaveLedgerBalanceReflectsDeltas asserts that the leave_balances
// view (and Store.LeaveBalance) always equal SUM(delta_days) across
// accruals and deductions for a given (employee, leave_type) — the
// append-only invariant that lets approved leave requests safely
// update balances without racing.
func TestLeaveLedgerBalanceReflectsDeltas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store := newTenantForHR(t, h)

	employee := uuid.New()
	actor := uuid.New()
	requestID := uuid.New()

	// Annual accrual.
	if _, err := store.AppendLeaveLedger(ctx, hr.LeaveLedgerEntry{
		TenantID: tn.ID, EmployeeID: employee, LeaveType: "annual",
		DeltaDays: decimal.NewFromInt(20), CreatedBy: actor,
	}); err != nil {
		t.Fatalf("accrual: %v", err)
	}
	// Approved leave request deducts 3 days. source_id lets retries
	// collide with the partial unique index.
	if _, err := store.AppendLeaveLedger(ctx, hr.LeaveLedgerEntry{
		TenantID: tn.ID, EmployeeID: employee, LeaveType: "annual",
		DeltaDays: decimal.NewFromInt(-3), SourceKType: "hr.leave_request",
		SourceID: &requestID, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("deduction: %v", err)
	}

	bal, err := store.LeaveBalance(ctx, tn.ID, employee, "annual")
	if err != nil {
		t.Fatalf("leave balance: %v", err)
	}
	if !bal.Equal(decimal.NewFromInt(17)) {
		t.Fatalf("balance = %s; want 17 (20 - 3)", bal)
	}

	// Re-posting the same approved leave request must not double-
	// deduct: the partial unique index surfaces ErrDuplicateLeaveSource
	// which the worker treats as a no-op.
	_, err = store.AppendLeaveLedger(ctx, hr.LeaveLedgerEntry{
		TenantID: tn.ID, EmployeeID: employee, LeaveType: "annual",
		DeltaDays: decimal.NewFromInt(-3), SourceKType: "hr.leave_request",
		SourceID: &requestID, CreatedBy: actor,
	})
	if err != hr.ErrDuplicateLeaveSource {
		t.Fatalf("expected ErrDuplicateLeaveSource on retry; got %v", err)
	}
	bal2, err := store.LeaveBalance(ctx, tn.ID, employee, "annual")
	if err != nil {
		t.Fatalf("reload balance: %v", err)
	}
	if !bal2.Equal(bal) {
		t.Fatalf("balance drifted after duplicate post: before=%s after=%s", bal, bal2)
	}

	balances, err := store.ListBalances(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list balances: %v", err)
	}
	var found bool
	for _, b := range balances {
		if b.EmployeeID == employee && b.LeaveType == "annual" {
			found = true
			if !b.BalanceDays.Equal(decimal.NewFromInt(17)) {
				t.Fatalf("list balance = %s; want 17", b.BalanceDays)
			}
		}
	}
	if !found {
		t.Fatalf("employee balance not returned by ListBalances")
	}
}

// TestLessonProgressTracksScoreAndCompletion walks a learner through
// in_progress → completed on a single lesson and confirms the
// enrollment_progress view rolls up the completion counts correctly.
func TestLessonProgressTracksScoreAndCompletion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store := newTenantForLMS(t, h)

	enrollmentID := uuid.New()
	lessonA := uuid.New()
	lessonB := uuid.New()

	// Start lesson A (no score yet).
	if _, err := store.UpsertProgress(ctx, lms.Progress{
		TenantID: tn.ID, EnrollmentID: enrollmentID, LessonID: lessonA,
		Status: lms.ProgressInProgress,
	}); err != nil {
		t.Fatalf("start lesson a: %v", err)
	}

	// Complete lesson A with a quiz score. Attempts += 1 because score
	// is non-nil.
	score := decimal.NewFromInt(85)
	if _, err := store.UpsertProgress(ctx, lms.Progress{
		TenantID: tn.ID, EnrollmentID: enrollmentID, LessonID: lessonA,
		Status: lms.ProgressCompleted, Score: &score,
	}); err != nil {
		t.Fatalf("complete lesson a: %v", err)
	}

	// Lesson B still in progress.
	if _, err := store.UpsertProgress(ctx, lms.Progress{
		TenantID: tn.ID, EnrollmentID: enrollmentID, LessonID: lessonB,
		Status: lms.ProgressInProgress,
	}); err != nil {
		t.Fatalf("start lesson b: %v", err)
	}

	progress, err := store.ListProgress(ctx, tn.ID, enrollmentID)
	if err != nil {
		t.Fatalf("list progress: %v", err)
	}
	if len(progress) != 2 {
		t.Fatalf("progress rows = %d; want 2", len(progress))
	}
	byLesson := map[uuid.UUID]lms.Progress{}
	for _, p := range progress {
		byLesson[p.LessonID] = p
	}
	aProg := byLesson[lessonA]
	if aProg.Status != lms.ProgressCompleted {
		t.Fatalf("lesson A status = %q; want completed", aProg.Status)
	}
	if aProg.Score == nil || !aProg.Score.Equal(score) {
		t.Fatalf("lesson A score = %v; want 85", aProg.Score)
	}
	if aProg.Attempts != 1 {
		t.Fatalf("lesson A attempts = %d; want 1", aProg.Attempts)
	}

	summary, err := store.EnrollmentSummary(ctx, tn.ID, enrollmentID)
	if err != nil {
		t.Fatalf("enrollment summary: %v", err)
	}
	if summary.CompletedLessons != 1 || summary.TotalLessons != 2 {
		t.Fatalf("summary = %+v; want 1/2", summary)
	}
}

// newLeaveRequestExecutor builds an agent executor wired with the HR
// tools against a live record store, workflow engine, and leave-ledger
// store. Mirrors the production wiring in services/api/main.go so
// hr.approve_leave runs the same code path end-to-end.
func newLeaveRequestExecutor(h *harness, engine *workflow.Engine, hrStore *hr.Store) *agents.Executor {
	ex := agents.NewExecutor(h.records, engine, h.auditor)
	agents.RegisterHRTools(ex, hrStore)
	return ex
}

// TestLeaveRequestApprovalFlow exercises the Phase E leave-request
// acceptance criterion end-to-end: an employee KRecord is created, a
// leave_request KRecord submits through its workflow (draft →
// pending_approval), the HR approval agent tool finalizes the
// decision, and the leave_ledger records the deduction so the balance
// projection reflects the approved days. The test deliberately routes
// through the workflow engine for the "submit" step and the agent
// tool for the "approve" step because that mirrors the two entry
// points callers use in production (HTTP action + agent invoke).
func TestLeaveRequestApprovalFlow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, hrStore := newTenantForHR(t, h)

	engine := workflow.NewEngine(h.pool, h.publisher, h.auditor)
	if err := engine.RegisterWorkflow(ctx, workflow.WorkflowDef{
		TenantID: tn.ID,
		Name:     hr.WorkflowLeaveRequest,
		Version:  1,
		Definition: workflow.Definition{
			InitialState: "draft",
			States:       []string{"draft", "pending_approval", "approved", "rejected", "cancelled"},
			Transitions: []workflow.Transition{
				{From: []string{"draft"}, To: "pending_approval", Action: "submit"},
				{From: []string{"pending_approval"}, To: "approved", Action: "approve"},
				{From: []string{"pending_approval"}, To: "rejected", Action: "reject"},
			},
		},
	}); err != nil {
		t.Fatalf("register workflow: %v", err)
	}

	actor := uuid.New()

	// 1. Employee KRecord.
	employee, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     hr.KTypeEmployee,
		Data:      json.RawMessage(`{"name":"Ada Lovelace","email":"ada@example.com","status":"active"}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}

	// 2. hr.leave_request KRecord, referencing the employee. Status
	//    starts as `draft` — the workflow owns state from here.
	leaveBody, _ := json.Marshal(map[string]any{
		"employee_id": employee.ID.String(),
		"leave_type":  "annual",
		"from_date":   "2026-03-01",
		"to_date":     "2026-03-03",
		"days":        3,
		"reason":      "Personal",
		"status":      "draft",
	})
	leaveReq, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     hr.KTypeLeaveRequest,
		Data:      leaveBody,
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create leave request: %v", err)
	}

	// 3. Submit for approval via workflow action. draft → pending_approval.
	run, err := engine.StartRun(ctx, tn.ID, hr.WorkflowLeaveRequest, leaveReq.ID, "", actor)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run, err = engine.Transition(ctx, tn.ID, run.ID, "submit", actor)
	if err != nil {
		t.Fatalf("submit transition: %v", err)
	}
	if run.State != "pending_approval" {
		t.Fatalf("post-submit state = %q; want pending_approval", run.State)
	}

	// 4. Approve via the HR agent tool — same path `POST /api/v1/agents
	//    /tools/hr.approve_leave` routes through. This is what posts
	//    to the leave_ledger so the projection moves.
	executor := newLeaveRequestExecutor(h, engine, hrStore)
	inputs, _ := json.Marshal(map[string]any{
		"leave_request_id": leaveReq.ID,
		"approver_id":      actor,
		"days":             3,
	})
	result, err := executor.Invoke(ctx, agents.Invocation{
		TenantID:  tn.ID,
		ActorID:   actor,
		ToolName:  "hr.approve_leave",
		Inputs:    inputs,
		Mode:      agents.ModeCommit,
		Confirmed: true,
	})
	if err != nil {
		t.Fatalf("approve tool: %v", err)
	}
	if result == nil || result.Record == nil {
		t.Fatalf("approve tool returned no record: %+v", result)
	}

	// 5. Finalize the workflow run alongside the record state the tool
	//    just patched; keeps the workflow_runs history authoritative.
	run, err = engine.Transition(ctx, tn.ID, run.ID, "approve", actor)
	if err != nil {
		t.Fatalf("approve transition: %v", err)
	}
	if run.State != "approved" {
		t.Fatalf("post-approve state = %q; want approved", run.State)
	}

	// 6. The record must now carry status=approved (the tool patched it).
	reloaded, err := h.records.Get(ctx, tn.ID, leaveReq.ID)
	if err != nil {
		t.Fatalf("reload leave request: %v", err)
	}
	var patched struct {
		Status     string `json:"status"`
		ApproverID string `json:"approver_id"`
	}
	if err := json.Unmarshal(reloaded.Data, &patched); err != nil {
		t.Fatalf("decode leave request: %v", err)
	}
	if patched.Status != "approved" {
		t.Fatalf("leave request status = %q; want approved", patched.Status)
	}

	// 7. The leave_ledger now carries a -3-day deduction keyed by the
	//    leave request, and the balance projection reflects it.
	bal, err := hrStore.LeaveBalance(ctx, tn.ID, employee.ID, "annual")
	if err != nil {
		t.Fatalf("leave balance: %v", err)
	}
	if !bal.Equal(decimal.NewFromInt(-3)) {
		t.Fatalf("leave balance = %s; want -3", bal)
	}

	// 8. Replaying the tool is a no-op: the partial unique index on
	//    leave_ledger (tenant_id, source_ktype, source_id) collides and
	//    the tool swallows ErrDuplicateLeaveSource so the ledger can't
	//    double-deduct.
	if _, err := executor.Invoke(ctx, agents.Invocation{
		TenantID:  tn.ID,
		ActorID:   actor,
		ToolName:  "hr.approve_leave",
		Inputs:    inputs,
		Mode:      agents.ModeCommit,
		Confirmed: true,
	}); err != nil {
		t.Fatalf("approve tool replay: %v", err)
	}
	bal2, err := hrStore.LeaveBalance(ctx, tn.ID, employee.ID, "annual")
	if err != nil {
		t.Fatalf("reload balance: %v", err)
	}
	if !bal2.Equal(bal) {
		t.Fatalf("balance drifted on replay: before=%s after=%s", bal, bal2)
	}

	// 9. The audit log carries entries for the workflow transitions and
	//    agent tool invocations so the lifecycle is reviewable end-to-end.
	actions, err := auditActionsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("load audit actions: %v", err)
	}
	hasAgentCommit := false
	for _, a := range actions {
		if a == "agent.tool.commit" {
			hasAgentCommit = true
			break
		}
	}
	if !hasAgentCommit {
		t.Fatalf("expected agent.tool.commit in audit log; got %v", actions)
	}
}

// TestCourseEnrollmentProgress exercises the Phase E LMS acceptance
// criterion that a course enrollment tracks progress across modules
// and lessons. Course / module / lesson / enrollment are stored as
// KRecords; the dedicated lesson_progress table + enrollment_progress
// view live on the LMS store and are the projection the UI hydrates
// its progress bar from.
func TestCourseEnrollmentProgress(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, lmsStore := newTenantForLMS(t, h)
	actor := uuid.New()

	courseBody, _ := json.Marshal(map[string]any{
		"title":  "Platform Fundamentals",
		"status": "published",
	})
	course, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: lms.KTypeCourse,
		Data: courseBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create course: %v", err)
	}

	moduleBody, _ := json.Marshal(map[string]any{
		"course_id": course.ID.String(),
		"title":     "Week 1",
		"order":     1,
	})
	module, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: lms.KTypeModule,
		Data: moduleBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create module: %v", err)
	}

	lessons := make([]record.KRecord, 3)
	for i := range lessons {
		body, _ := json.Marshal(map[string]any{
			"module_id":    module.ID.String(),
			"title":        "Lesson",
			"content_type": "text",
			"order":        i + 1,
		})
		rec, err := h.records.Create(ctx, record.KRecord{
			TenantID: tn.ID, KType: lms.KTypeLesson,
			Data: body, CreatedBy: actor,
		})
		if err != nil {
			t.Fatalf("create lesson %d: %v", i, err)
		}
		lessons[i] = *rec
	}

	enrollBody, _ := json.Marshal(map[string]any{
		"user_id":     actor.String(),
		"course_id":   course.ID.String(),
		"enrolled_at": time.Now().UTC().Format(time.RFC3339),
		"status":      "in_progress",
	})
	enrollment, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: lms.KTypeEnrollment,
		Data: enrollBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}

	// Two of three lessons completed; one still in progress → 2/3.
	for i, lesson := range lessons {
		status := lms.ProgressCompleted
		if i == len(lessons)-1 {
			status = lms.ProgressInProgress
		}
		if _, err := lmsStore.UpsertProgress(ctx, lms.Progress{
			TenantID: tn.ID, EnrollmentID: enrollment.ID, LessonID: lesson.ID,
			Status: status,
		}); err != nil {
			t.Fatalf("upsert progress %d: %v", i, err)
		}
	}

	summary, err := lmsStore.EnrollmentSummary(ctx, tn.ID, enrollment.ID)
	if err != nil {
		t.Fatalf("enrollment summary: %v", err)
	}
	if summary.TotalLessons != 3 {
		t.Fatalf("total_lessons = %d; want 3", summary.TotalLessons)
	}
	if summary.CompletedLessons != 2 {
		t.Fatalf("completed_lessons = %d; want 2", summary.CompletedLessons)
	}
	// The 2/3 ratio is what the web UI renders as the course
	// completion bar; covering it here guards the view from
	// regressing into a stale projection.
	pct := float64(summary.CompletedLessons) / float64(summary.TotalLessons) * 100
	if pct < 66.0 || pct > 67.0 {
		t.Fatalf("completion percentage = %.2f; want ~66.67", pct)
	}
}

// TestQuizSubmissionScoring covers the Phase E "a quiz submission is
// scored and recorded" acceptance criterion. A quiz KRecord and the
// lesson it belongs to are created, then UpsertProgress is called
// with the score field to simulate a graded submission; the progress
// row must carry the score verbatim and the derived status.
func TestQuizSubmissionScoring(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, lmsStore := newTenantForLMS(t, h)
	actor := uuid.New()

	lessonBody, _ := json.Marshal(map[string]any{
		"module_id":    uuid.NewString(),
		"title":        "Quiz Lesson",
		"content_type": "quiz",
		"order":        1,
	})
	lesson, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: lms.KTypeLesson,
		Data: lessonBody, CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create lesson: %v", err)
	}

	quizBody, _ := json.Marshal(map[string]any{
		"lesson_id":      lesson.ID.String(),
		"title":          "Module 1 quiz",
		"pass_threshold": 0.7,
	})
	if _, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: lms.KTypeQuiz,
		Data: quizBody, CreatedBy: actor,
	}); err != nil {
		t.Fatalf("create quiz: %v", err)
	}

	enrollmentID := uuid.New()
	score := decimal.NewFromFloat(88.5)
	now := time.Now().UTC()
	progress, err := lmsStore.UpsertProgress(ctx, lms.Progress{
		TenantID: tn.ID, EnrollmentID: enrollmentID, LessonID: lesson.ID,
		Status: lms.ProgressCompleted, Score: &score,
		StartedAt: &now, CompletedAt: &now,
	})
	if err != nil {
		t.Fatalf("submit quiz score: %v", err)
	}
	if progress.Status != lms.ProgressCompleted {
		t.Fatalf("status = %q; want completed", progress.Status)
	}
	if progress.Score == nil || !progress.Score.Equal(score) {
		t.Fatalf("score = %v; want %s", progress.Score, score)
	}
	if progress.Attempts != 1 {
		t.Fatalf("attempts = %d; want 1 on first scored submission", progress.Attempts)
	}

	// A second scored submission (re-attempt) bumps attempts and keeps
	// the latest score — reflects the "best-of" UX we want for quizzes.
	better := decimal.NewFromInt(95)
	progress, err = lmsStore.UpsertProgress(ctx, lms.Progress{
		TenantID: tn.ID, EnrollmentID: enrollmentID, LessonID: lesson.ID,
		Status: lms.ProgressCompleted, Score: &better,
	})
	if err != nil {
		t.Fatalf("resubmit quiz score: %v", err)
	}
	if progress.Attempts != 2 {
		t.Fatalf("attempts after resubmission = %d; want 2", progress.Attempts)
	}
	if progress.Score == nil || !progress.Score.Equal(better) {
		t.Fatalf("latest score = %v; want %s", progress.Score, better)
	}
}
