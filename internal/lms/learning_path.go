package lms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Learning-path KType identifiers. These mirror the typed tables
// added in migrations/000083_lms_deep.sql; the typed tables are the
// source of truth (relational joins for sequencing / completion /
// auto-enrollment), and the KType mirror exposes the same shape over
// the metadata-driven UI, agent, and KChat surfaces — the same split
// finance.budget uses.
const (
	KTypeLearningPath           = "lms.learning_path"
	KTypeLearningPathCourse     = "lms.learning_path_course"
	KTypeLearningPathEnrollment = "lms.learning_path_enrollment"
)

// Learning-path lifecycle states.
const (
	PathStatusDraft     = "draft"
	PathStatusPublished = "published"
	PathStatusArchived  = "archived"
)

// Learning-path enrollment states.
const (
	PathEnrollmentEnrolled   = "enrolled"
	PathEnrollmentInProgress = "in_progress"
	PathEnrollmentCompleted  = "completed"
)

// Enrollment sources — manual (explicit) vs. auto (role-driven).
const (
	EnrollSourceManual = "manual"
	EnrollSourceAuto   = "auto"
)

// Difficulty levels.
const (
	DifficultyBeginner     = "beginner"
	DifficultyIntermediate = "intermediate"
	DifficultyAdvanced     = "advanced"
)

// Sentinel errors surfaced through the HTTP / agent layers so callers
// can map them to 400 / 404.
var (
	ErrInvalidLearningPath    = errors.New("lms: invalid learning path")
	ErrLearningPathNotFound   = errors.New("lms: learning path not found")
	ErrPathEnrollmentNotFound = errors.New("lms: learning path enrollment not found")
)

// LearningPath is a learning_paths row — an ordered curriculum of
// courses targeted at one or more roles.
type LearningPath struct {
	TenantID               uuid.UUID  `json:"tenant_id"`
	ID                     uuid.UUID  `json:"id"`
	Title                  string     `json:"title"`
	Description            string     `json:"description"`
	Status                 string     `json:"status"`
	TargetRoles            []string   `json:"target_roles"`
	EstimatedDurationHours int        `json:"estimated_duration_hours"`
	Difficulty             string     `json:"difficulty"`
	CreatedBy              *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// LearningPathCourse pins a course into a path at a sequence position.
type LearningPathCourse struct {
	TenantID              uuid.UUID   `json:"tenant_id"`
	ID                    uuid.UUID   `json:"id"`
	LearningPathID        uuid.UUID   `json:"learning_path_id"`
	CourseID              uuid.UUID   `json:"course_id"`
	SequenceOrder         int         `json:"sequence_order"`
	IsMandatory           bool        `json:"is_mandatory"`
	PrerequisiteCourseIDs []uuid.UUID `json:"prerequisite_course_ids"`
	CreatedAt             time.Time   `json:"created_at"`
}

// LearningPathEnrollment is one (path, user) enrollment row.
type LearningPathEnrollment struct {
	TenantID       uuid.UUID  `json:"tenant_id"`
	ID             uuid.UUID  `json:"id"`
	LearningPathID uuid.UUID  `json:"learning_path_id"`
	UserID         uuid.UUID  `json:"user_id"`
	Status         string     `json:"status"`
	Source         string     `json:"source"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// CompletionResult is the rollup of a learner's progress through a
// path. Complete is true when every mandatory course is done.
type CompletionResult struct {
	TotalCourses       int  `json:"total_courses"`
	MandatoryCourses   int  `json:"mandatory_courses"`
	CompletedMandatory int  `json:"completed_mandatory"`
	CompletedTotal     int  `json:"completed_total"`
	Complete           bool `json:"complete"`
}

// EvaluateCompletion computes the completion rollup for a path given
// the set of course IDs the learner has completed. The rule: a path is
// complete when all MANDATORY courses are complete; non-mandatory
// courses are counted in CompletedTotal but never block completion.
//
// Pure function (no I/O) so the gating rule is unit-testable in
// isolation from the record store. An empty path (no courses, or no
// mandatory courses) is treated as NOT complete — there is nothing to
// have accomplished — which prevents a freshly-created empty path from
// auto-completing every enrollee.
func EvaluateCompletion(courses []LearningPathCourse, completed map[uuid.UUID]bool) CompletionResult {
	res := CompletionResult{TotalCourses: len(courses)}
	for i := range courses {
		c := &courses[i]
		done := completed[c.CourseID]
		if done {
			res.CompletedTotal++
		}
		if c.IsMandatory {
			res.MandatoryCourses++
			if done {
				res.CompletedMandatory++
			}
		}
	}
	res.Complete = res.MandatoryCourses > 0 && res.CompletedMandatory == res.MandatoryCourses
	return res
}

// UnlockedCourseIDs returns the subset of a path's courses whose
// prerequisites are all satisfied by `completed`. A course with no
// prerequisites is always unlocked. Used by the learner UI to gate
// access to a course until its prerequisites are done, and exposed as
// pure logic so the gating is testable without a database.
func UnlockedCourseIDs(courses []LearningPathCourse, completed map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(courses))
	for i := range courses {
		c := &courses[i]
		unlocked := true
		for _, pre := range c.PrerequisiteCourseIDs {
			if !completed[pre] {
				unlocked = false
				break
			}
		}
		if unlocked {
			out = append(out, c.CourseID)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Store.
// ---------------------------------------------------------------------------

// LearningPathStore is the Postgres-backed persistence for learning
// paths, their course members, and enrollments. All reads/writes route
// through platform.WithTenantTx so RLS enforces tenant isolation, and
// every mutation appends an audit entry inside the same transaction so
// the audit trail is durable iff the mutation commits.
type LearningPathStore struct {
	pool    *pgxpool.Pool
	auditor audit.Logger
	now     func() time.Time
}

// NewLearningPathStore wires a store against the shared pool. A nil
// auditor is tolerated so unit tests can exercise the store without an
// audit sink.
func NewLearningPathStore(pool *pgxpool.Pool, auditor audit.Logger) *LearningPathStore {
	return &LearningPathStore{
		pool:    pool,
		auditor: auditor,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithClock substitutes the store's time source (tests).
func (s *LearningPathStore) WithClock(now func() time.Time) *LearningPathStore {
	if now != nil {
		s.now = now
	}
	return s
}

// validatePath applies the same invariants the DB CHECK constraints
// enforce, but with friendly sentinel-wrapped errors so the HTTP layer
// returns 400 instead of a raw 23514.
func validatePath(in *LearningPath) error {
	if in.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id required", ErrInvalidLearningPath)
	}
	if strings.TrimSpace(in.Title) == "" {
		return fmt.Errorf("%w: title required", ErrInvalidLearningPath)
	}
	if in.Status == "" {
		in.Status = PathStatusDraft
	}
	switch in.Status {
	case PathStatusDraft, PathStatusPublished, PathStatusArchived:
	default:
		return fmt.Errorf("%w: status %q not one of draft/published/archived", ErrInvalidLearningPath, in.Status)
	}
	if in.Difficulty == "" {
		in.Difficulty = DifficultyBeginner
	}
	switch in.Difficulty {
	case DifficultyBeginner, DifficultyIntermediate, DifficultyAdvanced:
	default:
		return fmt.Errorf("%w: difficulty %q invalid", ErrInvalidLearningPath, in.Difficulty)
	}
	if in.EstimatedDurationHours < 0 {
		return fmt.Errorf("%w: estimated_duration_hours must be >= 0", ErrInvalidLearningPath)
	}
	// Normalize roles: trim, drop empties, dedupe — so containment
	// matching in the auto-enroller is exact.
	in.TargetRoles = normalizeRoles(in.TargetRoles)
	return nil
}

// normalizeRoles trims, drops empty entries, and dedupes a role slice
// while preserving first-seen order.
func normalizeRoles(roles []string) []string {
	seen := make(map[string]struct{}, len(roles))
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}

// CreatePath inserts a learning path header. ID is generated when zero.
func (s *LearningPathStore) CreatePath(ctx context.Context, in LearningPath) (*LearningPath, error) {
	if err := validatePath(&in); err != nil {
		return nil, err
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	var out LearningPath
	err := platform.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO learning_paths
			    (tenant_id, id, title, description, status, target_roles,
			     estimated_duration_hours, difficulty, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 RETURNING tenant_id, id, title, description, status, target_roles,
			           estimated_duration_hours, difficulty, created_by,
			           created_at, updated_at`,
			in.TenantID, in.ID, in.Title, in.Description, in.Status, in.TargetRoles,
			in.EstimatedDurationHours, in.Difficulty, in.CreatedBy,
		)
		if err := scanPath(row, &out); err != nil {
			return err
		}
		return s.audit(ctx, tx, in.TenantID, in.CreatedBy, "lms.learning_path.create", out.ID, nil, mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdatePath updates a learning path header in place.
func (s *LearningPathStore) UpdatePath(ctx context.Context, in LearningPath, actor *uuid.UUID) (*LearningPath, error) {
	if in.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: id required for update", ErrInvalidLearningPath)
	}
	if err := validatePath(&in); err != nil {
		return nil, err
	}
	var out LearningPath
	err := platform.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		before, err := getPathTx(ctx, tx, in.TenantID, in.ID)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx,
			`UPDATE learning_paths
			    SET title = $3, description = $4, status = $5, target_roles = $6,
			        estimated_duration_hours = $7, difficulty = $8, updated_at = $9
			  WHERE tenant_id = $1 AND id = $2
			 RETURNING tenant_id, id, title, description, status, target_roles,
			           estimated_duration_hours, difficulty, created_by,
			           created_at, updated_at`,
			in.TenantID, in.ID, in.Title, in.Description, in.Status, in.TargetRoles,
			in.EstimatedDurationHours, in.Difficulty, s.now(),
		)
		if err := scanPath(row, &out); err != nil {
			return err
		}
		return s.audit(ctx, tx, in.TenantID, actor, "lms.learning_path.update", out.ID, mustJSON(before), mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPath returns a single learning path by id.
func (s *LearningPathStore) GetPath(ctx context.Context, tenantID, id uuid.UUID) (*LearningPath, error) {
	var out *LearningPath
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p, err := getPathTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPaths returns every learning path for a tenant, newest first.
// When statusFilter is non-empty only paths in that status are returned
// (e.g. "published" for the learner catalog).
func (s *LearningPathStore) ListPaths(ctx context.Context, tenantID uuid.UUID, statusFilter string) ([]LearningPath, error) {
	// Non-nil so an empty result marshals as [] (not null) at the handler.
	out := []LearningPath{}
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, title, description, status, target_roles,
			        estimated_duration_hours, difficulty, created_by,
			        created_at, updated_at
			   FROM learning_paths
			  WHERE tenant_id = $1
			    AND ($2 = '' OR status = $2)
			  ORDER BY created_at DESC, id`,
			tenantID, statusFilter,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p LearningPath
			if err := scanPath(rows, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// DeletePath removes a learning path (cascading to its courses and
// enrollments via the composite FKs).
func (s *LearningPathStore) DeletePath(ctx context.Context, tenantID, id uuid.UUID, actor *uuid.UUID) error {
	return platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		before, err := getPathTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM learning_paths WHERE tenant_id = $1 AND id = $2`, tenantID, id); err != nil {
			return fmt.Errorf("delete learning_path: %w", err)
		}
		return s.audit(ctx, tx, tenantID, actor, "lms.learning_path.delete", id, mustJSON(before), nil)
	})
}

// AddCourse pins a course into a path. Re-adding the same course is a
// no-op on conflict (the UNIQUE(tenant, path, course) constraint),
// returning the existing membership.
func (s *LearningPathStore) AddCourse(ctx context.Context, in LearningPathCourse, actor *uuid.UUID) (*LearningPathCourse, error) {
	if in.TenantID == uuid.Nil || in.LearningPathID == uuid.Nil || in.CourseID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id, learning_path_id, course_id required", ErrInvalidLearningPath)
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	if in.PrerequisiteCourseIDs == nil {
		in.PrerequisiteCourseIDs = []uuid.UUID{}
	}
	var out LearningPathCourse
	err := platform.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO learning_path_courses
			    (tenant_id, id, learning_path_id, course_id, sequence_order,
			     is_mandatory, prerequisite_course_ids)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 ON CONFLICT (tenant_id, learning_path_id, course_id) DO UPDATE
			    SET sequence_order = EXCLUDED.sequence_order,
			        is_mandatory = EXCLUDED.is_mandatory,
			        prerequisite_course_ids = EXCLUDED.prerequisite_course_ids
			 RETURNING tenant_id, id, learning_path_id, course_id, sequence_order,
			           is_mandatory, prerequisite_course_ids, created_at`,
			in.TenantID, in.ID, in.LearningPathID, in.CourseID, in.SequenceOrder,
			in.IsMandatory, in.PrerequisiteCourseIDs,
		)
		if err := scanPathCourse(row, &out); err != nil {
			return err
		}
		return s.audit(ctx, tx, in.TenantID, actor, "lms.learning_path_course.add", out.ID, nil, mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveCourse drops a course from a path.
func (s *LearningPathStore) RemoveCourse(ctx context.Context, tenantID, pathID, courseID uuid.UUID, actor *uuid.UUID) error {
	return platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM learning_path_courses
			  WHERE tenant_id = $1 AND learning_path_id = $2 AND course_id = $3`,
			tenantID, pathID, courseID)
		if err != nil {
			return fmt.Errorf("remove learning_path_course: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return s.audit(ctx, tx, tenantID, actor, "lms.learning_path_course.remove", courseID, nil, nil)
	})
}

// ListCourses returns a path's courses in sequence order.
func (s *LearningPathStore) ListCourses(ctx context.Context, tenantID, pathID uuid.UUID) ([]LearningPathCourse, error) {
	var out []LearningPathCourse
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := listCoursesTx(ctx, tx, tenantID, pathID)
		if err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// Enroll enrolls a user into a path. Idempotent via the UNIQUE
// (tenant, path, user) constraint: a repeat enroll returns the existing
// enrollment without creating a duplicate or re-stamping started_at.
// `source` distinguishes manual from auto (role-driven) enrollment.
func (s *LearningPathStore) Enroll(ctx context.Context, tenantID, pathID, userID uuid.UUID, source string, actor *uuid.UUID) (*LearningPathEnrollment, error) {
	if tenantID == uuid.Nil || pathID == uuid.Nil || userID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id, learning_path_id, user_id required", ErrInvalidLearningPath)
	}
	if source == "" {
		source = EnrollSourceManual
	}
	if source != EnrollSourceManual && source != EnrollSourceAuto {
		return nil, fmt.Errorf("%w: source %q invalid", ErrInvalidLearningPath, source)
	}
	var (
		out     LearningPathEnrollment
		created bool
	)
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Guard: the path must exist (the FK would catch this, but a
		// clean sentinel beats a raw 23503 for the API).
		if _, err := getPathTx(ctx, tx, tenantID, pathID); err != nil {
			return err
		}
		now := s.now()
		row := tx.QueryRow(ctx,
			`INSERT INTO learning_path_enrollments
			    (tenant_id, id, learning_path_id, user_id, status, source, started_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 ON CONFLICT (tenant_id, learning_path_id, user_id) DO UPDATE
			    SET updated_at = learning_path_enrollments.updated_at
			 RETURNING tenant_id, id, learning_path_id, user_id, status, source,
			           started_at, completed_at, created_at, updated_at,
			           (xmax = 0) AS inserted`,
			tenantID, uuid.New(), pathID, userID, PathEnrollmentEnrolled, source, now,
		)
		if err := scanEnrollmentWithFlag(row, &out, &created); err != nil {
			return err
		}
		if !created {
			return nil
		}
		return s.audit(ctx, tx, tenantID, actor, "lms.learning_path.enroll", out.ID, nil, mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEnrollment returns a user's enrollment in a path (or
// ErrPathEnrollmentNotFound).
func (s *LearningPathStore) GetEnrollment(ctx context.Context, tenantID, pathID, userID uuid.UUID) (*LearningPathEnrollment, error) {
	var out *LearningPathEnrollment
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		e, err := getEnrollmentTx(ctx, tx, tenantID, pathID, userID)
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListEnrollmentsForUser returns every path enrollment for a user.
func (s *LearningPathStore) ListEnrollmentsForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]LearningPathEnrollment, error) {
	var out []LearningPathEnrollment
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, learning_path_id, user_id, status, source,
			        started_at, completed_at, created_at, updated_at
			   FROM learning_path_enrollments
			  WHERE tenant_id = $1 AND user_id = $2
			  ORDER BY created_at DESC, id`,
			tenantID, userID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e LearningPathEnrollment
			if err := scanEnrollment(rows, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// SetEnrollmentStatus transitions an enrollment to the supplied status,
// stamping completed_at when moving to completed. Used by the
// completion recompute path and the explicit /complete endpoint.
func (s *LearningPathStore) SetEnrollmentStatus(ctx context.Context, tenantID, pathID, userID uuid.UUID, status string, actor *uuid.UUID) (*LearningPathEnrollment, error) {
	switch status {
	case PathEnrollmentEnrolled, PathEnrollmentInProgress, PathEnrollmentCompleted:
	default:
		return nil, fmt.Errorf("%w: status %q invalid", ErrInvalidLearningPath, status)
	}
	var out LearningPathEnrollment
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		before, err := getEnrollmentTx(ctx, tx, tenantID, pathID, userID)
		if err != nil {
			return err
		}
		var completedAt *time.Time
		if status == PathEnrollmentCompleted {
			if before.CompletedAt != nil {
				completedAt = before.CompletedAt
			} else {
				now := s.now()
				completedAt = &now
			}
		}
		row := tx.QueryRow(ctx,
			`UPDATE learning_path_enrollments
			    SET status = $4, completed_at = $5, updated_at = $6
			  WHERE tenant_id = $1 AND learning_path_id = $2 AND user_id = $3
			 RETURNING tenant_id, id, learning_path_id, user_id, status, source,
			           started_at, completed_at, created_at, updated_at`,
			tenantID, pathID, userID, status, completedAt, s.now(),
		)
		if err := scanEnrollment(row, &out); err != nil {
			return err
		}
		if before.Status == out.Status {
			return nil
		}
		return s.audit(ctx, tx, tenantID, actor, "lms.learning_path.enrollment_status", out.ID, mustJSON(before), mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RecomputeCompletion recomputes a user's path enrollment status from
// the set of courses they have completed and persists the resulting
// status (in_progress / completed). Returns the rollup so callers (the
// /complete endpoint, the worker) can surface progress. The completed
// set is supplied by the caller (resolved via the record store) so this
// method stays a thin persistence wrapper around the pure
// EvaluateCompletion rule.
func (s *LearningPathStore) RecomputeCompletion(ctx context.Context, tenantID, pathID, userID uuid.UUID, completedCourseIDs map[uuid.UUID]bool, actor *uuid.UUID) (*LearningPathEnrollment, CompletionResult, error) {
	var (
		out LearningPathEnrollment
		res CompletionResult
	)
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		before, err := getEnrollmentTx(ctx, tx, tenantID, pathID, userID)
		if err != nil {
			return err
		}
		courses, err := listCoursesTx(ctx, tx, tenantID, pathID)
		if err != nil {
			return err
		}
		res = EvaluateCompletion(courses, completedCourseIDs)
		newStatus := before.Status
		completedAt := before.CompletedAt
		switch {
		case res.Complete:
			newStatus = PathEnrollmentCompleted
			if completedAt == nil {
				now := s.now()
				completedAt = &now
			}
		case res.CompletedTotal > 0 && before.Status == PathEnrollmentEnrolled:
			newStatus = PathEnrollmentInProgress
		}
		if newStatus == before.Status {
			out = *before
			return nil
		}
		row := tx.QueryRow(ctx,
			`UPDATE learning_path_enrollments
			    SET status = $4, completed_at = $5, updated_at = $6
			  WHERE tenant_id = $1 AND learning_path_id = $2 AND user_id = $3
			 RETURNING tenant_id, id, learning_path_id, user_id, status, source,
			           started_at, completed_at, created_at, updated_at`,
			tenantID, pathID, userID, newStatus, completedAt, s.now(),
		)
		if err := scanEnrollment(row, &out); err != nil {
			return err
		}
		return s.audit(ctx, tx, tenantID, actor, "lms.learning_path.completion", out.ID, mustJSON(before), mustJSON(out))
	})
	if err != nil {
		return nil, CompletionResult{}, err
	}
	return &out, res, nil
}

// PublishedPathsForRoles returns the published paths whose target_roles
// overlap any of the supplied roles. Drives auto-enrollment; the GIN
// index on target_roles makes the && overlap operator index-assisted.
func (s *LearningPathStore) PublishedPathsForRoles(ctx context.Context, tenantID uuid.UUID, roles []string) ([]LearningPath, error) {
	roles = normalizeRoles(roles)
	if len(roles) == 0 {
		return nil, nil
	}
	var out []LearningPath
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, title, description, status, target_roles,
			        estimated_duration_hours, difficulty, created_by,
			        created_at, updated_at
			   FROM learning_paths
			  WHERE tenant_id = $1 AND status = $2 AND target_roles && $3
			  ORDER BY created_at DESC, id`,
			tenantID, PathStatusPublished, roles,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p LearningPath
			if err := scanPath(rows, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Auto-enrollment + completion lookup.
// ---------------------------------------------------------------------------

// CourseCompletionLookup resolves the set of course IDs a user has
// completed within a tenant. Implemented at the API / worker layer over
// the lms.enrollment KRecords so the lms package stays decoupled from
// the generic record store. Returning a set (map) keeps EvaluateCompletion
// O(courses) rather than O(courses × completions).
type CourseCompletionLookup interface {
	CompletedCourseIDs(ctx context.Context, tenantID, userID uuid.UUID) (map[uuid.UUID]bool, error)
}

// PathAutoEnroller enrolls users into published paths when they are
// granted a role that matches a path's target_roles. It is driven by
// authz.role.assigned events (handled in the worker) and is safe to
// invoke repeatedly: enrollment is idempotent, so replays never
// duplicate.
type PathAutoEnroller struct {
	store *LearningPathStore
}

// NewPathAutoEnroller wires an auto-enroller over a learning-path store.
func NewPathAutoEnroller(store *LearningPathStore) *PathAutoEnroller {
	return &PathAutoEnroller{store: store}
}

// OnRolesAssigned enrolls the user into every published path whose
// target_roles overlap the supplied roles. Returns the path ids the
// user was newly or already enrolled in (idempotent). Enrollments are
// stamped source="auto" so reporting can distinguish them from manual
// enrollment. An empty roles slice is a no-op.
func (a *PathAutoEnroller) OnRolesAssigned(ctx context.Context, tenantID, userID uuid.UUID, roles []string) ([]uuid.UUID, error) {
	if tenantID == uuid.Nil || userID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id and user_id required", ErrInvalidLearningPath)
	}
	paths, err := a.store.PublishedPathsForRoles(ctx, tenantID, roles)
	if err != nil {
		return nil, err
	}
	enrolled := make([]uuid.UUID, 0, len(paths))
	for i := range paths {
		pathID := paths[i].ID
		if _, err := a.store.Enroll(ctx, tenantID, pathID, userID, EnrollSourceAuto, nil); err != nil {
			return enrolled, fmt.Errorf("auto-enroll path %s: %w", pathID, err)
		}
		enrolled = append(enrolled, pathID)
	}
	return enrolled, nil
}

// ---------------------------------------------------------------------------
// tx-bound helpers + scanners.
// ---------------------------------------------------------------------------

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func getPathTx(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (*LearningPath, error) {
	row := tx.QueryRow(ctx,
		`SELECT tenant_id, id, title, description, status, target_roles,
		        estimated_duration_hours, difficulty, created_by, created_at, updated_at
		   FROM learning_paths WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	var p LearningPath
	if err := scanPath(row, &p); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrLearningPathNotFound
		}
		return nil, err
	}
	return &p, nil
}

func listCoursesTx(ctx context.Context, tx pgx.Tx, tenantID, pathID uuid.UUID) ([]LearningPathCourse, error) {
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, learning_path_id, course_id, sequence_order,
		        is_mandatory, prerequisite_course_ids, created_at
		   FROM learning_path_courses
		  WHERE tenant_id = $1 AND learning_path_id = $2
		  ORDER BY sequence_order, created_at`,
		tenantID, pathID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LearningPathCourse
	for rows.Next() {
		var c LearningPathCourse
		if err := scanPathCourse(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func getEnrollmentTx(ctx context.Context, tx pgx.Tx, tenantID, pathID, userID uuid.UUID) (*LearningPathEnrollment, error) {
	row := tx.QueryRow(ctx,
		`SELECT tenant_id, id, learning_path_id, user_id, status, source,
		        started_at, completed_at, created_at, updated_at
		   FROM learning_path_enrollments
		  WHERE tenant_id = $1 AND learning_path_id = $2 AND user_id = $3`,
		tenantID, pathID, userID)
	var e LearningPathEnrollment
	if err := scanEnrollment(row, &e); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPathEnrollmentNotFound
		}
		return nil, err
	}
	return &e, nil
}

func scanPath(row rowScanner, p *LearningPath) error {
	return row.Scan(
		&p.TenantID, &p.ID, &p.Title, &p.Description, &p.Status, &p.TargetRoles,
		&p.EstimatedDurationHours, &p.Difficulty, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
	)
}

func scanPathCourse(row rowScanner, c *LearningPathCourse) error {
	return row.Scan(
		&c.TenantID, &c.ID, &c.LearningPathID, &c.CourseID, &c.SequenceOrder,
		&c.IsMandatory, &c.PrerequisiteCourseIDs, &c.CreatedAt,
	)
}

func scanEnrollment(row rowScanner, e *LearningPathEnrollment) error {
	return row.Scan(
		&e.TenantID, &e.ID, &e.LearningPathID, &e.UserID, &e.Status, &e.Source,
		&e.StartedAt, &e.CompletedAt, &e.CreatedAt, &e.UpdatedAt,
	)
}

func scanEnrollmentWithFlag(row rowScanner, e *LearningPathEnrollment, inserted *bool) error {
	return row.Scan(
		&e.TenantID, &e.ID, &e.LearningPathID, &e.UserID, &e.Status, &e.Source,
		&e.StartedAt, &e.CompletedAt, &e.CreatedAt, &e.UpdatedAt, inserted,
	)
}

// audit appends an audit entry inside the caller's transaction. A nil
// auditor is a no-op so unit tests without an audit sink still run.
func (s *LearningPathStore) audit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, action string, target uuid.UUID, before, after []byte) error {
	if s.auditor == nil {
		return nil
	}
	kind := audit.ActorUser
	if actor == nil {
		kind = audit.ActorSystem
	}
	tid := target
	return s.auditor.LogTx(ctx, tx, audit.Entry{
		TenantID:    tenantID,
		ActorID:     actor,
		ActorKind:   kind,
		Action:      action,
		TargetKType: KTypeLearningPath,
		TargetID:    &tid,
		Before:      before,
		After:       after,
	})
}
