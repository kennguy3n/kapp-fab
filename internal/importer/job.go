package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// JobStore persists ImportJob rows. All reads/writes happen inside
// dbutil.WithTenantTx so RLS on `import_jobs` is authoritative.
type JobStore struct {
	pool *pgxpool.Pool
}

// NewJobStore constructs a JobStore over the shared pool.
func NewJobStore(pool *pgxpool.Pool) *JobStore { return &JobStore{pool: pool} }

// Create inserts a fresh job in `pending` state. The caller supplies
// the source type, opaque config blob, and creator user id.
func (s *JobStore) Create(ctx context.Context, job ImportJob) (*ImportJob, error) {
	if job.TenantID == uuid.Nil || job.CreatedBy == uuid.Nil {
		return nil, errors.New("importer: tenant_id and created_by required")
	}
	if job.SourceType == "" {
		return nil, errors.New("importer: source_type required")
	}
	if job.ID == uuid.Nil {
		job.ID = uuid.New()
	}
	if job.Status == "" {
		job.Status = StagePending
	}
	job.Config = defaultJSON(job.Config, "{}")
	job.Mapping = defaultJSON(job.Mapping, "{}")
	job.Progress = defaultJSON(job.Progress, "{}")
	job.Errors = defaultJSON(job.Errors, "[]")
	job.Reconciliation = defaultJSON(job.Reconciliation, "{}")
	out := job
	err := dbutil.WithTenantTx(ctx, s.pool, job.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO import_jobs
			     (id, tenant_id, source_type, status, config, mapping,
			      progress, errors, reconciliation, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			 RETURNING created_at, updated_at`,
			job.ID, job.TenantID, job.SourceType, job.Status,
			job.Config, job.Mapping, job.Progress, job.Errors,
			job.Reconciliation, job.CreatedBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("importer: create job: %w", err)
	}
	return &out, nil
}

// Get returns a job by id for the tenant or ErrJobNotFound.
func (s *JobStore) Get(ctx context.Context, tenantID, jobID uuid.UUID) (*ImportJob, error) {
	var job ImportJob
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJob(tx.QueryRow(ctx,
			`SELECT id, tenant_id, source_type, status, config, mapping,
			        progress, errors, reconciliation,
			        created_by, created_at, updated_at, completed_at
			   FROM import_jobs
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, jobID), &job)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	return &job, nil
}

// List returns recent jobs for the tenant, newest first.
func (s *JobStore) List(ctx context.Context, tenantID uuid.UUID, limit int) ([]ImportJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := make([]ImportJob, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, source_type, status, config, mapping,
			        progress, errors, reconciliation,
			        created_by, created_at, updated_at, completed_at
			   FROM import_jobs
			  WHERE tenant_id = $1
			  ORDER BY updated_at DESC
			  LIMIT $2`,
			tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var job ImportJob
			if err := scanJob(rows, &job); err != nil {
				return err
			}
			out = append(out, job)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateStatus moves the job to a new status and persists any progress
// / error deltas. The caller is responsible for validating the
// transition via Pipeline.Advance; this helper only writes.
func (s *JobStore) UpdateStatus(
	ctx context.Context, tenantID, jobID uuid.UUID,
	status string, progress, errs json.RawMessage,
) (*ImportJob, error) {
	progress = defaultJSON(progress, "{}")
	errs = defaultJSON(errs, "[]")
	out := ImportJob{}
	completed := status == StageCompleted || status == StageFailed
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJob(tx.QueryRow(ctx,
			`UPDATE import_jobs
			    SET status = $3,
			        progress = $4,
			        errors = $5,
			        updated_at = now(),
			        completed_at = CASE WHEN $6 THEN now() ELSE completed_at END
			  WHERE tenant_id = $1 AND id = $2
			 RETURNING id, tenant_id, source_type, status, config, mapping,
			           progress, errors, reconciliation,
			           created_by, created_at, updated_at, completed_at`,
			tenantID, jobID, status, progress, errs, completed), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	return &out, nil
}

// UpdateMapping persists a field-mapping blob submitted by the
// operator. The blob shape is opaque to the store — the adapter and
// validator are responsible for interpreting it.
func (s *JobStore) UpdateMapping(ctx context.Context, tenantID, jobID uuid.UUID, mapping json.RawMessage) (*ImportJob, error) {
	mapping = defaultJSON(mapping, "{}")
	out := ImportJob{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJob(tx.QueryRow(ctx,
			`UPDATE import_jobs
			    SET mapping = $3,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2
			 RETURNING id, tenant_id, source_type, status, config, mapping,
			           progress, errors, reconciliation,
			           created_by, created_at, updated_at, completed_at`,
			tenantID, jobID, mapping), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	return &out, nil
}

// UpdateReconciliation writes a reconciliation summary into the
// `reconciliation` JSONB column so the UI can render the report.
func (s *JobStore) UpdateReconciliation(ctx context.Context, tenantID, jobID uuid.UUID, summary Reconciliation) (*ImportJob, error) {
	blob, err := json.Marshal(summary)
	if err != nil {
		return nil, fmt.Errorf("importer: marshal reconciliation: %w", err)
	}
	out := ImportJob{}
	err = dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJob(tx.QueryRow(ctx,
			`UPDATE import_jobs
			    SET reconciliation = $3,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2
			 RETURNING id, tenant_id, source_type, status, config, mapping,
			           progress, errors, reconciliation,
			           created_by, created_at, updated_at, completed_at`,
			tenantID, jobID, blob), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	return &out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(r rowScanner, out *ImportJob) error {
	return r.Scan(
		&out.ID, &out.TenantID, &out.SourceType, &out.Status,
		&out.Config, &out.Mapping, &out.Progress, &out.Errors,
		&out.Reconciliation,
		&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt, &out.CompletedAt,
	)
}

func defaultJSON(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}
