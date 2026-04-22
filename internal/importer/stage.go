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

// StagingStore persists rows into `import_staging`. Every insert and
// update runs inside dbutil.WithTenantTx so RLS is enforced and the
// per-tenant quota middleware can cap total staging work.
type StagingStore struct {
	pool *pgxpool.Pool
}

// NewStagingStore wraps a pool.
func NewStagingStore(pool *pgxpool.Pool) *StagingStore { return &StagingStore{pool: pool} }

// Insert appends a staging row and returns the assigned id + timestamps.
func (s *StagingStore) Insert(ctx context.Context, row StagingRow) (*StagingRow, error) {
	if row.TenantID == uuid.Nil || row.JobID == uuid.Nil {
		return nil, errors.New("importer: tenant_id and job_id required")
	}
	if row.TargetKType == "" {
		return nil, errors.New("importer: target_ktype required")
	}
	if row.Status == "" {
		row.Status = StagingPending
	}
	row.Data = defaultJSON(row.Data, "{}")
	row.ValidationErrors = defaultJSON(row.ValidationErrors, "[]")
	out := row
	err := dbutil.WithTenantTx(ctx, s.pool, row.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO import_staging
			    (tenant_id, job_id, source_type, source_id, target_ktype,
			     data, validation_errors, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			 RETURNING id, created_at, updated_at`,
			row.TenantID, row.JobID, row.SourceType,
			nullIfEmpty(row.SourceID), row.TargetKType,
			row.Data, row.ValidationErrors, row.Status,
		).Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("importer: insert staging: %w", err)
	}
	return &out, nil
}

// MarkValidated updates one staging row's status and validation_errors.
// Used by the validator stage once it has a decision per row.
func (s *StagingStore) MarkValidated(
	ctx context.Context, tenantID uuid.UUID, id int64,
	status string, errs []ValidationError,
) error {
	if status != StagingValid && status != StagingInvalid {
		return fmt.Errorf("importer: invalid validation status %q", status)
	}
	blob, err := json.Marshal(errs)
	if err != nil {
		return fmt.Errorf("importer: marshal validation errors: %w", err)
	}
	if len(errs) == 0 {
		blob = []byte("[]")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE import_staging
			    SET status = $3,
			        validation_errors = $4,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, status, blob)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrJobNotFound
		}
		return nil
	})
}

// MarkImported flips a staging row to `imported` and records the
// newly-created KRecord id so the operator can trace source → target.
func (s *StagingStore) MarkImported(ctx context.Context, tenantID uuid.UUID, id int64, recordID uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE import_staging
			    SET status = $3,
			        imported_record_id = $4,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, StagingImported, recordID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrJobNotFound
		}
		return nil
	})
}

// ListByJob returns staging rows for a job, filtered optionally by
// status and paged by limit/offset.
func (s *StagingStore) ListByJob(
	ctx context.Context, tenantID, jobID uuid.UUID,
	status string, limit, offset int,
) ([]StagingRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := make([]StagingRow, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if status == "" {
			rows, err = tx.Query(ctx,
				`SELECT id, tenant_id, job_id, source_type,
				        COALESCE(source_id, ''), target_ktype, data,
				        validation_errors, status, imported_record_id,
				        created_at, updated_at
				   FROM import_staging
				  WHERE tenant_id = $1 AND job_id = $2
				  ORDER BY id
				  LIMIT $3 OFFSET $4`,
				tenantID, jobID, limit, offset)
		} else {
			rows, err = tx.Query(ctx,
				`SELECT id, tenant_id, job_id, source_type,
				        COALESCE(source_id, ''), target_ktype, data,
				        validation_errors, status, imported_record_id,
				        created_at, updated_at
				   FROM import_staging
				  WHERE tenant_id = $1 AND job_id = $2 AND status = $3
				  ORDER BY id
				  LIMIT $4 OFFSET $5`,
				tenantID, jobID, status, limit, offset)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row StagingRow
			if err := rows.Scan(
				&row.ID, &row.TenantID, &row.JobID, &row.SourceType,
				&row.SourceID, &row.TargetKType, &row.Data,
				&row.ValidationErrors, &row.Status, &row.ImportedRecordID,
				&row.CreatedAt, &row.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

// CountsByStatus aggregates staging rows into a status→count map so
// the reconciler and the UI can render progress bars without pulling
// every row.
func (s *StagingStore) CountsByStatus(ctx context.Context, tenantID, jobID uuid.UUID) (map[string]int64, error) {
	out := make(map[string]int64)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT status, COUNT(*) FROM import_staging
			  WHERE tenant_id = $1 AND job_id = $2
			  GROUP BY status`,
			tenantID, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				status string
				count  int64
			)
			if err := rows.Scan(&status, &count); err != nil {
				return err
			}
			out[status] = count
		}
		return rows.Err()
	})
	return out, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
