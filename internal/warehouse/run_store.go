package warehouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// ErrRunNotFound is returned when a run id does not resolve under the
// tenant's RLS scope.
var ErrRunNotFound = errors.New("warehouse: sync run not found")

// RunStore persists warehouse_sync_runs under tenant RLS. A run is
// inserted as 'running' when the export starts and finalized to
// 'success' / 'error' on completion, so an in-flight or crashed run is
// observable in the history.
type RunStore struct {
	pool *pgxpool.Pool
}

// NewRunStore wires the store to the shared (tenant-scoped) pool.
func NewRunStore(pool *pgxpool.Pool) *RunStore {
	return &RunStore{pool: pool}
}

// Start inserts a new 'running' row for a config and returns it. mode
// and trigger are validated against the lifecycle constants.
func (s *RunStore) Start(ctx context.Context, tenantID, configID uuid.UUID, mode, trigger string, startedAt time.Time) (*Run, error) {
	switch mode {
	case ModeFull, ModeIncremental:
	default:
		return nil, fmt.Errorf("warehouse: invalid run mode %q", mode)
	}
	switch trigger {
	case TriggerSchedule, TriggerManual:
	default:
		return nil, fmt.Errorf("warehouse: invalid run trigger %q", trigger)
	}
	run := Run{
		TenantID:  tenantID,
		ID:        uuid.New(),
		ConfigID:  configID,
		Status:    StatusRunning,
		Mode:      mode,
		Trigger:   trigger,
		StartedAt: startedAt,
		Details:   map[string]int64{},
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO warehouse_sync_runs
			   (tenant_id, id, config_id, status, mode, trigger, started_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING created_at`,
			run.TenantID, run.ID, run.ConfigID, run.Status, run.Mode, run.Trigger, run.StartedAt,
		).Scan(&run.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("warehouse: start run: %w", err)
	}
	return &run, nil
}

// Finish finalizes a run with its terminal status, totals, per-source
// detail, and finished timestamp. errMsg is empty on success.
func (s *RunStore) Finish(ctx context.Context, run *Run, status string, finishedAt time.Time, errMsg string) error {
	switch status {
	case StatusSuccess, StatusError:
	default:
		return fmt.Errorf("warehouse: invalid terminal status %q", status)
	}
	detailsJSON, err := json.Marshal(run.Details)
	if err != nil {
		return fmt.Errorf("warehouse: marshal run details: %w", err)
	}
	err = dbutil.WithTenantTx(ctx, s.pool, run.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE warehouse_sync_runs
			    SET status = $3, rows_exported = $4, tables_exported = $5,
			        finished_at = $6, error = $7, details = $8
			  WHERE tenant_id = $1 AND id = $2`,
			run.TenantID, run.ID, status, run.RowsExported, run.TablesExported,
			finishedAt, errMsg, detailsJSON)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrRunNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	run.Status = status
	run.Error = errMsg
	run.FinishedAt = &finishedAt
	return nil
}

// List returns the run history for a config, newest first, capped at
// limit (a non-positive limit defaults to 50, and is bounded at 500 so
// a single call cannot scan an unbounded history).
func (s *RunStore) List(ctx context.Context, tenantID, configID uuid.UUID, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var out []Run
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scanRuns(ctx, tx,
			`SELECT tenant_id, id, config_id, status, mode, trigger, rows_exported,
			        tables_exported, started_at, finished_at, error, details, created_at
			   FROM warehouse_sync_runs
			  WHERE tenant_id = $1 AND config_id = $2
			  ORDER BY started_at DESC, id
			  LIMIT $3`,
			tenantID, configID, limit)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

func scanRuns(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]Run, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			r          Run
			detailsRaw []byte
			errMsg     *string
		)
		if err := rows.Scan(
			&r.TenantID, &r.ID, &r.ConfigID, &r.Status, &r.Mode, &r.Trigger, &r.RowsExported,
			&r.TablesExported, &r.StartedAt, &r.FinishedAt, &errMsg, &detailsRaw, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		if errMsg != nil {
			r.Error = *errMsg
		}
		if len(detailsRaw) > 0 {
			if err := json.Unmarshal(detailsRaw, &r.Details); err != nil {
				return nil, fmt.Errorf("warehouse: decode run details: %w", err)
			}
		}
		if r.Details == nil {
			r.Details = map[string]int64{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
