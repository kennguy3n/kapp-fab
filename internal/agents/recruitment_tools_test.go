package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/hr"
)

// mkInvocation builds an Invocation with JSON-encoded inputs for the
// recruitment tool tests. The executor normally does this marshalling;
// the tool unit tests drive Invoke directly so they can exercise
// dry-run / validation branches without a database.
func mkInvocation(t *testing.T, mode Mode, inputs any) Invocation {
	t.Helper()
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatalf("marshal inputs: %v", err)
	}
	return Invocation{
		TenantID: uuid.New(),
		ActorID:  uuid.New(),
		Inputs:   raw,
		Mode:     mode,
	}
}

func TestRegisterRecruitmentToolsNilStore(t *testing.T) {
	t.Parallel()
	x := NewExecutor(nil, nil, nil, nil)
	// A nil store must not panic at registration time — kernel/integration
	// tests that never apply the recruitment migration still register.
	RegisterRecruitmentTools(x, nil)
	for _, name := range []string{
		"hr.create_job_opening",
		"hr.advance_application",
		"hr.schedule_interview",
		"hr.recommend_candidates",
	} {
		if _, ok := x.handlers[name]; !ok {
			t.Fatalf("tool %q not registered", name)
		}
	}
}

func TestCreateJobOpeningTool(t *testing.T) {
	t.Parallel()
	tool := &createJobOpeningTool{}

	if tool.Name() != "hr.create_job_opening" {
		t.Fatalf("Name() = %q", tool.Name())
	}
	if !tool.RequiresConfirmation() {
		t.Fatal("create_job_opening must require confirmation")
	}

	t.Run("missing title errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, createJobOpeningInput{}))
		if err == nil {
			t.Fatal("expected error for missing title")
		}
	})

	t.Run("dry-run returns preview without store", func(t *testing.T) {
		t.Parallel()
		res, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, createJobOpeningInput{Title: "Backend Engineer"}))
		if err != nil {
			t.Fatalf("Invoke dry-run: %v", err)
		}
		if len(res.Preview) == 0 {
			t.Fatal("dry-run must populate Preview")
		}
		if res.Summary == "" {
			t.Fatal("dry-run must populate Summary")
		}
	})

	t.Run("commit without store errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeCommit, createJobOpeningInput{Title: "Backend Engineer"}))
		if err == nil {
			t.Fatal("commit with nil store must error")
		}
	})
}

func TestAdvanceApplicationTool(t *testing.T) {
	t.Parallel()
	tool := &advanceApplicationTool{}

	if !tool.RequiresConfirmation() {
		t.Fatal("advance_application must require confirmation")
	}

	t.Run("missing application_id errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, advanceApplicationInput{Status: "screening"}))
		if err == nil {
			t.Fatal("expected error for missing application_id")
		}
	})

	t.Run("missing status errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, advanceApplicationInput{ApplicationID: uuid.New()}))
		if err == nil {
			t.Fatal("expected error for missing status")
		}
	})

	t.Run("dry-run returns preview", func(t *testing.T) {
		t.Parallel()
		res, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, advanceApplicationInput{ApplicationID: uuid.New(), Status: "screening"}))
		if err != nil {
			t.Fatalf("Invoke dry-run: %v", err)
		}
		if len(res.Preview) == 0 {
			t.Fatal("dry-run must populate Preview")
		}
	})

	t.Run("commit without store errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeCommit, advanceApplicationInput{ApplicationID: uuid.New(), Status: "screening"}))
		if err == nil {
			t.Fatal("commit with nil store must error")
		}
	})
}

func TestScheduleInterviewTool(t *testing.T) {
	t.Parallel()
	tool := &scheduleInterviewTool{}

	if !tool.RequiresConfirmation() {
		t.Fatal("schedule_interview must require confirmation")
	}

	t.Run("missing application_id errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, scheduleInterviewInput{}))
		if err == nil {
			t.Fatal("expected error for missing application_id")
		}
	})

	t.Run("dry-run returns preview", func(t *testing.T) {
		t.Parallel()
		res, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, scheduleInterviewInput{ApplicationID: uuid.New()}))
		if err != nil {
			t.Fatalf("Invoke dry-run: %v", err)
		}
		if len(res.Preview) == 0 {
			t.Fatal("dry-run must populate Preview")
		}
	})

	t.Run("commit without store errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeCommit, scheduleInterviewInput{ApplicationID: uuid.New()}))
		if err == nil {
			t.Fatal("commit with nil store must error")
		}
	})
}

func TestRecommendCandidatesTool(t *testing.T) {
	t.Parallel()
	tool := &recommendCandidatesTool{}

	if tool.RequiresConfirmation() {
		t.Fatal("recommend_candidates is read-only and must not require confirmation")
	}

	t.Run("missing job_opening_id errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, recommendCandidatesInput{}))
		if err == nil {
			t.Fatal("expected error for missing job_opening_id")
		}
	})

	t.Run("nil store errors after validation", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeCommit, recommendCandidatesInput{JobOpeningID: uuid.New()}))
		if err == nil {
			t.Fatal("nil store must error")
		}
	})
}

func TestDefaultInterviewType(t *testing.T) {
	t.Parallel()
	if got := defaultInterviewType(""); got != "video" {
		t.Fatalf("defaultInterviewType(\"\") = %q, want video", got)
	}
	if got := defaultInterviewType("phone"); got != "phone" {
		t.Fatalf("defaultInterviewType(phone) = %q, want phone", got)
	}
}

func TestApplicationStageWeight(t *testing.T) {
	t.Parallel()
	// The ordering is the load-bearing property: a more advanced stage
	// must outrank a less advanced one so the shortlist is defensible.
	order := []string{
		hr.AppStatusApplied,
		hr.AppStatusScreening,
		hr.AppStatusShortlisted,
		hr.AppStatusInterview,
		hr.AppStatusOffered,
		hr.AppStatusHired,
	}
	for i := 1; i < len(order); i++ {
		if applicationStageWeight(order[i]) <= applicationStageWeight(order[i-1]) {
			t.Fatalf("stage %q must outrank %q", order[i], order[i-1])
		}
	}
	if applicationStageWeight("nonsense") != 0 {
		t.Fatal("unknown status must weigh 0")
	}
}

func TestRankCandidates(t *testing.T) {
	t.Parallel()

	rating := func(n int) *int { return &n }
	interviewed := uuid.New()
	applied := uuid.New()
	rejected := uuid.New()
	withdrawn := uuid.New()
	shortlistedHighRating := uuid.New()

	apps := []hr.JobApplication{
		{ID: applied, ApplicantName: "Applied", Status: hr.AppStatusApplied},
		{ID: interviewed, ApplicantName: "Interviewed", Status: hr.AppStatusInterview, Rating: rating(3)},
		{ID: rejected, ApplicantName: "Rejected", Status: hr.AppStatusRejected, Rating: rating(5)},
		{ID: withdrawn, ApplicantName: "Withdrawn", Status: hr.AppStatusWithdrawn},
		{ID: shortlistedHighRating, ApplicantName: "Shortlisted", Status: hr.AppStatusShortlisted, Rating: rating(5)},
	}

	recs := rankCandidates(apps)

	// Rejected + withdrawn are dropped.
	if len(recs) != 3 {
		t.Fatalf("expected 3 ranked candidates, got %d", len(recs))
	}
	for _, r := range recs {
		if r.Status == hr.AppStatusRejected || r.Status == hr.AppStatusWithdrawn {
			t.Fatalf("terminal status %q must be dropped", r.Status)
		}
	}

	// Interview (4*10+3=43) outranks shortlisted (3*10+5=35) outranks applied (1*10=10).
	if recs[0].ApplicationID != interviewed {
		t.Fatalf("expected interviewed candidate first, got %s", recs[0].ApplicantName)
	}
	if recs[1].ApplicationID != shortlistedHighRating {
		t.Fatalf("expected shortlisted second, got %s", recs[1].ApplicantName)
	}
	if recs[2].ApplicationID != applied {
		t.Fatalf("expected applied last, got %s", recs[2].ApplicantName)
	}
	if recs[0].Score != 43 {
		t.Fatalf("interviewed score = %d, want 43", recs[0].Score)
	}
}
