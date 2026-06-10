package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/hr"
)

// RegisterRecruitmentTools wires the Session 16 recruitment agent tools
// onto an executor. A nil `store` is tolerated so kernel / integration
// tests that never apply the recruitment migration still register the
// tools — commit-mode calls then return a clear error rather than
// panicking, matching RegisterHRTools' contract.
//
// The four tools mirror the four headline domain actions:
//   - hr.create_job_opening  — draft a requisition
//   - hr.advance_application — move a candidate through the pipeline
//   - hr.schedule_interview  — book an interview round
//   - hr.recommend_candidates — read-only ranking of an opening's pipeline
//
// All mutating tools require confirmation and support dry-run preview per
// the ARCHITECTURE.md §11 agent-tool contract.
func RegisterRecruitmentTools(x *Executor, store *hr.RecruitmentStore) {
	x.Register(&createJobOpeningTool{store: store})
	x.Register(&advanceApplicationTool{store: store})
	x.Register(&scheduleInterviewTool{store: store})
	x.Register(&recommendCandidatesTool{store: store})
}

// ----- hr.create_job_opening -----

type createJobOpeningInput struct {
	Title          string `json:"title"`
	Department     string `json:"department,omitempty"`
	EmploymentType string `json:"employment_type,omitempty"`
	Location       string `json:"location,omitempty"`
	MaxPositions   int    `json:"max_positions,omitempty"`
}

type createJobOpeningTool struct {
	store *hr.RecruitmentStore
}

func (t *createJobOpeningTool) Name() string               { return "hr.create_job_opening" }
func (t *createJobOpeningTool) RequiresConfirmation() bool { return true }
func (t *createJobOpeningTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createJobOpeningInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Title == "" {
		return nil, errors.New("hr.create_job_opening: title required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would create job opening %q", in.Title),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("hr.create_job_opening: recruitment store not configured")
	}
	opening, err := t.store.CreateJobOpening(ctx, inv.TenantID, inv.ActorID, hr.CreateJobOpeningInput{
		Title:          in.Title,
		Department:     in.Department,
		EmploymentType: in.EmploymentType,
		Location:       in.Location,
		MaxPositions:   in.MaxPositions,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Created job opening %q (%s)", opening.Title, opening.Status),
		Extra: map[string]any{
			"job_opening_id": opening.ID,
			"status":         opening.Status,
		},
	}, nil
}

// ----- hr.advance_application -----

type advanceApplicationInput struct {
	ApplicationID uuid.UUID `json:"application_id"`
	Status        string    `json:"status"`
}

type advanceApplicationTool struct {
	store *hr.RecruitmentStore
}

func (t *advanceApplicationTool) Name() string               { return "hr.advance_application" }
func (t *advanceApplicationTool) RequiresConfirmation() bool { return true }
func (t *advanceApplicationTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in advanceApplicationInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ApplicationID == uuid.Nil {
		return nil, errors.New("hr.advance_application: application_id required")
	}
	if in.Status == "" {
		return nil, errors.New("hr.advance_application: status required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would advance application %s → %s", in.ApplicationID, in.Status),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("hr.advance_application: recruitment store not configured")
	}
	app, err := t.store.AdvanceApplication(ctx, inv.TenantID, in.ApplicationID, in.Status, inv.ActorID)
	if err != nil {
		return nil, err
	}
	extra := map[string]any{
		"job_application_id": app.ID,
		"status":             app.Status,
	}
	if app.HiredEmployeeID != nil {
		extra["hired_employee_id"] = *app.HiredEmployeeID
	}
	return &Result{
		Summary: fmt.Sprintf("Advanced application %s to %s", app.ID, app.Status),
		Extra:   extra,
	}, nil
}

// ----- hr.schedule_interview -----

type scheduleInterviewInput struct {
	ApplicationID   uuid.UUID  `json:"application_id"`
	InterviewerID   *uuid.UUID `json:"interviewer_id,omitempty"`
	InterviewType   string     `json:"interview_type,omitempty"`
	ScheduledAt     *time.Time `json:"scheduled_at,omitempty"`
	DurationMinutes int        `json:"duration_minutes,omitempty"`
	Location        string     `json:"location,omitempty"`
	MeetingLink     string     `json:"meeting_link,omitempty"`
}

type scheduleInterviewTool struct {
	store *hr.RecruitmentStore
}

func (t *scheduleInterviewTool) Name() string               { return "hr.schedule_interview" }
func (t *scheduleInterviewTool) RequiresConfirmation() bool { return true }
func (t *scheduleInterviewTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in scheduleInterviewInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ApplicationID == uuid.Nil {
		return nil, errors.New("hr.schedule_interview: application_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would schedule a %s interview for application %s",
				defaultInterviewType(in.InterviewType), in.ApplicationID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("hr.schedule_interview: recruitment store not configured")
	}
	iv, err := t.store.CreateInterview(ctx, inv.TenantID, inv.ActorID, hr.CreateInterviewInput{
		ApplicationID:   in.ApplicationID,
		InterviewerID:   in.InterviewerID,
		InterviewType:   in.InterviewType,
		ScheduledAt:     in.ScheduledAt,
		DurationMinutes: in.DurationMinutes,
		Location:        in.Location,
		MeetingLink:     in.MeetingLink,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Scheduled %s interview %s", iv.InterviewType, iv.ID),
		Extra: map[string]any{
			"interview_id":       iv.ID,
			"job_application_id": iv.ApplicationID,
		},
	}, nil
}

func defaultInterviewType(t string) string {
	if t == "" {
		return "video"
	}
	return t
}

// ----- hr.recommend_candidates -----

type recommendCandidatesInput struct {
	JobOpeningID uuid.UUID `json:"job_opening_id"`
	Limit        int       `json:"limit,omitempty"`
}

type candidateRecommendation struct {
	ApplicationID uuid.UUID `json:"application_id"`
	ApplicantName string    `json:"applicant_name"`
	Status        string    `json:"status"`
	Rating        *int      `json:"rating,omitempty"`
	Score         int       `json:"score"`
}

type recommendCandidatesTool struct {
	store *hr.RecruitmentStore
}

func (t *recommendCandidatesTool) Name() string               { return "hr.recommend_candidates" }
func (t *recommendCandidatesTool) RequiresConfirmation() bool { return false }
func (t *recommendCandidatesTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in recommendCandidatesInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.JobOpeningID == uuid.Nil {
		return nil, errors.New("hr.recommend_candidates: job_opening_id required")
	}
	if t.store == nil {
		return nil, errors.New("hr.recommend_candidates: recruitment store not configured")
	}
	apps, err := t.store.ListApplications(ctx, inv.TenantID, hr.ApplicationFilter{JobOpeningID: in.JobOpeningID})
	if err != nil {
		return nil, err
	}
	recs := rankCandidates(apps)
	limit := in.Limit
	if limit <= 0 || limit > len(recs) {
		limit = len(recs)
	}
	recs = recs[:limit]
	return &Result{
		Summary: fmt.Sprintf("Top %d candidate(s) for opening %s", len(recs), in.JobOpeningID),
		Extra:   map[string]any{"candidates": recs},
	}, nil
}

// rankCandidates orders the live pipeline best-first. Withdrawn and
// rejected applications are dropped; the remainder are scored by how far
// they have advanced (stage weight) plus their interview rating, so an
// HR user gets a defensible shortlist rather than raw insertion order.
func rankCandidates(apps []hr.JobApplication) []candidateRecommendation {
	recs := make([]candidateRecommendation, 0, len(apps))
	for i := range apps {
		a := &apps[i]
		if a.Status == hr.AppStatusRejected || a.Status == hr.AppStatusWithdrawn {
			continue
		}
		score := applicationStageWeight(a.Status) * 10
		if a.Rating != nil {
			score += *a.Rating
		}
		recs = append(recs, candidateRecommendation{
			ApplicationID: a.ID,
			ApplicantName: a.ApplicantName,
			Status:        a.Status,
			Rating:        a.Rating,
			Score:         score,
		})
	}
	sort.SliceStable(recs, func(i, j int) bool {
		return recs[i].Score > recs[j].Score
	})
	return recs
}

// applicationStageWeight maps a pipeline status to an ordinal so further-
// advanced candidates rank above fresh applicants. Terminal "good"
// states (offered, hired) weigh highest.
func applicationStageWeight(status string) int {
	switch status {
	case hr.AppStatusHired:
		return 6
	case hr.AppStatusOffered:
		return 5
	case hr.AppStatusInterview:
		return 4
	case hr.AppStatusShortlisted:
		return 3
	case hr.AppStatusScreening:
		return 2
	case hr.AppStatusApplied:
		return 1
	default:
		return 0
	}
}
