package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeDataRetentionSweep is the scheduler action_type the
// wizard seeds for the daily retention sweep. The worker registers
// the RetentionSweeper under this name.
const ActionTypeDataRetentionSweep = "data_retention_sweep"

// Categories of data covered by the retention sweeper. The sweeper
// maps each category to its (table, timestamp_column) pair below.
const (
	RetentionCategoryAuditLog          = "audit_log"
	RetentionCategoryEvents            = "events"
	RetentionCategorySLALog            = "sla_log"
	RetentionCategoryWebhookDeliveries = "webhook_deliveries"
	RetentionCategoryNotifications     = "notifications"
	RetentionCategoryImportStaging     = "import_staging"
)

// retentionTarget is the (table, ts_column, optional extra predicate)
// triple we DELETE under per category. extra runs as part of the
// WHERE clause so categories like "events" can require delivered=true
// before removal.
type retentionTarget struct {
	table    string
	tsColumn string
	extra    string
}

var retentionTargets = map[string]retentionTarget{
	RetentionCategoryAuditLog:          {table: "audit_log", tsColumn: "created_at"},
	RetentionCategoryEvents:            {table: "events", tsColumn: "created_at", extra: "delivered = TRUE"},
	RetentionCategorySLALog:            {table: "sla_event_log", tsColumn: "occurred_at"},
	RetentionCategoryWebhookDeliveries: {table: "webhook_delivery_log", tsColumn: "delivered_at"},
	RetentionCategoryNotifications:     {table: "notifications", tsColumn: "created_at"},
	RetentionCategoryImportStaging:     {table: "import_jobs", tsColumn: "completed_at", extra: "status IN ('completed','failed')"},
}

// RetentionPolicy is one row of data_retention_policies.
type RetentionPolicy struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	Category      string    `json:"category"`
	RetentionDays int       `json:"retention_days"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RetentionStore reads/writes data_retention_policies.
type RetentionStore struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
}

// NewRetentionStore wires the store. adminPool is required for
// cross-tenant scans by the worker; pool is the regular RLS-scoped
// pool used by per-tenant CRUD endpoints.
func NewRetentionStore(pool, adminPool *pgxpool.Pool) *RetentionStore {
	return &RetentionStore{pool: pool, adminPool: adminPool}
}

// Upsert writes a (tenant, category) policy. retention_days must be in
// [1, 3650]; the table CHECK enforces the same bound but the explicit
// guard here gives callers a typed validation error.
func (s *RetentionStore) Upsert(ctx context.Context, p RetentionPolicy) (*RetentionPolicy, error) {
	if p.TenantID == uuid.Nil {
		return nil, errors.New("retention: tenant id required")
	}
	if _, ok := retentionTargets[p.Category]; !ok {
		return nil, fmt.Errorf("retention: unknown category %q", p.Category)
	}
	if p.RetentionDays < 1 || p.RetentionDays > 3650 {
		return nil, errors.New("retention: retention_days must be 1..3650")
	}
	out := p
	err := dbutil.WithTenantTx(ctx, s.pool, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO data_retention_policies (tenant_id, category, retention_days, enabled)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, category) DO UPDATE SET
			     retention_days = EXCLUDED.retention_days,
			     enabled        = EXCLUDED.enabled,
			     updated_at     = now()
			 RETURNING created_at, updated_at`,
			p.TenantID, p.Category, p.RetentionDays, p.Enabled,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("retention: upsert: %w", err)
	}
	return &out, nil
}

// List returns the tenant's policies. Categories without an explicit
// row are not synthesised here — the wizard seeds defaults; callers
// can interpret a missing category as "no retention configured".
func (s *RetentionStore) List(ctx context.Context, tenantID uuid.UUID) ([]RetentionPolicy, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("retention: tenant id required")
	}
	out := make([]RetentionPolicy, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, category, retention_days, enabled, created_at, updated_at
			   FROM data_retention_policies WHERE tenant_id = $1
			  ORDER BY category`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p RetentionPolicy
			if err := rows.Scan(&p.TenantID, &p.Category, &p.RetentionDays, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("retention: list: %w", err)
	}
	return out, nil
}

// RetentionSweeper implements scheduler.ActionHandler. On each run
// it loads the tenant's enabled policies and DELETEs rows older than
// retention_days for each (table, ts_column) pair.
type RetentionSweeper struct {
	store *RetentionStore
}

// NewRetentionSweeper wires the sweeper from a store. Sweeps execute
// under the tenant's RLS context (per-DELETE WithTenantTx) so a buggy
// policy can never delete another tenant's rows.
func NewRetentionSweeper(store *RetentionStore) *RetentionSweeper {
	if store == nil {
		panic("retention: sweeper requires non-nil store")
	}
	return &RetentionSweeper{store: store}
}

// Handle is the scheduler entry point. The action's payload is
// reserved for future per-run overrides; nothing reads it today.
func (s *RetentionSweeper) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	policies, err := s.store.List(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("retention sweep: list: %w", err)
	}
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		target, ok := retentionTargets[p.Category]
		if !ok {
			continue
		}
		if err := s.sweepOne(ctx, tenantID, target, p.RetentionDays); err != nil {
			return fmt.Errorf("retention sweep %s: %w", p.Category, err)
		}
	}
	return nil
}

func (s *RetentionSweeper) sweepOne(ctx context.Context, tenantID uuid.UUID, target retentionTarget, days int) error {
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	extra := ""
	if target.extra != "" {
		extra = " AND " + target.extra
	}
	sql := fmt.Sprintf(
		`DELETE FROM %s WHERE tenant_id = $1 AND %s < $2%s`,
		target.table, target.tsColumn, extra,
	)
	return dbutil.WithTenantTx(ctx, s.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, tenantID, cutoff)
		return err
	})
}
