package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// RegisterLMSTools attaches the Phase E LMS tools to an executor. A
// nil `store` is tolerated — commit-mode calls that need lesson
// progress persistence return a clear error rather than panicking.
func RegisterLMSTools(x *Executor, store *lms.Store) {
	x.Register(&recommendCourseTool{executor: x})
	x.Register(&gradeAssignmentTool{executor: x, store: store})
}

// ----- lms.recommend_course -----

type recommendCourseInput struct {
	UserID    uuid.UUID `json:"user_id,omitempty"`
	Role      string    `json:"role,omitempty"`
	TopN      int       `json:"top_n,omitempty"`
}

type recommendCourseTool struct{ executor *Executor }

func (t *recommendCourseTool) Name() string               { return "lms.recommend_course" }
func (t *recommendCourseTool) RequiresConfirmation() bool { return false }
func (t *recommendCourseTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in recommendCourseInput
	if len(inv.Inputs) > 0 {
		if err := json.Unmarshal(inv.Inputs, &in); err != nil {
			return nil, fmt.Errorf("lms.recommend_course: decode inputs: %w", err)
		}
	}
	if in.TopN == 0 || in.TopN > 20 {
		in.TopN = 5
	}
	if t.executor.records == nil {
		return nil, errors.New("lms.recommend_course: record store not configured")
	}
	recs, err := t.executor.records.List(ctx, inv.TenantID, record.ListFilter{
		KType: lms.KTypeCourse,
		Limit: in.TopN,
	})
	if err != nil {
		return nil, err
	}
	type courseSummary struct {
		ID    uuid.UUID       `json:"id"`
		Title string          `json:"title"`
		Data  json.RawMessage `json:"data"`
	}
	summaries := make([]courseSummary, 0, len(recs))
	for _, r := range recs {
		var body map[string]any
		_ = json.Unmarshal(r.Data, &body)
		title, _ := body["title"].(string)
		summaries = append(summaries, courseSummary{ID: r.ID, Title: title, Data: r.Data})
	}
	body, _ := json.Marshal(summaries)
	return &Result{
		Summary: fmt.Sprintf("Recommended %d courses", len(summaries)),
		Preview: body,
	}, nil
}

// ----- lms.grade_assignment -----

type gradeAssignmentInput struct {
	EnrollmentID uuid.UUID       `json:"enrollment_id"`
	LessonID     uuid.UUID       `json:"lesson_id"`
	Score        decimal.Decimal `json:"score"`
	Passed       *bool           `json:"passed,omitempty"`
}

type gradeAssignmentTool struct {
	executor *Executor
	store    *lms.Store
}

func (t *gradeAssignmentTool) Name() string               { return "lms.grade_assignment" }
func (t *gradeAssignmentTool) RequiresConfirmation() bool { return true }
func (t *gradeAssignmentTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in gradeAssignmentInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.EnrollmentID == uuid.Nil || in.LessonID == uuid.Nil {
		return nil, errors.New("lms.grade_assignment: enrollment_id and lesson_id required")
	}
	status := lms.ProgressCompleted
	if in.Passed != nil && !*in.Passed {
		status = lms.ProgressInProgress
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would grade lesson %s @ %s", in.LessonID, in.Score),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("lms.grade_assignment: lms store not configured")
	}
	now := time.Now().UTC()
	score := in.Score
	var completedAt *time.Time
	if status == lms.ProgressCompleted {
		completedAt = &now
	}
	p, err := t.store.UpsertProgress(ctx, lms.Progress{
		TenantID:     inv.TenantID,
		EnrollmentID: in.EnrollmentID,
		LessonID:     in.LessonID,
		Status:       status,
		Score:        &score,
		StartedAt:    &now,
		CompletedAt:  completedAt,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(p)
	return &Result{
		Summary: fmt.Sprintf("Graded lesson %s — status=%s score=%s", p.LessonID, p.Status, p.Score),
		Preview: body,
	}, nil
}
