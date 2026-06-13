package warehouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Sync modes and run lifecycle constants. These mirror the CHECK
// constraints in migrations/000094_warehouse_sync.sql; the store
// validates against them before any write so a bad value is rejected
// in Go rather than surfacing as an opaque constraint violation.
const (
	ModeFull        = "full"
	ModeIncremental = "incremental"

	StatusRunning = "running"
	StatusSuccess = "success"
	StatusError   = "error"

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"

	// DefaultDestinationSchema is the target schema the mirror tables
	// are written into when a config does not specify one.
	DefaultDestinationSchema = "kapp"

	// MaxSourcesPerConfig bounds the fan-out of a single sync so one
	// config cannot pin a worker iteration for an unbounded time or
	// open an unbounded number of destination statements.
	MaxSourcesPerConfig = 100
)

// ErrConfigNotFound is returned when a config id does not resolve
// under the tenant's RLS scope.
var ErrConfigNotFound = errors.New("warehouse: sync config not found")

// ErrRunInProgress is returned when a run for a config is requested
// while another run of the SAME config is already executing. It maps
// to HTTP 409 so a "run now" click that collides with a scheduler tick
// (or a double click) is reported as a benign conflict rather than
// racing into a second concurrent export.
var ErrRunInProgress = errors.New("warehouse: a run for this config is already in progress")

// ErrInvalidConfig wraps every client-correctable rejection (failed
// validation, a duplicate name, a destination datasource that does not
// exist) so the HTTP layer can map the whole class to 400 without
// matching on message text.
var ErrInvalidConfig = errors.New("warehouse: invalid config")

// invalidConfig tags err as client-correctable.
func invalidConfig(err error) error {
	return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
}

// mapWriteErr translates a Postgres constraint violation raised by a
// config write into the client-correctable class. The two reachable
// constraints are the per-tenant unique name and the composite FK to
// insights_data_sources.
func mapWriteErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return invalidConfig(errors.New("name already in use"))
		case "23503":
			return invalidConfig(errors.New("destination datasource not found"))
		}
	}
	return err
}

// cronParser enforces the same 5-field cron grammar the platform
// scheduler and the report scheduler use, so a cron string entered for
// a warehouse sync behaves identically to one entered for a report
// schedule.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Config is one registered warehouse sync. Watermarks holds the
// per-source incremental cursor keyed by source key; callers treat it
// as opaque (the export engine owns the shape). LastRun* denormalize
// the most recent run outcome for cheap listing.
type Config struct {
	TenantID                uuid.UUID                  `json:"tenant_id"`
	ID                      uuid.UUID                  `json:"id"`
	Name                    string                     `json:"name"`
	DestinationDataSourceID uuid.UUID                  `json:"destination_datasource_id"`
	DestinationSchema       string                     `json:"destination_schema"`
	Sources                 []string                   `json:"sources"`
	CronExpression          string                     `json:"cron_expression"`
	Mode                    string                     `json:"mode"`
	Enabled                 bool                       `json:"enabled"`
	Watermarks              map[string]json.RawMessage `json:"-"`
	LastRunAt               *time.Time                 `json:"last_run_at,omitempty"`
	LastStatus              string                     `json:"last_status,omitempty"`
	LastError               string                     `json:"last_error,omitempty"`
	CreatedBy               *uuid.UUID                 `json:"created_by,omitempty"`
	CreatedAt               time.Time                  `json:"created_at"`
	UpdatedAt               time.Time                  `json:"updated_at"`
}

// Run is one row of warehouse_sync_runs — the auditable history of a
// single export. Details maps each source key to the number of rows
// that landed for it on this run.
type Run struct {
	TenantID       uuid.UUID        `json:"tenant_id"`
	ID             uuid.UUID        `json:"id"`
	ConfigID       uuid.UUID        `json:"config_id"`
	Status         string           `json:"status"`
	Mode           string           `json:"mode"`
	Trigger        string           `json:"trigger"`
	RowsExported   int64            `json:"rows_exported"`
	TablesExported int              `json:"tables_exported"`
	StartedAt      time.Time        `json:"started_at"`
	FinishedAt     *time.Time       `json:"finished_at,omitempty"`
	Error          string           `json:"error,omitempty"`
	Details        map[string]int64 `json:"details,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// ConfigStore persists warehouse_sync_configs under tenant RLS.
type ConfigStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewConfigStore wires the store to the shared (tenant-scoped) pool.
func NewConfigStore(pool *pgxpool.Pool) *ConfigStore {
	return &ConfigStore{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

// Validate normalizes and checks a config before a write. It defaults
// the destination schema, dedupes sources while preserving order, and
// rejects anything the export engine could not faithfully run.
func (c *Config) Validate() error {
	if c.Name == "" {
		return errors.New("warehouse: name required")
	}
	if c.DestinationDataSourceID == uuid.Nil {
		return errors.New("warehouse: destination_datasource_id required")
	}
	if c.DestinationSchema == "" {
		c.DestinationSchema = DefaultDestinationSchema
	}
	if !isIdentifier(c.DestinationSchema) {
		return fmt.Errorf("warehouse: invalid destination_schema %q", c.DestinationSchema)
	}
	switch c.Mode {
	case "":
		c.Mode = ModeIncremental
	case ModeFull, ModeIncremental:
	default:
		return fmt.Errorf("warehouse: invalid mode %q", c.Mode)
	}
	if c.CronExpression == "" {
		return errors.New("warehouse: cron_expression required")
	}
	if _, err := cronParser.Parse(c.CronExpression); err != nil {
		return fmt.Errorf("warehouse: invalid cron_expression %q: %w", c.CronExpression, err)
	}
	if len(c.Sources) == 0 {
		return errors.New("warehouse: at least one source required")
	}
	if len(c.Sources) > MaxSourcesPerConfig {
		return fmt.Errorf("warehouse: too many sources (%d > %d)", len(c.Sources), MaxSourcesPerConfig)
	}
	seen := make(map[string]struct{}, len(c.Sources))
	deduped := make([]string, 0, len(c.Sources))
	for _, s := range c.Sources {
		if _, err := resolveSource(s); err != nil {
			return err
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		deduped = append(deduped, s)
	}
	c.Sources = deduped
	return nil
}

// Create inserts a new config. Watermarks always start empty: the
// first incremental run reads from the beginning and records cursors
// as rows land.
func (s *ConfigStore) Create(ctx context.Context, c Config) (*Config, error) {
	if c.TenantID == uuid.Nil {
		return nil, errors.New("warehouse: tenant id required")
	}
	if err := c.Validate(); err != nil {
		return nil, invalidConfig(err)
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	sourcesJSON, err := json.Marshal(c.Sources)
	if err != nil {
		return nil, fmt.Errorf("warehouse: marshal sources: %w", err)
	}
	out := c
	err = dbutil.WithTenantTx(ctx, s.pool, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if c.CreatedBy != nil {
			createdBy = *c.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO warehouse_sync_configs
			   (tenant_id, id, name, destination_datasource_id, destination_schema,
			    sources, cron_expression, mode, enabled, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 RETURNING created_at, updated_at`,
			c.TenantID, c.ID, c.Name, c.DestinationDataSourceID, c.DestinationSchema,
			sourcesJSON, c.CronExpression, c.Mode, c.Enabled, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, mapWriteErr(fmt.Errorf("warehouse: create config: %w", err))
	}
	return &out, nil
}

// Update replaces the mutable fields of a config. Watermarks and
// last-run state are NOT touched here — those are owned by the export
// engine — so an operator editing a schedule never clobbers cursor
// state. Changing the source set or switching to full mode is honored
// by the next run.
func (s *ConfigStore) Update(ctx context.Context, c Config) (*Config, error) {
	if c.TenantID == uuid.Nil || c.ID == uuid.Nil {
		return nil, errors.New("warehouse: tenant id and config id required")
	}
	if err := c.Validate(); err != nil {
		return nil, invalidConfig(err)
	}
	sourcesJSON, err := json.Marshal(c.Sources)
	if err != nil {
		return nil, fmt.Errorf("warehouse: marshal sources: %w", err)
	}
	err = dbutil.WithTenantTx(ctx, s.pool, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE warehouse_sync_configs
			    SET name = $3, destination_datasource_id = $4, destination_schema = $5,
			        sources = $6, cron_expression = $7, mode = $8, enabled = $9,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			c.TenantID, c.ID, c.Name, c.DestinationDataSourceID, c.DestinationSchema,
			sourcesJSON, c.CronExpression, c.Mode, c.Enabled,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConfigNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConfigNotFound) {
			return nil, err
		}
		return nil, mapWriteErr(fmt.Errorf("warehouse: update config: %w", err))
	}
	return s.Get(ctx, c.TenantID, c.ID)
}

// Get returns a single config under tenant RLS.
func (s *ConfigStore) Get(ctx context.Context, tenantID, id uuid.UUID) (*Config, error) {
	var out *Config
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := scanConfigs(ctx, tx,
			`SELECT tenant_id, id, name, destination_datasource_id, destination_schema,
			        sources, cron_expression, mode, enabled, watermarks,
			        last_run_at, last_status, last_error, created_by, created_at, updated_at
			   FROM warehouse_sync_configs
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id)
		if err != nil {
			return err
		}
		if len(c) == 0 {
			return ErrConfigNotFound
		}
		out = &c[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// List returns every config for the tenant, newest first.
func (s *ConfigStore) List(ctx context.Context, tenantID uuid.UUID) ([]Config, error) {
	var out []Config
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := scanConfigs(ctx, tx,
			`SELECT tenant_id, id, name, destination_datasource_id, destination_schema,
			        sources, cron_expression, mode, enabled, watermarks,
			        last_run_at, last_status, last_error, created_by, created_at, updated_at
			   FROM warehouse_sync_configs
			  WHERE tenant_id = $1
			  ORDER BY created_at DESC, id`,
			tenantID)
		if err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// Delete removes a config (and, via the FK cascade, its run history).
func (s *ConfigStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM warehouse_sync_configs WHERE tenant_id = $1 AND id = $2`,
			tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConfigNotFound
		}
		return nil
	})
}

// ListDue returns the enabled configs whose next cron fire is at or
// before now. A config with no recorded last_run_at fires immediately.
//
// The enabled = TRUE predicate is pushed to SQL so a scheduler tick
// never loads (or deserializes the JSONB sources/watermarks of)
// configs a tenant has paused — across a 5000-tenant fleet ticking
// every few minutes that is the bulk of the avoidable per-tick cost.
// The cron "is it due yet" test stays in Go because the cron grammar
// is not expressible as a SQL predicate; the remaining in-Go filter
// therefore runs only over the already-active subset.
func (s *ConfigStore) ListDue(ctx context.Context, tenantID uuid.UUID, now time.Time) ([]Config, error) {
	var enabled []Config
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := scanConfigs(ctx, tx,
			`SELECT tenant_id, id, name, destination_datasource_id, destination_schema,
			        sources, cron_expression, mode, enabled, watermarks,
			        last_run_at, last_status, last_error, created_by, created_at, updated_at
			   FROM warehouse_sync_configs
			  WHERE tenant_id = $1 AND enabled = TRUE
			  ORDER BY created_at DESC, id`,
			tenantID)
		if err != nil {
			return err
		}
		enabled = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	due := make([]Config, 0, len(enabled))
	for i := range enabled {
		c := enabled[i]
		sched, err := cronParser.Parse(c.CronExpression)
		if err != nil {
			continue
		}
		if c.LastRunAt == nil {
			due = append(due, c)
			continue
		}
		if !sched.Next(*c.LastRunAt).After(now) {
			due = append(due, c)
		}
	}
	return due, nil
}

// TryLockRun acquires a session-scoped Postgres advisory lock that
// serializes runs of a single config, so a manual "run now" and a
// scheduler tick (or two ticks) never export the same config
// concurrently. Without it, two concurrent full-mode runs could each
// TRUNCATE the destination and race on the COPY, leaving the warehouse
// empty while a run still reports success.
//
// The lock is held on a dedicated pooled connection for the run's
// whole duration and auto-releases if that connection dies, so a
// crashed worker never wedges future runs — no operational cleanup or
// stale-row reaper is required. ok is false when another run already
// holds the lock; the caller should skip (scheduler) or surface
// ErrRunInProgress (API). The key is namespaced by config id; config
// UUIDs are globally unique, so no cross-config or cross-subsystem
// collision is possible.
func (s *ConfigStore) TryLockRun(ctx context.Context, configID uuid.UUID) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("warehouse: acquire run-lock conn: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended('warehouse_sync_run:' || $1::text, 0))`,
		configID,
	).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("warehouse: acquire run lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// WithoutCancel so the unlock still issues even when the run's
		// ctx was cancelled; the lock would auto-release on Release
		// anyway, but an explicit unlock returns the slot promptly.
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtextextended('warehouse_sync_run:' || $1::text, 0))`,
			configID)
		conn.Release()
	}, true, nil
}

// SaveWatermarks persists the per-source cursor map after a run, and
// records the run outcome on the config row in the SAME transaction so
// a config's denormalized last_* fields and its cursor advance
// atomically.
func (s *ConfigStore) SaveWatermarks(ctx context.Context, tenantID, id uuid.UUID, watermarks map[string]json.RawMessage, ranAt time.Time, status, errMsg string) error {
	wmJSON, err := json.Marshal(watermarks)
	if err != nil {
		return fmt.Errorf("warehouse: marshal watermarks: %w", err)
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE warehouse_sync_configs
			    SET watermarks = $3, last_run_at = $4, last_status = $5, last_error = $6,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, wmJSON, ranAt, status, errMsg)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConfigNotFound
		}
		return nil
	})
}

// scanConfigs runs sql and scans every row into a Config. The
// watermarks JSONB column decodes into the opaque per-source map.
func scanConfigs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]Config, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Config
	for rows.Next() {
		var (
			c          Config
			sourcesRaw []byte
			wmRaw      []byte
			createdBy  *uuid.UUID
			lastStatus *string
			lastError  *string
		)
		if err := rows.Scan(
			&c.TenantID, &c.ID, &c.Name, &c.DestinationDataSourceID, &c.DestinationSchema,
			&sourcesRaw, &c.CronExpression, &c.Mode, &c.Enabled, &wmRaw,
			&c.LastRunAt, &lastStatus, &lastError, &createdBy, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if len(sourcesRaw) > 0 {
			if err := json.Unmarshal(sourcesRaw, &c.Sources); err != nil {
				return nil, fmt.Errorf("warehouse: decode sources: %w", err)
			}
		}
		if len(wmRaw) > 0 {
			if err := json.Unmarshal(wmRaw, &c.Watermarks); err != nil {
				return nil, fmt.Errorf("warehouse: decode watermarks: %w", err)
			}
		}
		if c.Watermarks == nil {
			c.Watermarks = map[string]json.RawMessage{}
		}
		c.CreatedBy = createdBy
		if lastStatus != nil {
			c.LastStatus = *lastStatus
		}
		if lastError != nil {
			c.LastError = *lastError
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
