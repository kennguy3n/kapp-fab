package lms

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// ProgressStatus captures lesson completion state.
const (
	ProgressNotStarted = "not_started"
	ProgressInProgress = "in_progress"
	ProgressCompleted  = "completed"
)

// Progress is one row in `lesson_progress`. Identity is
// (tenant_id, enrollment_id, lesson_id). Score is optional — only
// lessons with a quiz or assignment carry one.
type Progress struct {
	TenantID     uuid.UUID        `json:"tenant_id"`
	EnrollmentID uuid.UUID        `json:"enrollment_id"`
	LessonID     uuid.UUID        `json:"lesson_id"`
	Status       string           `json:"status"`
	Score        *decimal.Decimal `json:"score,omitempty"`
	Attempts     int              `json:"attempts"`
	StartedAt    *time.Time       `json:"started_at,omitempty"`
	CompletedAt  *time.Time       `json:"completed_at,omitempty"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// EnrollmentSummary is one row in `enrollment_progress` — how many
// lessons in the enrollment are complete vs total.
type EnrollmentSummary struct {
	EnrollmentID     uuid.UUID `json:"enrollment_id"`
	CompletedLessons int       `json:"completed_lessons"`
	TotalLessons     int       `json:"total_lessons"`
}

// Store narrows the LMS persistence surface to the pieces agent tools
// and the /learn KChat command need. Course / module / lesson KRecords
// stay in the generic record.PGStore.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a Store backed by the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertProgress creates or updates the (enrollment_id, lesson_id)
// progress row. Status transitions clamp to the allowed set via the
// DB CHECK constraint. `attempts` is incremented by one on every call
// that provides a non-nil score (quiz submission / assignment grade).
func (s *Store) UpsertProgress(ctx context.Context, p Progress) (*Progress, error) {
	if p.TenantID == uuid.Nil || p.EnrollmentID == uuid.Nil || p.LessonID == uuid.Nil {
		return nil, errors.New("lms: tenant_id, enrollment_id, lesson_id required")
	}
	if p.Status == "" {
		p.Status = ProgressInProgress
	}
	var out Progress
	err := platform.WithTenantTx(ctx, s.pool, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now().UTC()
		incr := 0
		if p.Score != nil {
			incr = 1
		}
		row := tx.QueryRow(ctx,
			`INSERT INTO lesson_progress
			    (tenant_id, enrollment_id, lesson_id, status, score,
			     attempts, started_at, completed_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 ON CONFLICT (tenant_id, enrollment_id, lesson_id) DO UPDATE
			    SET status       = EXCLUDED.status,
			        score        = COALESCE(EXCLUDED.score, lesson_progress.score),
			        attempts     = lesson_progress.attempts + $10,
			        started_at   = COALESCE(lesson_progress.started_at, EXCLUDED.started_at),
			        completed_at = COALESCE(EXCLUDED.completed_at, lesson_progress.completed_at),
			        updated_at   = EXCLUDED.updated_at
			 RETURNING tenant_id, enrollment_id, lesson_id, status, score,
			           attempts, started_at, completed_at, updated_at`,
			p.TenantID, p.EnrollmentID, p.LessonID, p.Status, p.Score,
			incr, p.StartedAt, p.CompletedAt, now, incr,
		)
		return row.Scan(
			&out.TenantID, &out.EnrollmentID, &out.LessonID, &out.Status,
			&out.Score, &out.Attempts, &out.StartedAt, &out.CompletedAt, &out.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("upsert lesson_progress: %w", err)
	}
	return &out, nil
}

// ListProgress returns every progress row for an enrollment.
func (s *Store) ListProgress(ctx context.Context, tenantID, enrollmentID uuid.UUID) ([]Progress, error) {
	var out []Progress
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, enrollment_id, lesson_id, status, score,
			        attempts, started_at, completed_at, updated_at
			   FROM lesson_progress
			  WHERE tenant_id = $1 AND enrollment_id = $2
			  ORDER BY lesson_id`,
			tenantID, enrollmentID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Progress
			if err := rows.Scan(
				&p.TenantID, &p.EnrollmentID, &p.LessonID, &p.Status, &p.Score,
				&p.Attempts, &p.StartedAt, &p.CompletedAt, &p.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// EnrollmentSummary returns the completed/total rollup for a single
// enrollment. Missing rows surface as (0, 0).
func (s *Store) EnrollmentSummary(ctx context.Context, tenantID, enrollmentID uuid.UUID) (*EnrollmentSummary, error) {
	summary := &EnrollmentSummary{EnrollmentID: enrollmentID}
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT COALESCE(completed_lessons, 0), COALESCE(total_lessons, 0)
			   FROM enrollment_progress
			  WHERE tenant_id = $1 AND enrollment_id = $2`,
			tenantID, enrollmentID,
		)
		err := row.Scan(&summary.CompletedLessons, &summary.TotalLessons)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return summary, err
}
