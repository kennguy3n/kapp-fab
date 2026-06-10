// Session 16 — Recruitment store.
//
// RecruitmentStore is the typed persistence + state-machine layer for the
// four recruitment entities. It follows the manufacturing module shape:
// every mutation runs inside dbutil.WithTenantTx (so the app.tenant_id GUC
// is set before any RLS-protected table is touched) and emits both an
// audit entry (audit.Logger.LogTx) and an outbox event (events.Publisher
// .EmitTx) in the same transaction as the write, so the audit trail and
// notifications are durable iff the mutation commits.
//
// The application lifecycle is a Go state machine (AdvanceApplication),
// mirroring manufacturing work orders, rather than the KRecord workflow
// engine: the recruitment rows live in dedicated typed tables, not in
// krecords, so a hand-rolled transition table keeps the legal moves close
// to the data and the error surface a typed error instead of a 404 for a
// missing per-tenant workflow definition.

package hr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// Recruitment sentinel errors surfaced through the API / KChat bridge /
// agent tools. Handlers map these onto HTTP status codes.
var (
	ErrRecruitInvalidInput   = errors.New("recruitment: invalid input")
	ErrRecruitNotFound       = errors.New("recruitment: not found")
	ErrInvalidStatus         = errors.New("recruitment: unknown status")
	ErrInvalidTransition     = errors.New("recruitment: illegal status transition")
	ErrOpeningNotPublishable = errors.New("recruitment: job opening cannot be published from its current status")
	ErrOpeningFull           = errors.New("recruitment: job opening has no remaining positions")
	ErrOfferNotApproved      = errors.New("recruitment: offer letter is awaiting hiring-manager approval")
	ErrInterviewNotScheduled = errors.New("recruitment: interview is not in scheduled state")
)

// Application lifecycle states. Exported so handlers, agent tools and
// the KChat bridge validate against the same canonical set.
const (
	AppStatusApplied     = "applied"
	AppStatusScreening   = "screening"
	AppStatusShortlisted = "shortlisted"
	AppStatusInterview   = "interview"
	AppStatusOffered     = "offered"
	AppStatusHired       = "hired"
	AppStatusRejected    = "rejected"
	AppStatusWithdrawn   = "withdrawn"
)

// Job-opening lifecycle states.
const (
	OpeningStatusDraft  = "draft"
	OpeningStatusOpen   = "open"
	OpeningStatusOnHold = "on_hold"
	OpeningStatusClosed = "closed"
	OpeningStatusFilled = "filled"
)

// Offer-letter lifecycle states.
const (
	OfferStatusDraft     = "draft"
	OfferStatusSent      = "sent"
	OfferStatusAccepted  = "accepted"
	OfferStatusRejected  = "rejected"
	OfferStatusExpired   = "expired"
	OfferStatusWithdrawn = "withdrawn"
)

// Interview lifecycle states.
const (
	InterviewStatusScheduled = "scheduled"
	InterviewStatusCompleted = "completed"
	InterviewStatusCancelled = "cancelled"
	InterviewStatusNoShow    = "no_show"
)

// applicationTransitions is the legal-move table for the application
// lifecycle. The zero value (absent key) means "no transitions out", i.e.
// a terminal state. Re-applying the current status is treated as an
// idempotent no-op by AdvanceApplication and is therefore not listed.
var applicationTransitions = map[string][]string{
	AppStatusApplied:     {AppStatusScreening, AppStatusRejected, AppStatusWithdrawn},
	AppStatusScreening:   {AppStatusShortlisted, AppStatusRejected, AppStatusWithdrawn},
	AppStatusShortlisted: {AppStatusInterview, AppStatusRejected, AppStatusWithdrawn},
	AppStatusInterview:   {AppStatusOffered, AppStatusRejected, AppStatusWithdrawn},
	AppStatusOffered:     {AppStatusHired, AppStatusRejected, AppStatusWithdrawn},
	AppStatusHired:       {},
	AppStatusRejected:    {},
	AppStatusWithdrawn:   {},
}

// validApplicationStatus reports whether s is a known application status.
func validApplicationStatus(s string) bool {
	_, ok := applicationTransitions[s]
	return ok
}

// canAdvanceApplication reports whether from→to is a legal application
// transition. Equal states are reported as legal so callers can treat a
// repeated advance as an idempotent no-op.
func canAdvanceApplication(from, to string) bool {
	if from == to {
		return true
	}
	for _, allowed := range applicationTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// ----- domain structs -----

// JobOpening mirrors one row of job_openings.
type JobOpening struct {
	ID              uuid.UUID        `json:"id"`
	TenantID        uuid.UUID        `json:"tenant_id"`
	Title           string           `json:"title"`
	Department      string           `json:"department,omitempty"`
	Description     string           `json:"description,omitempty"`
	Requirements    string           `json:"requirements,omitempty"`
	EmploymentType  string           `json:"employment_type"`
	Location        string           `json:"location,omitempty"`
	SalaryRangeMin  *decimal.Decimal `json:"salary_range_min,omitempty"`
	SalaryRangeMax  *decimal.Decimal `json:"salary_range_max,omitempty"`
	Currency        string           `json:"currency"`
	Status          string           `json:"status"`
	HiringManagerID *uuid.UUID       `json:"hiring_manager_id,omitempty"`
	MaxPositions    int              `json:"max_positions"`
	PositionsFilled int              `json:"positions_filled"`
	PublishedAt     *time.Time       `json:"published_at,omitempty"`
	ClosesAt        *time.Time       `json:"closes_at,omitempty"`
	CreatedBy       *uuid.UUID       `json:"created_by,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// JobApplication mirrors one row of job_applications.
type JobApplication struct {
	ID                 uuid.UUID  `json:"id"`
	TenantID           uuid.UUID  `json:"tenant_id"`
	JobOpeningID       uuid.UUID  `json:"job_opening_id"`
	ApplicantName      string     `json:"applicant_name"`
	ApplicantEmail     string     `json:"applicant_email,omitempty"`
	Phone              string     `json:"phone,omitempty"`
	ResumeFileID       *uuid.UUID `json:"resume_file_id,omitempty"`
	CoverLetter        string     `json:"cover_letter,omitempty"`
	Source             string     `json:"source"`
	ReferrerEmployeeID *uuid.UUID `json:"referrer_employee_id,omitempty"`
	Status             string     `json:"status"`
	Rating             *int       `json:"rating,omitempty"`
	Notes              string     `json:"notes,omitempty"`
	HiredEmployeeID    *uuid.UUID `json:"hired_employee_id,omitempty"`
	AppliedAt          time.Time  `json:"applied_at"`
	CreatedBy          *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// Interview mirrors one row of interviews.
type Interview struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenant_id"`
	ApplicationID   uuid.UUID  `json:"application_id"`
	InterviewerID   *uuid.UUID `json:"interviewer_id,omitempty"`
	InterviewType   string     `json:"interview_type"`
	ScheduledAt     *time.Time `json:"scheduled_at,omitempty"`
	DurationMinutes int        `json:"duration_minutes"`
	Location        string     `json:"location,omitempty"`
	MeetingLink     string     `json:"meeting_link,omitempty"`
	Status          string     `json:"status"`
	Rating          *int       `json:"rating,omitempty"`
	Feedback        string     `json:"feedback,omitempty"`
	Recommendation  string     `json:"recommendation,omitempty"`
	CreatedBy       *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// OfferLetter mirrors one row of offer_letters.
type OfferLetter struct {
	ID                 uuid.UUID        `json:"id"`
	TenantID           uuid.UUID        `json:"tenant_id"`
	ApplicationID      uuid.UUID        `json:"application_id"`
	EmployeeTemplateID *uuid.UUID       `json:"employee_template_id,omitempty"`
	Designation        string           `json:"designation,omitempty"`
	Department         string           `json:"department,omitempty"`
	Salary             *decimal.Decimal `json:"salary,omitempty"`
	Currency           string           `json:"currency"`
	JoiningDate        *time.Time       `json:"joining_date,omitempty"`
	ProbationMonths    int              `json:"probation_months"`
	Benefits           json.RawMessage  `json:"benefits,omitempty"`
	Status             string           `json:"status"`
	ApprovalID         *uuid.UUID       `json:"approval_id,omitempty"`
	SentAt             *time.Time       `json:"sent_at,omitempty"`
	RespondedAt        *time.Time       `json:"responded_at,omitempty"`
	ValidUntil         *time.Time       `json:"valid_until,omitempty"`
	CreatedBy          *uuid.UUID       `json:"created_by,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// RecruitmentStore persists the recruitment entities and owns their
// lifecycle state machines. records / workflow may be nil for unit tests
// that do not exercise the hire-side-effects or offer-approval paths;
// the affected methods return a clear error in that case rather than
// panicking.
type RecruitmentStore struct {
	pool     *pgxpool.Pool
	auditor  audit.Logger
	events   events.Publisher
	records  *record.PGStore
	workflow *workflow.Engine
	now      func() time.Time
}

// NewRecruitmentStore wires a RecruitmentStore. auditor and events should
// be non-nil in production so every mutation is audited and notifiable;
// records enables the hired→employee auto-create and workflow enables the
// offer-letter approval + onboarding enrolment.
func NewRecruitmentStore(
	pool *pgxpool.Pool,
	auditor audit.Logger,
	pub events.Publisher,
	records *record.PGStore,
	wf *workflow.Engine,
) *RecruitmentStore {
	return &RecruitmentStore{
		pool:     pool,
		auditor:  auditor,
		events:   pub,
		records:  records,
		workflow: wf,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// emitTx writes an audit entry and an outbox event inside the caller's
// transaction. Either dependency may be nil (unit tests); a nil
// dependency is simply skipped. payload is the event body and after is
// the audit "after" snapshot — callers pass the post-mutation state.
func (s *RecruitmentStore) emitTx(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	actorID *uuid.UUID,
	action, targetKType string,
	targetID uuid.UUID,
	before, after any,
	eventType string,
	payload map[string]any,
) error {
	if s.events != nil && eventType != "" {
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("recruitment: marshal event: %w", err)
		}
		if err := s.events.EmitTx(ctx, tx, events.Event{
			TenantID: tenantID, Type: eventType, Payload: body,
		}); err != nil {
			return err
		}
	}
	if s.auditor != nil {
		var beforeJSON, afterJSON json.RawMessage
		if before != nil {
			b, err := json.Marshal(before)
			if err != nil {
				return fmt.Errorf("recruitment: marshal audit before: %w", err)
			}
			beforeJSON = b
		}
		if after != nil {
			a, err := json.Marshal(after)
			if err != nil {
				return fmt.Errorf("recruitment: marshal audit after: %w", err)
			}
			afterJSON = a
		}
		if err := s.auditor.LogTx(ctx, tx, audit.Entry{
			TenantID:    tenantID,
			ActorID:     actorID,
			ActorKind:   audit.ActorUser,
			Action:      action,
			TargetKType: targetKType,
			TargetID:    &targetID,
			Before:      beforeJSON,
			After:       afterJSON,
		}); err != nil {
			return err
		}
	}
	return nil
}

func actorPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// pgxScanner is satisfied by both pgx.Row (QueryRow) and the per-row
// Scan of a pgx.Rows iterator, so the scan helpers are shared between
// the Get and List paths and the column order is declared once.
type pgxScanner interface {
	Scan(dest ...any) error
}

// ============================ Job openings ============================

const jobOpeningColumns = `id, tenant_id, title, COALESCE(department, ''), COALESCE(description, ''),
	COALESCE(requirements, ''), employment_type, COALESCE(location, ''), salary_range_min,
	salary_range_max, currency, status, hiring_manager_id, max_positions, positions_filled,
	published_at, closes_at, created_by, created_at, updated_at`

func scanJobOpening(r pgxScanner, o *JobOpening) error {
	if err := r.Scan(
		&o.ID, &o.TenantID, &o.Title, &o.Department, &o.Description, &o.Requirements,
		&o.EmploymentType, &o.Location, &o.SalaryRangeMin, &o.SalaryRangeMax, &o.Currency,
		&o.Status, &o.HiringManagerID, &o.MaxPositions, &o.PositionsFilled,
		&o.PublishedAt, &o.ClosesAt, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRecruitNotFound
		}
		return fmt.Errorf("recruitment: scan job opening: %w", err)
	}
	return nil
}

// CreateJobOpeningInput is the canonical input for CreateJobOpening.
// Title is required; the rest carry sensible defaults.
type CreateJobOpeningInput struct {
	Title           string
	Department      string
	Description     string
	Requirements    string
	EmploymentType  string
	Location        string
	SalaryRangeMin  *decimal.Decimal
	SalaryRangeMax  *decimal.Decimal
	Currency        string
	HiringManagerID *uuid.UUID
	MaxPositions    int
	ClosesAt        *time.Time
}

var validEmploymentType = map[string]bool{
	"full_time": true, "part_time": true, "contract": true, "intern": true,
}

// CreateJobOpening inserts a new requisition in 'draft' status.
func (s *RecruitmentStore) CreateJobOpening(ctx context.Context, tenantID, actorID uuid.UUID, in CreateJobOpeningInput) (*JobOpening, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	if in.Title == "" {
		return nil, fmt.Errorf("%w: title required", ErrRecruitInvalidInput)
	}
	if in.EmploymentType == "" {
		in.EmploymentType = "full_time"
	}
	if !validEmploymentType[in.EmploymentType] {
		return nil, fmt.Errorf("%w: invalid employment_type %q", ErrRecruitInvalidInput, in.EmploymentType)
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.MaxPositions <= 0 {
		in.MaxPositions = 1
	}
	if in.SalaryRangeMin != nil && in.SalaryRangeMax != nil && in.SalaryRangeMax.LessThan(*in.SalaryRangeMin) {
		return nil, fmt.Errorf("%w: salary_range_max must be >= salary_range_min", ErrRecruitInvalidInput)
	}

	out := JobOpening{
		ID:              uuid.New(),
		TenantID:        tenantID,
		Title:           in.Title,
		Department:      in.Department,
		Description:     in.Description,
		Requirements:    in.Requirements,
		EmploymentType:  in.EmploymentType,
		Location:        in.Location,
		SalaryRangeMin:  in.SalaryRangeMin,
		SalaryRangeMax:  in.SalaryRangeMax,
		Currency:        in.Currency,
		Status:          OpeningStatusDraft,
		HiringManagerID: in.HiringManagerID,
		MaxPositions:    in.MaxPositions,
		PositionsFilled: 0,
		ClosesAt:        in.ClosesAt,
		CreatedBy:       actorPtr(actorID),
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO job_openings
			    (id, tenant_id, title, department, description, requirements, employment_type,
			     location, salary_range_min, salary_range_max, currency, status,
			     hiring_manager_id, max_positions, positions_filled, closes_at, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			 RETURNING created_at, updated_at`,
			out.ID, tenantID, out.Title, nullIfEmpty(out.Department), nullIfEmpty(out.Description),
			nullIfEmpty(out.Requirements), out.EmploymentType, nullIfEmpty(out.Location),
			out.SalaryRangeMin, out.SalaryRangeMax, out.Currency, out.Status,
			out.HiringManagerID, out.MaxPositions, out.PositionsFilled, out.ClosesAt, actorPtr(actorID),
		).Scan(&out.CreatedAt, &out.UpdatedAt); err != nil {
			return fmt.Errorf("recruitment: insert job opening: %w", err)
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.job_opening.create", KTypeJobOpening, out.ID, nil, out,
			"hr.job_opening.created", map[string]any{"job_opening_id": out.ID, "title": out.Title, "status": out.Status})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateJobOpeningInput carries the mutable fields of a job opening. The
// lifecycle status is not updatable here — it is driven by
// PublishJobOpening / CloseJobOpening / AdvanceApplication.
type UpdateJobOpeningInput struct {
	Title           string
	Department      string
	Description     string
	Requirements    string
	EmploymentType  string
	Location        string
	SalaryRangeMin  *decimal.Decimal
	SalaryRangeMax  *decimal.Decimal
	Currency        string
	HiringManagerID *uuid.UUID
	MaxPositions    int
	ClosesAt        *time.Time
}

// UpdateJobOpening replaces the mutable fields of an opening.
func (s *RecruitmentStore) UpdateJobOpening(ctx context.Context, tenantID, actorID, id uuid.UUID, in UpdateJobOpeningInput) (*JobOpening, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	if in.Title == "" {
		return nil, fmt.Errorf("%w: title required", ErrRecruitInvalidInput)
	}
	if in.EmploymentType == "" {
		in.EmploymentType = "full_time"
	}
	if !validEmploymentType[in.EmploymentType] {
		return nil, fmt.Errorf("%w: invalid employment_type %q", ErrRecruitInvalidInput, in.EmploymentType)
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.MaxPositions <= 0 {
		in.MaxPositions = 1
	}
	if in.SalaryRangeMin != nil && in.SalaryRangeMax != nil && in.SalaryRangeMax.LessThan(*in.SalaryRangeMin) {
		return nil, fmt.Errorf("%w: salary_range_max must be >= salary_range_min", ErrRecruitInvalidInput)
	}
	var out JobOpening
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var before JobOpening
		if err := scanJobOpening(tx.QueryRow(ctx,
			`SELECT `+jobOpeningColumns+` FROM job_openings WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id), &before); err != nil {
			return err
		}
		if before.MaxPositions != in.MaxPositions && in.MaxPositions < before.PositionsFilled {
			return fmt.Errorf("%w: max_positions %d below already-filled %d", ErrRecruitInvalidInput, in.MaxPositions, before.PositionsFilled)
		}
		if err := scanJobOpening(tx.QueryRow(ctx,
			`UPDATE job_openings SET
			    title = $3, department = $4, description = $5, requirements = $6,
			    employment_type = $7, location = $8, salary_range_min = $9, salary_range_max = $10,
			    currency = $11, hiring_manager_id = $12, max_positions = $13, closes_at = $14,
			    updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+jobOpeningColumns,
			tenantID, id, in.Title, nullIfEmpty(in.Department), nullIfEmpty(in.Description),
			nullIfEmpty(in.Requirements), in.EmploymentType, nullIfEmpty(in.Location),
			in.SalaryRangeMin, in.SalaryRangeMax, in.Currency, in.HiringManagerID,
			in.MaxPositions, in.ClosesAt), &out); err != nil {
			return err
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.job_opening.update", KTypeJobOpening, out.ID, before, out,
			"hr.job_opening.updated", map[string]any{"job_opening_id": out.ID, "title": out.Title})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetJobOpening returns one opening by id.
func (s *RecruitmentStore) GetJobOpening(ctx context.Context, tenantID, id uuid.UUID) (*JobOpening, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	var out JobOpening
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJobOpening(tx.QueryRow(ctx,
			`SELECT `+jobOpeningColumns+` FROM job_openings WHERE tenant_id = $1 AND id = $2`,
			tenantID, id), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// JobOpeningFilter narrows ListJobOpenings. Empty fields are ignored.
type JobOpeningFilter struct {
	Status     string
	Department string
}

// ListJobOpenings returns openings for a tenant, newest first, optionally
// filtered by status and/or department.
func (s *RecruitmentStore) ListJobOpenings(ctx context.Context, tenantID uuid.UUID, f JobOpeningFilter) ([]JobOpening, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	out := make([]JobOpening, 0, 16)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+jobOpeningColumns+`
			   FROM job_openings
			  WHERE tenant_id = $1
			    AND ($2 = '' OR status = $2)
			    AND ($3 = '' OR department = $3)
			  ORDER BY created_at DESC`,
			tenantID, f.Status, f.Department)
		if err != nil {
			return fmt.Errorf("recruitment: list job openings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var o JobOpening
			if err := scanJobOpening(rows, &o); err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PublishJobOpening transitions an opening to 'open' and stamps
// published_at. Legal from draft and on_hold; re-publishing an already
// open opening is an idempotent no-op.
func (s *RecruitmentStore) PublishJobOpening(ctx context.Context, tenantID, actorID, id uuid.UUID) (*JobOpening, error) {
	return s.setOpeningStatus(ctx, tenantID, actorID, id, OpeningStatusOpen)
}

// CloseJobOpening transitions an opening to 'closed'. Legal from any
// non-terminal status; re-closing is an idempotent no-op.
func (s *RecruitmentStore) CloseJobOpening(ctx context.Context, tenantID, actorID, id uuid.UUID) (*JobOpening, error) {
	return s.setOpeningStatus(ctx, tenantID, actorID, id, OpeningStatusClosed)
}

// openingTransitions is the legal-move table for the opening lifecycle.
var openingTransitions = map[string][]string{
	OpeningStatusDraft:  {OpeningStatusOpen, OpeningStatusClosed},
	OpeningStatusOpen:   {OpeningStatusOnHold, OpeningStatusClosed, OpeningStatusFilled},
	OpeningStatusOnHold: {OpeningStatusOpen, OpeningStatusClosed},
	OpeningStatusClosed: {},
	OpeningStatusFilled: {OpeningStatusClosed},
}

func canTransitionOpening(from, to string) bool {
	if from == to {
		return true
	}
	for _, a := range openingTransitions[from] {
		if a == to {
			return true
		}
	}
	return false
}

func (s *RecruitmentStore) setOpeningStatus(ctx context.Context, tenantID, actorID, id uuid.UUID, newStatus string) (*JobOpening, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	var out JobOpening
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var before JobOpening
		if err := scanJobOpening(tx.QueryRow(ctx,
			`SELECT `+jobOpeningColumns+` FROM job_openings WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id), &before); err != nil {
			return err
		}
		if before.Status == newStatus {
			out = before
			return nil // idempotent no-op
		}
		if !canTransitionOpening(before.Status, newStatus) {
			if newStatus == OpeningStatusOpen {
				return fmt.Errorf("%w: %s", ErrOpeningNotPublishable, before.Status)
			}
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, before.Status, newStatus)
		}
		// Stamp published_at the first time an opening goes open.
		setPublished := newStatus == OpeningStatusOpen && before.PublishedAt == nil
		if err := scanJobOpening(tx.QueryRow(ctx,
			`UPDATE job_openings SET status = $3,
			    published_at = CASE WHEN $4 THEN now() ELSE published_at END,
			    updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+jobOpeningColumns,
			tenantID, id, newStatus, setPublished), &out); err != nil {
			return err
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.job_opening."+newStatus, KTypeJobOpening, out.ID, before, out,
			"hr.job_opening."+newStatus, map[string]any{"job_opening_id": out.ID, "status": newStatus})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// =========================== Job applications ===========================

const jobApplicationColumns = `id, tenant_id, job_opening_id, applicant_name, COALESCE(applicant_email, ''),
	COALESCE(phone, ''), resume_file_id, COALESCE(cover_letter, ''), source, referrer_employee_id,
	status, rating, COALESCE(notes, ''), hired_employee_id, applied_at, created_by, created_at, updated_at`

func scanJobApplication(r pgxScanner, a *JobApplication) error {
	if err := r.Scan(
		&a.ID, &a.TenantID, &a.JobOpeningID, &a.ApplicantName, &a.ApplicantEmail,
		&a.Phone, &a.ResumeFileID, &a.CoverLetter, &a.Source, &a.ReferrerEmployeeID,
		&a.Status, &a.Rating, &a.Notes, &a.HiredEmployeeID, &a.AppliedAt,
		&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRecruitNotFound
		}
		return fmt.Errorf("recruitment: scan job application: %w", err)
	}
	return nil
}

// CreateApplicationInput is the canonical input for CreateApplication.
type CreateApplicationInput struct {
	JobOpeningID       uuid.UUID
	ApplicantName      string
	ApplicantEmail     string
	Phone              string
	ResumeFileID       *uuid.UUID
	CoverLetter        string
	Source             string
	ReferrerEmployeeID *uuid.UUID
	Rating             *int
	Notes              string
}

var validApplicationSource = map[string]bool{
	"website": true, "referral": true, "linkedin": true, "agency": true, "other": true,
}

func validRating(r *int) error {
	if r != nil && (*r < 1 || *r > 5) {
		return fmt.Errorf("%w: rating must be 1-5", ErrRecruitInvalidInput)
	}
	return nil
}

// CreateApplication inserts a new application against an opening in
// 'applied' status and emits the application_received notification.
func (s *RecruitmentStore) CreateApplication(ctx context.Context, tenantID, actorID uuid.UUID, in CreateApplicationInput) (*JobApplication, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	if in.JobOpeningID == uuid.Nil {
		return nil, fmt.Errorf("%w: job_opening_id required", ErrRecruitInvalidInput)
	}
	if in.ApplicantName == "" {
		return nil, fmt.Errorf("%w: applicant_name required", ErrRecruitInvalidInput)
	}
	if in.Source == "" {
		in.Source = "website"
	}
	if !validApplicationSource[in.Source] {
		return nil, fmt.Errorf("%w: invalid source %q", ErrRecruitInvalidInput, in.Source)
	}
	if err := validRating(in.Rating); err != nil {
		return nil, err
	}
	out := JobApplication{
		ID:                 uuid.New(),
		TenantID:           tenantID,
		JobOpeningID:       in.JobOpeningID,
		ApplicantName:      in.ApplicantName,
		ApplicantEmail:     in.ApplicantEmail,
		Phone:              in.Phone,
		ResumeFileID:       in.ResumeFileID,
		CoverLetter:        in.CoverLetter,
		Source:             in.Source,
		ReferrerEmployeeID: in.ReferrerEmployeeID,
		Status:             AppStatusApplied,
		Rating:             in.Rating,
		Notes:              in.Notes,
		CreatedBy:          actorPtr(actorID),
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var opening JobOpening
		if err := scanJobOpening(tx.QueryRow(ctx,
			`SELECT `+jobOpeningColumns+` FROM job_openings WHERE tenant_id = $1 AND id = $2`,
			tenantID, in.JobOpeningID), &opening); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO job_applications
			    (id, tenant_id, job_opening_id, applicant_name, applicant_email, phone, resume_file_id,
			     cover_letter, source, referrer_employee_id, status, rating, notes, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			 RETURNING applied_at, created_at, updated_at`,
			out.ID, tenantID, out.JobOpeningID, out.ApplicantName, nullIfEmpty(out.ApplicantEmail),
			nullIfEmpty(out.Phone), out.ResumeFileID, nullIfEmpty(out.CoverLetter), out.Source,
			out.ReferrerEmployeeID, out.Status, out.Rating, nullIfEmpty(out.Notes), actorPtr(actorID),
		).Scan(&out.AppliedAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
			return fmt.Errorf("recruitment: insert application: %w", err)
		}
		payload := map[string]any{
			"job_application_id": out.ID,
			"job_opening_id":     out.JobOpeningID,
			"applicant_name":     out.ApplicantName,
			"status":             out.Status,
		}
		if env := applicationReceivedEmail(out, opening); env != nil {
			payload["notification"] = env
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.job_application.create", KTypeJobApplication, out.ID, nil, out,
			"hr.job_application.created", payload)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateApplicationInput carries the editable, non-lifecycle fields of an
// application. Status changes flow exclusively through AdvanceApplication.
type UpdateApplicationInput struct {
	ApplicantName      string
	ApplicantEmail     string
	Phone              string
	ResumeFileID       *uuid.UUID
	CoverLetter        string
	Source             string
	ReferrerEmployeeID *uuid.UUID
	Rating             *int
	Notes              string
}

// UpdateApplication replaces the editable fields of an application.
func (s *RecruitmentStore) UpdateApplication(ctx context.Context, tenantID, actorID, id uuid.UUID, in UpdateApplicationInput) (*JobApplication, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	if in.ApplicantName == "" {
		return nil, fmt.Errorf("%w: applicant_name required", ErrRecruitInvalidInput)
	}
	if in.Source == "" {
		in.Source = "website"
	}
	if !validApplicationSource[in.Source] {
		return nil, fmt.Errorf("%w: invalid source %q", ErrRecruitInvalidInput, in.Source)
	}
	if err := validRating(in.Rating); err != nil {
		return nil, err
	}
	var out JobApplication
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var before JobApplication
		if err := scanJobApplication(tx.QueryRow(ctx,
			`SELECT `+jobApplicationColumns+` FROM job_applications WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id), &before); err != nil {
			return err
		}
		if err := scanJobApplication(tx.QueryRow(ctx,
			`UPDATE job_applications SET
			    applicant_name = $3, applicant_email = $4, phone = $5, resume_file_id = $6,
			    cover_letter = $7, source = $8, referrer_employee_id = $9, rating = $10, notes = $11,
			    updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+jobApplicationColumns,
			tenantID, id, in.ApplicantName, nullIfEmpty(in.ApplicantEmail), nullIfEmpty(in.Phone),
			in.ResumeFileID, nullIfEmpty(in.CoverLetter), in.Source, in.ReferrerEmployeeID,
			in.Rating, nullIfEmpty(in.Notes)), &out); err != nil {
			return err
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.job_application.update", KTypeJobApplication, out.ID, before, out,
			"hr.job_application.updated", map[string]any{"job_application_id": out.ID})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetApplication returns one application by id.
func (s *RecruitmentStore) GetApplication(ctx context.Context, tenantID, id uuid.UUID) (*JobApplication, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	var out JobApplication
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJobApplication(tx.QueryRow(ctx,
			`SELECT `+jobApplicationColumns+` FROM job_applications WHERE tenant_id = $1 AND id = $2`,
			tenantID, id), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ApplicationFilter narrows ListApplications. Empty fields are ignored.
type ApplicationFilter struct {
	JobOpeningID uuid.UUID
	Status       string
}

// ListApplications returns applications for a tenant, newest first,
// optionally filtered by opening and/or status.
func (s *RecruitmentStore) ListApplications(ctx context.Context, tenantID uuid.UUID, f ApplicationFilter) ([]JobApplication, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	out := make([]JobApplication, 0, 32)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+jobApplicationColumns+`
			   FROM job_applications
			  WHERE tenant_id = $1
			    AND ($2::uuid IS NULL OR job_opening_id = $2)
			    AND ($3 = '' OR status = $3)
			  ORDER BY applied_at DESC`,
			tenantID, nullableUUIDPtr(f.JobOpeningID), f.Status)
		if err != nil {
			return fmt.Errorf("recruitment: list applications: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a JobApplication
			if err := scanJobApplication(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AdvanceApplication moves an application to newStatus, validating the
// transition against applicationTransitions, emitting audit + an event,
// and — on reaching 'hired' — bumping the opening's positions_filled and
// auto-creating a draft hr.employee KRecord prefilled from the
// application / opening / offer. Re-applying the current status is an
// idempotent no-op (but still reconciles a missing hired employee).
func (s *RecruitmentStore) AdvanceApplication(ctx context.Context, tenantID, applicationID uuid.UUID, newStatus string, actorID uuid.UUID) (*JobApplication, error) {
	if tenantID == uuid.Nil || applicationID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and application id required", ErrRecruitInvalidInput)
	}
	if actorID == uuid.Nil {
		return nil, fmt.Errorf("%w: actor id required", ErrRecruitInvalidInput)
	}
	if !validApplicationStatus(newStatus) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, newStatus)
	}
	var out JobApplication
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var before JobApplication
		if err := scanJobApplication(tx.QueryRow(ctx,
			`SELECT `+jobApplicationColumns+` FROM job_applications WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, applicationID), &before); err != nil {
			return err
		}
		if before.Status == newStatus {
			out = before
			return nil
		}
		if !canAdvanceApplication(before.Status, newStatus) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, before.Status, newStatus)
		}
		if err := scanJobApplication(tx.QueryRow(ctx,
			`UPDATE job_applications SET status = $3, updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+jobApplicationColumns,
			tenantID, applicationID, newStatus), &out); err != nil {
			return err
		}
		// On hire, reserve a slot on the opening: lock the row, reject the
		// hire when there is no remaining capacity (ErrOpeningFull → 409),
		// otherwise bump positions_filled and flip the opening to 'filled'
		// when the last slot is taken. Done in the same tx so the count can
		// never drift from the set of hired applications.
		//
		// The capacity check is the enforcement point for max_positions:
		// without it an over-hire (e.g. a 5th hire against a 2-position
		// opening) would be silently swallowed by a LEAST() cap, leaving
		// positions_filled inconsistent with the set of 'hired' applications.
		// The FOR UPDATE serialises concurrent hires of different
		// applications for the same opening (each tx already holds its own
		// application-row lock first, so the lock order is stable), so the
		// second hire sees the committed count and is rejected when full.
		//
		// The auto-flip only fires when the opening is currently 'open':
		// open→filled is the only legal move into 'filled' per
		// openingTransitions, so a draft / on_hold / closed opening keeps
		// its status while still counting the hire.
		if newStatus == AppStatusHired {
			var posFilled, maxPos int
			if err := tx.QueryRow(ctx,
				`SELECT positions_filled, max_positions FROM job_openings
				  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
				tenantID, out.JobOpeningID).Scan(&posFilled, &maxPos); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrRecruitNotFound
				}
				return fmt.Errorf("recruitment: lock job opening: %w", err)
			}
			if posFilled >= maxPos {
				return fmt.Errorf("%w: %d/%d filled", ErrOpeningFull, posFilled, maxPos)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE job_openings
				    SET positions_filled = positions_filled + 1,
				        status = CASE WHEN positions_filled + 1 >= max_positions
				                      AND status = 'open' THEN 'filled' ELSE status END,
				        updated_at = now()
				  WHERE tenant_id = $1 AND id = $2`,
				tenantID, out.JobOpeningID); err != nil {
				return fmt.Errorf("recruitment: bump positions_filled: %w", err)
			}
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.job_application.advance", KTypeJobApplication, out.ID, before, out,
			"hr.job_application.advanced", map[string]any{
				"job_application_id": out.ID,
				"from":               before.Status,
				"to":                 newStatus,
			})
	})
	if err != nil {
		return nil, err
	}
	// Hire side effects run after the status commit. They are
	// idempotent (deterministic employee id + hired_employee_id guard)
	// so a partial failure here can be safely retried by re-advancing
	// to 'hired'.
	if out.Status == AppStatusHired && out.HiredEmployeeID == nil {
		if _, err := s.reconcileHiredEmployee(ctx, tenantID, actorID, out.ID); err != nil {
			return nil, err
		}
		// Re-read so the caller sees hired_employee_id populated.
		refreshed, gerr := s.GetApplication(ctx, tenantID, out.ID)
		if gerr == nil {
			out = *refreshed
		}
	}
	return &out, nil
}

// RejectApplication is sugar over AdvanceApplication(rejected) that also
// records a rejection reason in the application notes.
func (s *RecruitmentStore) RejectApplication(ctx context.Context, tenantID, actorID, id uuid.UUID, reason string) (*JobApplication, error) {
	app, err := s.AdvanceApplication(ctx, tenantID, id, AppStatusRejected, actorID)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		return app, nil
	}
	err = dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE job_applications
			    SET notes = CASE WHEN COALESCE(notes, '') = '' THEN $3
			                     ELSE notes || E'\n' || $3 END,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, "Rejected: "+reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetApplication(ctx, tenantID, id)
}

// =============================== Interviews ===============================

const interviewColumns = `id, tenant_id, application_id, interviewer_id, interview_type, scheduled_at,
	duration_minutes, COALESCE(location, ''), COALESCE(meeting_link, ''), status, rating,
	COALESCE(feedback, ''), COALESCE(recommendation, ''), created_by, created_at, updated_at`

func scanInterview(r pgxScanner, iv *Interview) error {
	if err := r.Scan(
		&iv.ID, &iv.TenantID, &iv.ApplicationID, &iv.InterviewerID, &iv.InterviewType,
		&iv.ScheduledAt, &iv.DurationMinutes, &iv.Location, &iv.MeetingLink, &iv.Status,
		&iv.Rating, &iv.Feedback, &iv.Recommendation, &iv.CreatedBy, &iv.CreatedAt, &iv.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRecruitNotFound
		}
		return fmt.Errorf("recruitment: scan interview: %w", err)
	}
	return nil
}

// CreateInterviewInput is the canonical input for CreateInterview.
type CreateInterviewInput struct {
	ApplicationID   uuid.UUID
	InterviewerID   *uuid.UUID
	InterviewType   string
	ScheduledAt     *time.Time
	DurationMinutes int
	Location        string
	MeetingLink     string
}

var validInterviewType = map[string]bool{
	"phone": true, "video": true, "in_person": true, "panel": true, "technical": true, "cultural": true,
}

// CreateInterview schedules an interview against an application and emits
// the interview_scheduled notification to the applicant.
func (s *RecruitmentStore) CreateInterview(ctx context.Context, tenantID, actorID uuid.UUID, in CreateInterviewInput) (*Interview, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	if in.ApplicationID == uuid.Nil {
		return nil, fmt.Errorf("%w: application_id required", ErrRecruitInvalidInput)
	}
	if in.InterviewType == "" {
		in.InterviewType = "video"
	}
	if !validInterviewType[in.InterviewType] {
		return nil, fmt.Errorf("%w: invalid interview_type %q", ErrRecruitInvalidInput, in.InterviewType)
	}
	if in.DurationMinutes <= 0 {
		in.DurationMinutes = 60
	}
	out := Interview{
		ID:              uuid.New(),
		TenantID:        tenantID,
		ApplicationID:   in.ApplicationID,
		InterviewerID:   in.InterviewerID,
		InterviewType:   in.InterviewType,
		ScheduledAt:     in.ScheduledAt,
		DurationMinutes: in.DurationMinutes,
		Location:        in.Location,
		MeetingLink:     in.MeetingLink,
		Status:          InterviewStatusScheduled,
		CreatedBy:       actorPtr(actorID),
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var app JobApplication
		if err := scanJobApplication(tx.QueryRow(ctx,
			`SELECT `+jobApplicationColumns+` FROM job_applications WHERE tenant_id = $1 AND id = $2`,
			tenantID, in.ApplicationID), &app); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO interviews
			    (id, tenant_id, application_id, interviewer_id, interview_type, scheduled_at,
			     duration_minutes, location, meeting_link, status, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			 RETURNING created_at, updated_at`,
			out.ID, tenantID, out.ApplicationID, out.InterviewerID, out.InterviewType, out.ScheduledAt,
			out.DurationMinutes, nullIfEmpty(out.Location), nullIfEmpty(out.MeetingLink), out.Status, actorPtr(actorID),
		).Scan(&out.CreatedAt, &out.UpdatedAt); err != nil {
			return fmt.Errorf("recruitment: insert interview: %w", err)
		}
		payload := map[string]any{
			"interview_id":       out.ID,
			"job_application_id": out.ApplicationID,
			"interview_type":     out.InterviewType,
			"status":             out.Status,
		}
		if env := interviewScheduledEmail(out, app); env != nil {
			payload["notification"] = env
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.interview.create", KTypeInterview, out.ID, nil, out,
			"hr.interview.scheduled", payload)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CompleteInterviewInput carries the outcome of a completed interview.
type CompleteInterviewInput struct {
	Rating         *int
	Feedback       string
	Recommendation string
}

var validRecommendation = map[string]bool{
	"strong_yes": true, "yes": true, "neutral": true, "no": true, "strong_no": true,
}

// CompleteInterview transitions a scheduled interview to 'completed' and
// records the rating, feedback and recommendation.
func (s *RecruitmentStore) CompleteInterview(ctx context.Context, tenantID, actorID, id uuid.UUID, in CompleteInterviewInput) (*Interview, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	if err := validRating(in.Rating); err != nil {
		return nil, err
	}
	if in.Recommendation != "" && !validRecommendation[in.Recommendation] {
		return nil, fmt.Errorf("%w: invalid recommendation %q", ErrRecruitInvalidInput, in.Recommendation)
	}
	var out Interview
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var before Interview
		if err := scanInterview(tx.QueryRow(ctx,
			`SELECT `+interviewColumns+` FROM interviews WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id), &before); err != nil {
			return err
		}
		if before.Status != InterviewStatusScheduled {
			return fmt.Errorf("%w: status is %s", ErrInterviewNotScheduled, before.Status)
		}
		if err := scanInterview(tx.QueryRow(ctx,
			`UPDATE interviews SET status = 'completed', rating = $3, feedback = $4,
			    recommendation = $5, updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+interviewColumns,
			tenantID, id, in.Rating, nullIfEmpty(in.Feedback), nullIfEmpty(in.Recommendation)), &out); err != nil {
			return err
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.interview.complete", KTypeInterview, out.ID, before, out,
			"hr.interview.completed", map[string]any{
				"interview_id":   out.ID,
				"recommendation": out.Recommendation,
			})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// InterviewFilter narrows ListInterviews. Empty fields are ignored.
type InterviewFilter struct {
	ApplicationID uuid.UUID
	Status        string
}

// ListInterviews returns interviews for a tenant, soonest-scheduled
// first, optionally filtered by application and/or status.
func (s *RecruitmentStore) ListInterviews(ctx context.Context, tenantID uuid.UUID, f InterviewFilter) ([]Interview, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	out := make([]Interview, 0, 32)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+interviewColumns+`
			   FROM interviews
			  WHERE tenant_id = $1
			    AND ($2::uuid IS NULL OR application_id = $2)
			    AND ($3 = '' OR status = $3)
			  ORDER BY scheduled_at ASC NULLS LAST, created_at DESC`,
			tenantID, nullableUUIDPtr(f.ApplicationID), f.Status)
		if err != nil {
			return fmt.Errorf("recruitment: list interviews: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var iv Interview
			if err := scanInterview(rows, &iv); err != nil {
				return err
			}
			out = append(out, iv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ============================== Offer letters ==============================

const offerLetterColumns = `id, tenant_id, application_id, employee_template_id, COALESCE(designation, ''),
	COALESCE(department, ''), salary, currency, joining_date, probation_months, benefits, status,
	approval_id, sent_at, responded_at, valid_until, created_by, created_at, updated_at`

func scanOfferLetter(r pgxScanner, o *OfferLetter) error {
	if err := r.Scan(
		&o.ID, &o.TenantID, &o.ApplicationID, &o.EmployeeTemplateID, &o.Designation,
		&o.Department, &o.Salary, &o.Currency, &o.JoiningDate, &o.ProbationMonths, &o.Benefits,
		&o.Status, &o.ApprovalID, &o.SentAt, &o.RespondedAt, &o.ValidUntil,
		&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRecruitNotFound
		}
		return fmt.Errorf("recruitment: scan offer letter: %w", err)
	}
	return nil
}

// CreateOfferLetterInput is the canonical input for CreateOfferLetter.
type CreateOfferLetterInput struct {
	ApplicationID      uuid.UUID
	EmployeeTemplateID *uuid.UUID
	Designation        string
	Department         string
	Salary             *decimal.Decimal
	Currency           string
	JoiningDate        *time.Time
	ProbationMonths    int
	Benefits           json.RawMessage
	ValidUntil         *time.Time
}

// CreateOfferLetter drafts an offer for an application. The offer starts
// in 'draft'; SendOfferLetter gates the draft→sent move behind
// hiring-manager approval.
func (s *RecruitmentStore) CreateOfferLetter(ctx context.Context, tenantID, actorID uuid.UUID, in CreateOfferLetterInput) (*OfferLetter, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	if in.ApplicationID == uuid.Nil {
		return nil, fmt.Errorf("%w: application_id required", ErrRecruitInvalidInput)
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.ProbationMonths < 0 {
		return nil, fmt.Errorf("%w: probation_months must be >= 0", ErrRecruitInvalidInput)
	}
	benefits := in.Benefits
	if len(benefits) == 0 {
		benefits = json.RawMessage(`{}`)
	} else if !json.Valid(benefits) {
		return nil, fmt.Errorf("%w: benefits must be valid JSON", ErrRecruitInvalidInput)
	}
	out := OfferLetter{
		ID:                 uuid.New(),
		TenantID:           tenantID,
		ApplicationID:      in.ApplicationID,
		EmployeeTemplateID: in.EmployeeTemplateID,
		Designation:        in.Designation,
		Department:         in.Department,
		Salary:             in.Salary,
		Currency:           in.Currency,
		JoiningDate:        in.JoiningDate,
		ProbationMonths:    in.ProbationMonths,
		Benefits:           benefits,
		Status:             OfferStatusDraft,
		ValidUntil:         in.ValidUntil,
		CreatedBy:          actorPtr(actorID),
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var app JobApplication
		if err := scanJobApplication(tx.QueryRow(ctx,
			`SELECT `+jobApplicationColumns+` FROM job_applications WHERE tenant_id = $1 AND id = $2`,
			tenantID, in.ApplicationID), &app); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO offer_letters
			    (id, tenant_id, application_id, employee_template_id, designation, department, salary,
			     currency, joining_date, probation_months, benefits, status, valid_until, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			 RETURNING created_at, updated_at`,
			out.ID, tenantID, out.ApplicationID, out.EmployeeTemplateID, nullIfEmpty(out.Designation),
			nullIfEmpty(out.Department), out.Salary, out.Currency, out.JoiningDate, out.ProbationMonths,
			out.Benefits, out.Status, out.ValidUntil, actorPtr(actorID),
		).Scan(&out.CreatedAt, &out.UpdatedAt); err != nil {
			return fmt.Errorf("recruitment: insert offer letter: %w", err)
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.offer_letter.create", KTypeOfferLetter, out.ID, nil, out,
			"hr.offer_letter.created", map[string]any{"offer_letter_id": out.ID, "application_id": out.ApplicationID})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetOfferLetter returns one offer by id.
func (s *RecruitmentStore) GetOfferLetter(ctx context.Context, tenantID, id uuid.UUID) (*OfferLetter, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	var out OfferLetter
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanOfferLetter(tx.QueryRow(ctx,
			`SELECT `+offerLetterColumns+` FROM offer_letters WHERE tenant_id = $1 AND id = $2`,
			tenantID, id), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListOfferLetters returns offers for a tenant, newest first, optionally
// filtered by application.
func (s *RecruitmentStore) ListOfferLetters(ctx context.Context, tenantID, applicationID uuid.UUID) ([]OfferLetter, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrRecruitInvalidInput)
	}
	out := make([]OfferLetter, 0, 16)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+offerLetterColumns+`
			   FROM offer_letters
			  WHERE tenant_id = $1 AND ($2::uuid IS NULL OR application_id = $2)
			  ORDER BY created_at DESC`,
			tenantID, nullableUUIDPtr(applicationID))
		if err != nil {
			return fmt.Errorf("recruitment: list offer letters: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var o OfferLetter
			if err := scanOfferLetter(rows, &o); err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SendOfferLetter begins dispatch of a draft offer. When the offer's
// opening has a hiring manager, an approval is opened (record_ktype
// hr.offer_letter) and the offer stays in 'draft' until the approval is
// granted — DispatchApprovedOffer (driven by the approval.granted event)
// then flips it to 'sent' and emails the applicant. When there is no
// hiring manager to approve, the offer is sent immediately.
//
// Returns the (possibly still-draft) offer and the opened approval (nil
// when none was required).
func (s *RecruitmentStore) SendOfferLetter(ctx context.Context, tenantID, actorID, id uuid.UUID) (*OfferLetter, *workflow.Approval, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	offer, err := s.GetOfferLetter(ctx, tenantID, id)
	if err != nil {
		return nil, nil, err
	}
	if offer.Status == OfferStatusSent {
		return offer, nil, nil // idempotent
	}
	if offer.Status != OfferStatusDraft {
		return nil, nil, fmt.Errorf("%w: cannot send offer in status %s", ErrInvalidTransition, offer.Status)
	}
	// Resolve the hiring manager via the application's opening.
	app, err := s.GetApplication(ctx, tenantID, offer.ApplicationID)
	if err != nil {
		return nil, nil, err
	}
	opening, err := s.GetJobOpening(ctx, tenantID, app.JobOpeningID)
	if err != nil {
		return nil, nil, err
	}
	if opening.HiringManagerID != nil && *opening.HiringManagerID != uuid.Nil && s.workflow != nil {
		approval, err := s.workflow.RequestApproval(ctx, tenantID, KTypeOfferLetter, offer.ID,
			workflow.ApprovalChain{Steps: []workflow.ApprovalStep{{Approvers: []uuid.UUID{*opening.HiringManagerID}}}},
			actorID)
		if err != nil {
			return nil, nil, err
		}
		// Record the gating approval; offer stays draft until granted.
		if err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE offer_letters SET approval_id = $3, updated_at = now()
				 WHERE tenant_id = $1 AND id = $2`,
				tenantID, id, approval.ID)
			return err
		}); err != nil {
			return nil, nil, err
		}
		offer.ApprovalID = &approval.ID
		return offer, approval, nil
	}
	// No hiring manager → dispatch immediately.
	sent, err := s.dispatchOffer(ctx, tenantID, actorID, id)
	if err != nil {
		return nil, nil, err
	}
	return sent, nil, nil
}

// DispatchApprovedOffer flips an approved offer from draft to sent and
// emails the applicant. It is the action driven by the approval.granted
// event for an hr.offer_letter record. Idempotent: an already-sent offer
// is returned unchanged. Returns ErrOfferNotApproved when the gating
// approval has not been granted.
func (s *RecruitmentStore) DispatchApprovedOffer(ctx context.Context, tenantID, actorID, id uuid.UUID) (*OfferLetter, error) {
	offer, err := s.GetOfferLetter(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if offer.Status == OfferStatusSent {
		return offer, nil
	}
	if offer.Status != OfferStatusDraft {
		return nil, fmt.Errorf("%w: cannot send offer in status %s", ErrInvalidTransition, offer.Status)
	}
	if offer.ApprovalID != nil && s.workflow != nil {
		approval, err := s.workflow.GetApproval(ctx, tenantID, *offer.ApprovalID)
		if err != nil {
			return nil, err
		}
		if approval.State != workflow.ApprovalStateApproved {
			return nil, ErrOfferNotApproved
		}
	}
	return s.dispatchOffer(ctx, tenantID, actorID, id)
}

// dispatchOffer performs the draft→sent write, stamps sent_at, and emits
// the offer_sent notification. Callers must have verified any required
// approval first.
func (s *RecruitmentStore) dispatchOffer(ctx context.Context, tenantID, actorID, id uuid.UUID) (*OfferLetter, error) {
	var out OfferLetter
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var before OfferLetter
		if err := scanOfferLetter(tx.QueryRow(ctx,
			`SELECT `+offerLetterColumns+` FROM offer_letters WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id), &before); err != nil {
			return err
		}
		if before.Status == OfferStatusSent {
			out = before
			return nil
		}
		if before.Status != OfferStatusDraft {
			return fmt.Errorf("%w: cannot send offer in status %s", ErrInvalidTransition, before.Status)
		}
		if err := scanOfferLetter(tx.QueryRow(ctx,
			`UPDATE offer_letters SET status = 'sent', sent_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+offerLetterColumns,
			tenantID, id), &out); err != nil {
			return err
		}
		var app JobApplication
		if err := scanJobApplication(tx.QueryRow(ctx,
			`SELECT `+jobApplicationColumns+` FROM job_applications WHERE tenant_id = $1 AND id = $2`,
			tenantID, out.ApplicationID), &app); err != nil {
			return err
		}
		payload := map[string]any{"offer_letter_id": out.ID, "application_id": out.ApplicationID, "status": out.Status}
		if env := offerSentEmail(out, app); env != nil {
			payload["notification"] = env
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.offer_letter.send", KTypeOfferLetter, out.ID, before, out,
			"hr.offer_letter.sent", payload)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RespondToOffer records the candidate's response to a sent offer
// (accepted | rejected). On acceptance the offer_accepted notification is
// emitted. The candidate is moved to 'hired' via AdvanceApplication
// separately by the caller (HR confirms the hire), keeping the offer and
// application lifecycles decoupled.
func (s *RecruitmentStore) RespondToOffer(ctx context.Context, tenantID, actorID, id uuid.UUID, response string) (*OfferLetter, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and id required", ErrRecruitInvalidInput)
	}
	switch response {
	case OfferStatusAccepted, OfferStatusRejected:
	default:
		return nil, fmt.Errorf("%w: response must be accepted or rejected", ErrRecruitInvalidInput)
	}
	var out OfferLetter
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var before OfferLetter
		if err := scanOfferLetter(tx.QueryRow(ctx,
			`SELECT `+offerLetterColumns+` FROM offer_letters WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id), &before); err != nil {
			return err
		}
		if before.Status == response {
			out = before
			return nil
		}
		if before.Status != OfferStatusSent {
			return fmt.Errorf("%w: cannot respond to offer in status %s", ErrInvalidTransition, before.Status)
		}
		if err := scanOfferLetter(tx.QueryRow(ctx,
			`UPDATE offer_letters SET status = $3, responded_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+offerLetterColumns,
			tenantID, id, response), &out); err != nil {
			return err
		}
		payload := map[string]any{"offer_letter_id": out.ID, "application_id": out.ApplicationID, "status": out.Status}
		eventType := "hr.offer_letter.rejected"
		if response == OfferStatusAccepted {
			eventType = "hr.offer_letter.accepted"
			var app JobApplication
			if err := scanJobApplication(tx.QueryRow(ctx,
				`SELECT `+jobApplicationColumns+` FROM job_applications WHERE tenant_id = $1 AND id = $2`,
				tenantID, out.ApplicationID), &app); err != nil {
				return err
			}
			if env := offerAcceptedEmail(out, app); env != nil {
				payload["notification"] = env
			}
		}
		return s.emitTx(ctx, tx, tenantID, actorPtr(actorID),
			"hr.offer_letter.respond", KTypeOfferLetter, out.ID, before, out, eventType, payload)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// =========================== hire side effects ===========================

// recruitmentEmployeeNamespace seeds the deterministic UUIDv5 for the
// auto-created employee KRecord so the hired→employee step is idempotent:
// re-running it for the same application resolves to the same employee id
// and the krecords (tenant_id, id) primary key turns a double-create into
// a 23505 we swallow.
var recruitmentEmployeeNamespace = uuid.MustParse("b6b3f5a0-2c1e-4f7a-9d83-7e2b1c0a4f55")

// reconcileHiredEmployee creates (idempotently) the draft hr.employee
// KRecord for a hired application and records its id on the application.
// When records is nil (unit tests without the krecords schema) it is a
// no-op. When an onboarding workflow is registered for the tenant, the
// new employee is enrolled into a run.
func (s *RecruitmentStore) reconcileHiredEmployee(ctx context.Context, tenantID, actorID, applicationID uuid.UUID) (uuid.UUID, error) {
	if s.records == nil {
		return uuid.Nil, nil
	}
	app, err := s.GetApplication(ctx, tenantID, applicationID)
	if err != nil {
		return uuid.Nil, err
	}
	if app.HiredEmployeeID != nil {
		return *app.HiredEmployeeID, nil
	}
	opening, err := s.GetJobOpening(ctx, tenantID, app.JobOpeningID)
	if err != nil {
		return uuid.Nil, err
	}
	// Pull designation / joining date for the employee prefill. The
	// accepted offer's terms take precedence — the employee record must
	// reflect what the candidate actually accepted, never a newer draft
	// or sent offer that was superseded. Only when no offer was accepted
	// do we fall back to a best-effort prefill from the most recent offer
	// carrying each field (offers are newest-first).
	var designation string
	var joiningDate *time.Time
	if offers, oerr := s.ListOfferLetters(ctx, tenantID, applicationID); oerr == nil {
		var accepted *OfferLetter
		for i := range offers {
			if offers[i].Status == OfferStatusAccepted {
				accepted = &offers[i]
				break
			}
		}
		if accepted != nil {
			designation = accepted.Designation
			joiningDate = accepted.JoiningDate
		} else {
			for i := range offers {
				o := &offers[i]
				if designation == "" && o.Designation != "" {
					designation = o.Designation
				}
				if joiningDate == nil && o.JoiningDate != nil {
					joiningDate = o.JoiningDate
				}
			}
		}
	}
	empData := map[string]any{"name": app.ApplicantName, "status": "active"}
	if app.ApplicantEmail != "" {
		empData["email"] = app.ApplicantEmail
	}
	if opening.Department != "" {
		empData["department"] = opening.Department
	}
	if designation != "" {
		empData["designation"] = designation
	}
	if joiningDate != nil {
		empData["date_of_joining"] = joiningDate.Format("2006-01-02")
	}
	dataJSON, err := json.Marshal(empData)
	if err != nil {
		return uuid.Nil, fmt.Errorf("recruitment: marshal employee data: %w", err)
	}
	empID := uuid.NewSHA1(recruitmentEmployeeNamespace, append([]byte(tenantID.String()+":"), applicationID[:]...))
	createdBy := actorID
	if createdBy == uuid.Nil {
		createdBy = empID // self-attribution fallback; actorID is required upstream
	}
	_, err = s.records.Create(ctx, record.KRecord{
		ID:        empID,
		TenantID:  tenantID,
		KType:     KTypeEmployee,
		Data:      dataJSON,
		CreatedBy: createdBy,
	})
	if err != nil && !isUniqueViolation(err) {
		return uuid.Nil, fmt.Errorf("recruitment: auto-create employee: %w", err)
	}
	// Stamp hired_employee_id (only if still unset) so the hire is
	// recorded even when the employee row already existed from a retry.
	if err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE job_applications SET hired_employee_id = $3, updated_at = now()
			 WHERE tenant_id = $1 AND id = $2 AND hired_employee_id IS NULL`,
			tenantID, applicationID, empID)
		return e
	}); err != nil {
		return uuid.Nil, err
	}
	// Best-effort onboarding enrolment when a definition exists.
	if s.workflow != nil {
		if _, derr := s.workflow.GetDefinition(ctx, tenantID, WorkflowOnboarding); derr == nil {
			if _, serr := s.workflow.StartRun(ctx, tenantID, WorkflowOnboarding, empID, "", actorID); serr != nil {
				return empID, fmt.Errorf("recruitment: start onboarding: %w", serr)
			}
		}
	}
	return empID, nil
}

// nullableUUIDPtr returns nil for the zero UUID so a query parameter
// becomes SQL NULL (used by the "($n::uuid IS NULL OR col = $n)" filter
// idiom), and the uuid otherwise.
func nullableUUIDPtr(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}
