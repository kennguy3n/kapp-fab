//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
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
