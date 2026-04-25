// Package exporter implements per-tenant data export: CSV / JSON
// dumps of a single KType (or the literal "*" for a tenant-wide
// dump) tracked through the export_jobs table.
//
// The job model is fire-and-forget: the API enqueues a row with
// status `pending`, the worker (services/worker/export_worker.go)
// claims it under FOR UPDATE SKIP LOCKED, runs the export, and
// transitions status to `completed` (or `failed` with an error
// message). Download is served from the API by streaming the
// payload BYTEA back to the user.
//
// Reference: frappe/frappe Data Export.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Format enumerates the on-disk encodings the exporter supports.
// Both round-trip through the export_jobs.format CHECK constraint.
const (
	FormatCSV  = "csv"
	FormatJSON = "json"
)

// Status enumerates the export_jobs.status values. The state
// machine is: pending → running → completed | failed.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// KTypeAll is the sentinel value callers pass to request a
// tenant-wide dump rather than a single-KType slice.
const KTypeAll = "*"

// ExportJob mirrors a row of the export_jobs table. Payload is
// only populated on completion; while pending / running it is nil.
type ExportJob struct {
	TenantID    uuid.UUID  `json:"tenant_id"`
	ID          uuid.UUID  `json:"id"`
	KType       string     `json:"ktype"`
	Format      string     `json:"format"`
	Status      string     `json:"status"`
	ProgressPct int        `json:"progress_pct"`
	RowCount    int64      `json:"row_count"`
	Payload     []byte     `json:"-"` // never marshalled into the JSON response
	Error       string     `json:"error,omitempty"`
	FileName    string     `json:"file_name"`
	ContentType string     `json:"content_type"`
	CreatedBy   *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Sentinel errors surfaced to the API layer.
var (
	ErrJobNotFound  = errors.New("exporter: export job not found")
	ErrJobNotReady  = errors.New("exporter: export job not yet completed")
	ErrInvalidInput = errors.New("exporter: invalid input")
)

// Validate rejects malformed enqueue requests before they hit the DB.
func (j *ExportJob) Validate() error {
	if j.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	if j.KType == "" {
		return fmt.Errorf("%w: ktype required (use \"*\" for tenant-wide dump)", ErrInvalidInput)
	}
	switch j.Format {
	case FormatCSV, FormatJSON:
	default:
		return fmt.Errorf("%w: format must be csv|json", ErrInvalidInput)
	}
	return nil
}

// Store persists ExportJob rows. Writes happen under
// dbutil.WithTenantTx so the tenant_isolation RLS policy applies.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Enqueue creates a new export_jobs row with status `pending`. The
// caller is responsible for filling KType, Format, FileName, and
// optionally CreatedBy. Other fields are populated from defaults.
func (s *Store) Enqueue(ctx context.Context, j ExportJob) (*ExportJob, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	if j.ID == uuid.Nil {
		j.ID = uuid.New()
	}
	if j.FileName == "" {
		j.FileName = fmt.Sprintf("%s-%s.%s", sanitizeKType(j.KType), j.ID.String()[:8], j.Format)
	}
	if j.ContentType == "" {
		switch j.Format {
		case FormatCSV:
			j.ContentType = "text/csv; charset=utf-8"
		case FormatJSON:
			j.ContentType = "application/json"
		}
	}
	out := j
	err := dbutil.WithTenantTx(ctx, s.pool, j.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if j.CreatedBy != nil {
			createdBy = *j.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO export_jobs
			     (tenant_id, id, ktype, format, status, file_name, content_type, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING created_at`,
			j.TenantID, j.ID, j.KType, j.Format, StatusPending,
			j.FileName, j.ContentType, createdBy,
		).Scan(&out.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("exporter: enqueue: %w", err)
	}
	out.Status = StatusPending
	return &out, nil
}

// Get returns one job by id or ErrJobNotFound.
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (*ExportJob, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and job id required", ErrInvalidInput)
	}
	var out ExportJob
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJob(tx.QueryRow(ctx,
			`SELECT tenant_id, id, ktype, format, status, progress_pct, row_count,
			        payload, error, file_name, content_type, created_by,
			        created_at, started_at, completed_at
			   FROM export_jobs WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	return &out, nil
}

// List returns a tenant's export jobs ordered by created_at DESC.
// Capped at 100 rows so the UI never paginates over an unbounded
// table on a noisy tenant.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID) ([]ExportJob, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrInvalidInput)
	}
	out := make([]ExportJob, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, ktype, format, status, progress_pct, row_count,
			        NULL::bytea AS payload, error, file_name, content_type, created_by,
			        created_at, started_at, completed_at
			   FROM export_jobs WHERE tenant_id = $1
			   ORDER BY created_at DESC
			   LIMIT 100`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j ExportJob
			if err := scanJob(rows, &j); err != nil {
				return err
			}
			out = append(out, j)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimNext atomically claims the oldest pending job in any tenant
// using FOR UPDATE SKIP LOCKED, marking it `running` so concurrent
// workers do not collide. Returns nil with nil error when the
// queue is drained — the worker uses that as the loop sentinel.
//
// We deliberately do NOT use dbutil.WithTenantTx here because the
// claim has to scan across all tenants. The follow-up Process call
// runs under the per-tenant tx so the actual export still flows
// through the tenant_isolation policy.
func (s *Store) ClaimNext(ctx context.Context) (*ExportJob, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Disable RLS for this scan — the worker is the system principal.
	if _, err := tx.Exec(ctx, `SET LOCAL row_security = OFF`); err != nil {
		return nil, fmt.Errorf("exporter: disable RLS: %w", err)
	}

	var out ExportJob
	err = scanJob(tx.QueryRow(ctx,
		`UPDATE export_jobs SET status = $1, started_at = now()
		   WHERE id = (
		       SELECT id FROM export_jobs
		         WHERE status = $2
		         ORDER BY created_at
		         LIMIT 1
		         FOR UPDATE SKIP LOCKED
		   )
		   RETURNING tenant_id, id, ktype, format, status, progress_pct, row_count,
		             NULL::bytea AS payload, error, file_name, content_type, created_by,
		             created_at, started_at, completed_at`,
		StatusRunning, StatusPending,
	), &out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

// MarkProgress updates progress_pct + row_count without touching
// status. Called by the per-KType exporter every N rows so the UI
// can render a live progress bar.
func (s *Store) MarkProgress(ctx context.Context, tenantID, id uuid.UUID, pct int, rowCount int64) error {
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE export_jobs SET progress_pct = $3, row_count = $4
			   WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, pct, rowCount,
		)
		return err
	})
}

// Complete persists the export payload and flips status to completed.
func (s *Store) Complete(ctx context.Context, tenantID, id uuid.UUID, payload []byte, rowCount int64) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE export_jobs
			    SET status = $3, progress_pct = 100, row_count = $5,
			        payload = $4, completed_at = now(), error = NULL
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, StatusCompleted, payload, rowCount,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrJobNotFound
		}
		return nil
	})
}

// Fail flips status to failed and records the error message.
func (s *Store) Fail(ctx context.Context, tenantID, id uuid.UUID, errMsg string) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE export_jobs
			    SET status = $3, error = $4, completed_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, StatusFailed, errMsg,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrJobNotFound
		}
		return nil
	})
}

// rowScanner abstracts pgx.Row vs pgx.Rows so scanJob serves both
// Get and List paths without duplication.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(r rowScanner, j *ExportJob) error {
	var (
		createdBy   *uuid.UUID
		startedAt   *time.Time
		completedAt *time.Time
		payload     []byte
		errMsg      *string
	)
	if err := r.Scan(
		&j.TenantID, &j.ID, &j.KType, &j.Format, &j.Status,
		&j.ProgressPct, &j.RowCount, &payload, &errMsg,
		&j.FileName, &j.ContentType, &createdBy,
		&j.CreatedAt, &startedAt, &completedAt,
	); err != nil {
		return err
	}
	j.Payload = payload
	if errMsg != nil {
		j.Error = *errMsg
	}
	j.CreatedBy = createdBy
	j.StartedAt = startedAt
	j.CompletedAt = completedAt
	return nil
}

// sanitizeKType removes filesystem-unsafe characters from KType
// names so generated FileNames never escape the export directory.
func sanitizeKType(k string) string {
	if k == KTypeAll {
		return "tenant-dump"
	}
	out := make([]byte, 0, len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		case c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
