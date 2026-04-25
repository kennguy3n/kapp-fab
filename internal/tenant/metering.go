package tenant

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

// Canonical metric identifiers used by the middleware + file-upload
// path. New metrics can be added here in lock-step with
// plan_definitions.limits entries.
const (
	MetricAPICalls     = "api_calls"
	MetricStorageBytes = "storage_bytes"
	MetricKRecordCount = "krecord_count"
	MetricUserSeats    = "user_seats"
)

// UsageRow mirrors one row of tenant_usage for the API.
type UsageRow struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	PeriodStart time.Time `json:"period_start"`
	Metric      string    `json:"metric"`
	Value       int64     `json:"value"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MeteringStore persists per-tenant usage counters. Period is keyed
// by the first day of the containing month so a monthly billing
// cycle needs no separate cursor column.
type MeteringStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewMeteringStore binds a store to the shared pool.
func NewMeteringStore(pool *pgxpool.Pool) *MeteringStore {
	return &MeteringStore{
		pool: pool,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// WithNow pins the clock for deterministic tests.
func (s *MeteringStore) WithNow(now func() time.Time) *MeteringStore {
	if now != nil {
		s.now = now
	}
	return s
}

// CurrentPeriod returns the UTC first-of-month date the store keys
// increments by. Exposed so callers and handlers can describe the
// reporting window without duplicating the truncation logic.
func (s *MeteringStore) CurrentPeriod() time.Time {
	t := s.now().UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Increment atomically adds delta to (tenant, current_period,
// metric). delta may be negative (e.g. file deletion returning
// storage); the row is created on first write with the delta as
// its initial value.
func (s *MeteringStore) Increment(ctx context.Context, tenantID uuid.UUID, metric string, delta int64) error {
	if tenantID == uuid.Nil {
		return errors.New("tenant: tenant id required")
	}
	if metric == "" {
		return errors.New("tenant: metric required")
	}
	if delta == 0 {
		return nil
	}
	period := s.CurrentPeriod()
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_usage (tenant_id, period_start, metric, value)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, period_start, metric)
			 DO UPDATE SET value = tenant_usage.value + EXCLUDED.value,
			               updated_at = now()`,
			tenantID, period, metric, delta,
		)
		if err != nil {
			return fmt.Errorf("tenant: increment usage: %w", err)
		}
		return nil
	})
}

// GetUsage returns the counter for (tenant, period, metric) or
// zero when no row has been written yet. period is truncated to
// the first of its month to match the upsert key schema.
func (s *MeteringStore) GetUsage(ctx context.Context, tenantID uuid.UUID, period time.Time, metric string) (int64, error) {
	if tenantID == uuid.Nil {
		return 0, errors.New("tenant: tenant id required")
	}
	p := time.Date(period.Year(), period.Month(), 1, 0, 0, 0, 0, time.UTC)
	var v int64
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT value FROM tenant_usage
			 WHERE tenant_id = $1 AND period_start = $2 AND metric = $3`,
			tenantID, p, metric,
		).Scan(&v)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				v = 0
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("tenant: get usage: %w", err)
	}
	return v, nil
}

// GetAllMetrics returns every (metric, value) pair for the current
// billing period. Missing metrics are omitted — callers are
// expected to zero-fill against the plan's limits map when
// rendering the dashboard.
func (s *MeteringStore) GetAllMetrics(ctx context.Context, tenantID uuid.UUID) ([]UsageRow, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: tenant id required")
	}
	period := s.CurrentPeriod()
	out := []UsageRow{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, period_start, metric, value, updated_at
			 FROM tenant_usage
			 WHERE tenant_id = $1 AND period_start = $2
			 ORDER BY metric`,
			tenantID, period,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row UsageRow
			if err := rows.Scan(&row.TenantID, &row.PeriodStart, &row.Metric, &row.Value, &row.UpdatedAt); err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tenant: get all metrics: %w", err)
	}
	return out, nil
}

// HistoryRow is one (period, metric, value) tuple returned by
// GetHistory. The period_start anchors the row to a calendar month.
type HistoryRow struct {
	PeriodStart time.Time `json:"period_start"`
	Metric      string    `json:"metric"`
	Value       int64     `json:"value"`
}

// GetHistory returns the last `months` calendar periods of usage
// data for the tenant, ordered oldest -> newest. Used by the
// dashboard's historical-trend chart. Caller specifies months; the
// store guards against silly values (clamped to [1, 24]).
func (s *MeteringStore) GetHistory(ctx context.Context, tenantID uuid.UUID, months int) ([]HistoryRow, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: tenant id required")
	}
	if months < 1 {
		months = 6
	}
	if months > 24 {
		months = 24
	}
	cur := s.CurrentPeriod()
	cutoff := time.Date(cur.Year(), cur.Month()-time.Month(months-1), 1, 0, 0, 0, 0, time.UTC)
	out := []HistoryRow{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT period_start, metric, value
			   FROM tenant_usage
			  WHERE tenant_id = $1 AND period_start >= $2
			  ORDER BY period_start ASC, metric ASC`,
			tenantID, cutoff,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row HistoryRow
			if err := rows.Scan(&row.PeriodStart, &row.Metric, &row.Value); err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tenant: get history: %w", err)
	}
	return out, nil
}

// SnapshotStorageBytes records the tenant's current total storage
// footprint (sum of files.size_bytes) into tenant_usage. Called
// daily by the worker so the dashboard's storage line is accurate
// across the billing period instead of only reflecting the deltas
// observed since the last write.
//
// The implementation uses an absolute write rather than Increment
// to avoid drift from missed delta events.
func (s *MeteringStore) SnapshotStorageBytes(ctx context.Context, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return errors.New("tenant: tenant id required")
	}
	period := s.CurrentPeriod()
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var total int64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(size_bytes), 0) FROM files WHERE tenant_id = $1`,
			tenantID,
		).Scan(&total); err != nil {
			return fmt.Errorf("tenant: snapshot storage scan: %w", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_usage (tenant_id, period_start, metric, value)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, period_start, metric)
			 DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			tenantID, period, MetricStorageBytes, total,
		)
		if err != nil {
			return fmt.Errorf("tenant: snapshot storage upsert: %w", err)
		}
		return nil
	})
}

// SnapshotKRecordCount writes the tenant's current krecord row
// count into tenant_usage. Same pattern as SnapshotStorageBytes.
func (s *MeteringStore) SnapshotKRecordCount(ctx context.Context, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return errors.New("tenant: tenant id required")
	}
	period := s.CurrentPeriod()
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var total int64
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM krecords WHERE tenant_id = $1`,
			tenantID,
		).Scan(&total); err != nil {
			return fmt.Errorf("tenant: snapshot krecord scan: %w", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_usage (tenant_id, period_start, metric, value)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, period_start, metric)
			 DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			tenantID, period, MetricKRecordCount, total,
		)
		if err != nil {
			return fmt.Errorf("tenant: snapshot krecord upsert: %w", err)
		}
		return nil
	})
}
