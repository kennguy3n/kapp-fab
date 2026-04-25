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
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// RegisterLMSTools attaches the Phase E LMS tools to an executor. A
// nil `store` is tolerated — commit-mode calls that need lesson
// progress persistence return a clear error rather than panicking.
func RegisterLMSTools(x *Executor, store *lms.Store) {
	x.Register(&recommendCourseTool{executor: x})
	x.Register(&gradeAssignmentTool{executor: x, store: store})
	x.Register(&submitAssignmentTool{executor: x})
}

// RegisterCertificateTool wires the Phase K issue_certificate tool.
// Kept separate from RegisterLMSTools so callers that don't have a
// CertificateIssuer (older test harnesses) keep working.
func RegisterCertificateTool(x *Executor, issuer *lms.CertificateIssuer) {
	x.Register(&issueCertificateTool{issuer: issuer})
}

// ----- lms.issue_certificate -----

type issueCertificateInput struct {
	EnrollmentID uuid.UUID `json:"enrollment_id"`
	TemplateID   string    `json:"template_id,omitempty"`
}

type issueCertificateTool struct {
	issuer *lms.CertificateIssuer
}

func (t *issueCertificateTool) Name() string               { return "lms.issue_certificate" }
func (t *issueCertificateTool) RequiresConfirmation() bool { return true }
func (t *issueCertificateTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in issueCertificateInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.EnrollmentID == uuid.Nil {
		return nil, errors.New("lms.issue_certificate: enrollment_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would issue certificate for enrollment %s", in.EnrollmentID),
			Preview: preview,
		}, nil
	}
	if t.issuer == nil {
		return nil, errors.New("lms.issue_certificate: issuer not configured")
	}
	cert, err := t.issuer.IssueCertificate(ctx, inv.TenantID, in.EnrollmentID, inv.ActorID, lms.CertificateOptions{TemplateID: in.TemplateID})
	if err != nil && !errors.Is(err, lms.ErrCertificateAlreadyIssued) {
		return nil, err
	}
	body, _ := json.Marshal(cert)
	summary := fmt.Sprintf("Issued certificate %s for enrollment %s", cert.ID, in.EnrollmentID)
	if errors.Is(err, lms.ErrCertificateAlreadyIssued) {
		summary = fmt.Sprintf("Certificate %s already issued for enrollment %s", cert.ID, in.EnrollmentID)
	}
	return &Result{
		Summary: summary,
		Preview: body,
		Extra:   map[string]any{"certificate_id": cert.ID, "enrollment_id": in.EnrollmentID},
	}, nil
}

// ----- lms.recommend_course -----

type recommendCourseInput struct {
	UserID uuid.UUID `json:"user_id,omitempty"`
	Role   string    `json:"role,omitempty"`
	TopN   int       `json:"top_n,omitempty"`
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
	if in.TopN <= 0 || in.TopN > 20 {
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

// ----- lms.submit_assignment -----

type submitAssignmentInput struct {
	AssignmentID uuid.UUID `json:"assignment_id"`
}

type submitAssignmentTool struct{ executor *Executor }

func (t *submitAssignmentTool) Name() string               { return "lms.submit_assignment" }
func (t *submitAssignmentTool) RequiresConfirmation() bool { return true }
func (t *submitAssignmentTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in submitAssignmentInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.AssignmentID == uuid.Nil {
		return nil, errors.New("lms.submit_assignment: assignment_id required")
	}
	if t.executor.records == nil {
		return nil, errors.New("lms.submit_assignment: record store not configured")
	}

	rec, err := t.executor.records.Get(ctx, inv.TenantID, in.AssignmentID)
	if err != nil {
		return nil, fmt.Errorf("lms.submit_assignment: load assignment: %w", err)
	}
	if rec.KType != lms.KTypeAssignment {
		return nil, fmt.Errorf("lms.submit_assignment: record %s is %s, not %s", rec.ID, rec.KType, lms.KTypeAssignment)
	}

	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		return nil, fmt.Errorf("lms.submit_assignment: decode data: %w", err)
	}
	// Enforce the lms.assignment.lifecycle workflow's state-machine at
	// the tool level. The workflow definition restricts
	// `submit_for_review` to `draft` or `returned`; applying the guard
	// here prevents a double-submission (which would create a second
	// approval chain + reviewer card) and prevents reverting an
	// already-`approved` terminal state back to `submitted`.
	currentStatus, _ := data["status"].(string)
	if currentStatus == "" {
		currentStatus = lms.AssignmentStatusDraft
	}
	if currentStatus != lms.AssignmentStatusDraft && currentStatus != lms.AssignmentStatusReturned {
		return nil, fmt.Errorf(
			"lms.submit_assignment: assignment %s is in status %q; only %q or %q assignments can be submitted",
			rec.ID, currentStatus, lms.AssignmentStatusDraft, lms.AssignmentStatusReturned,
		)
	}
	reviewerID, _ := data["reviewer_id"].(string)
	if reviewerID == "" {
		return nil, errors.New("lms.submit_assignment: reviewer_id must be set on the assignment before submission")
	}
	reviewerUUID, err := uuid.Parse(reviewerID)
	if err != nil {
		return nil, fmt.Errorf("lms.submit_assignment: invalid reviewer_id %q: %w", reviewerID, err)
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(map[string]any{
			"assignment_id": in.AssignmentID,
			"reviewer_id":   reviewerUUID,
		})
		return &Result{
			Summary: fmt.Sprintf("Would submit assignment %s for review by %s", in.AssignmentID, reviewerUUID),
			Preview: preview,
		}, nil
	}

	if t.executor.workflow == nil {
		return nil, errors.New("lms.submit_assignment: workflow engine not configured")
	}
	// Create the approval first. The record store's Update and the
	// workflow engine's RequestApproval run in independent tenant-
	// scoped transactions, so any cross-step failure leaves visible
	// state. Requesting approval first preserves the invariant that
	// whenever an assignment is in `status=submitted` there is a
	// corresponding approval row the reviewer can act on — the
	// alternative (patch first) can strand the assignment with no
	// approver if the approval insert fails.
	approval, err := t.executor.workflow.RequestApproval(
		ctx, inv.TenantID,
		lms.KTypeAssignment, rec.ID,
		workflow.ApprovalChain{
			Steps: []workflow.ApprovalStep{
				{Approvers: []uuid.UUID{reviewerUUID}, RequiredCount: 1},
			},
		},
		inv.ActorID,
	)
	if err != nil {
		return nil, fmt.Errorf("lms.submit_assignment: request approval: %w", err)
	}

	patchJSON, _ := json.Marshal(map[string]any{"status": lms.AssignmentStatusSubmitted})
	updated, err := t.executor.records.Update(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		ID:        rec.ID,
		Data:      patchJSON,
		UpdatedBy: &inv.ActorID,
	})
	if err != nil {
		return nil, fmt.Errorf("lms.submit_assignment: update status (approval %s already requested): %w", approval.ID, err)
	}

	body, _ := json.Marshal(map[string]any{
		"assignment": updated,
		"approval":   approval,
	})
	return &Result{
		Summary: fmt.Sprintf("Submitted assignment %s — approval %s sent to reviewer %s", rec.ID, approval.ID, reviewerUUID),
		Record:  updated,
		Preview: body,
		Extra:   map[string]any{"approval_id": approval.ID},
	}, nil
}
