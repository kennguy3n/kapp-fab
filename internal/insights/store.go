package insights

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

// Sentinel errors surfaced to API callers.
var (
	ErrQueryNotFound     = errors.New("insights: query not found")
	ErrDashboardNotFound = errors.New("insights: dashboard not found")
	ErrWidgetNotFound    = errors.New("insights: dashboard widget not found")
	ErrShareNotFound     = errors.New("insights: share not found")
	// ErrValidation tags errors caused by invalid client input so the
	// HTTP layer can surface them as 400 Bad Request without resorting
	// to message-string matching. Wrap with fmt.Errorf("%w: …", or use
	// validationErr below.
	ErrValidation = errors.New("insights: validation")
)

// validationErr wraps a free-form message as an ErrValidation-tagged
// error. Stores call this in place of errors.New so writeInsightsError
// can map every user-input failure to 400 with one errors.Is check.
func validationErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrValidation}, args...)...)
}

// Default cache TTL applied when callers omit the field on Create.
// 5 minutes mirrors a sensible BI default that keeps cache hit rate
// high without serving truly stale data on hourly-changing sources.
const DefaultCacheTTLSeconds = 300

// resolveCacheTTL collapses the *int request value into the int
// stored on the row. nil means "field omitted → use the server
// default"; a non-nil 0 means "disable caching for this query"
// (honoured downstream by CacheStore.Set / Runner.Run); a negative
// value is rejected so storage stays in sync with the column's
// CHECK constraint.
func resolveCacheTTL(ttl *int) (int, error) {
	if ttl == nil {
		return DefaultCacheTTLSeconds, nil
	}
	if *ttl < 0 {
		return 0, validationErr("cache_ttl_seconds must be >= 0")
	}
	return *ttl, nil
}

// QueryStore persists insights_queries rows.
type QueryStore struct {
	pool *pgxpool.Pool
}

// NewQueryStore wires the store from the shared pool.
func NewQueryStore(pool *pgxpool.Pool) *QueryStore {
	return &QueryStore{pool: pool}
}

// Create inserts a new saved query. Name uniqueness per tenant is
// enforced by the insights_queries UNIQUE index.
func (s *QueryStore) Create(ctx context.Context, q Query) (*Query, error) {
	if q.TenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	if q.Name == "" {
		return nil, validationErr("query name required")
	}
	mode, rawSQL, err := normalizeMode(q)
	if err != nil {
		return nil, err
	}
	q.Mode = mode
	q.RawSQL = rawSQL
	if mode == QueryModeVisual {
		if err := q.Definition.Validate(); err != nil {
			return nil, err
		}
	}
	if q.ID == uuid.Nil {
		q.ID = uuid.New()
	}
	ttl, err := resolveCacheTTL(q.CacheTTLSeconds)
	if err != nil {
		return nil, err
	}
	q.CacheTTLSeconds = &ttl
	def, err := json.Marshal(q.Definition)
	if err != nil {
		return nil, fmt.Errorf("insights: marshal definition: %w", err)
	}
	out := q
	err = dbutil.WithTenantTx(ctx, s.pool, q.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if q.CreatedBy != nil {
			createdBy = *q.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO insights_queries
			   (tenant_id, id, name, description, definition, cache_ttl_seconds, created_by, mode, raw_sql)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 RETURNING created_at, updated_at`,
			q.TenantID, q.ID, q.Name, q.Description, def, ttl, createdBy, mode, rawSQL,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insights: create query: %w", err)
	}
	return &out, nil
}

// normalizeMode resolves the (Mode, RawSQL) inputs into a sane
// (mode, rawSQL) pair that satisfies the table-level CHECKs in
// migrations/000045_insights_sql_mode.sql:
//
//   - Empty Mode -> QueryModeVisual (back-compat with Phase L
//     callers that don't know about the field).
//   - QueryModeVisual must have empty RawSQL.
//   - QueryModeSQL must carry a non-empty RawSQL body.
//
// The DB CHECK is the source of truth; this helper exists so the
// API surface returns a 400 with a structured message instead of a
// confusing constraint-violation envelope from the driver.
func normalizeMode(q Query) (string, string, error) {
	mode := q.Mode
	if mode == "" {
		mode = QueryModeVisual
	}
	switch mode {
	case QueryModeVisual:
		if q.RawSQL != "" {
			return "", "", validationErr("visual queries must not carry raw_sql")
		}
		return QueryModeVisual, "", nil
	case QueryModeSQL:
		if q.RawSQL == "" {
			return "", "", validationErr("sql-mode queries require a non-empty raw_sql body")
		}
		return QueryModeSQL, q.RawSQL, nil
	default:
		return "", "", validationErr("unknown query mode %q", mode)
	}
}

// Update replaces a query's name, description, definition, and TTL.
// CreatedBy / CreatedAt are owned by Create; this handler never
// rewrites them.
func (s *QueryStore) Update(ctx context.Context, q Query) (*Query, error) {
	if q.TenantID == uuid.Nil || q.ID == uuid.Nil {
		return nil, validationErr("tenant id and query id required")
	}
	if q.Name == "" {
		return nil, validationErr("query name required")
	}
	mode, rawSQL, err := normalizeMode(q)
	if err != nil {
		return nil, err
	}
	if mode == QueryModeVisual {
		if err := q.Definition.Validate(); err != nil {
			return nil, err
		}
	}
	def, err := json.Marshal(q.Definition)
	if err != nil {
		return nil, err
	}
	ttl, err := resolveCacheTTL(q.CacheTTLSeconds)
	if err != nil {
		return nil, err
	}
	err = dbutil.WithTenantTx(ctx, s.pool, q.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE insights_queries
			    SET name = $3, description = $4, definition = $5,
			        cache_ttl_seconds = $6, mode = $7, raw_sql = $8, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			q.TenantID, q.ID, q.Name, q.Description, def, ttl, mode, rawSQL,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrQueryNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, q.TenantID, q.ID)
}

// Get loads a single query or returns ErrQueryNotFound.
func (s *QueryStore) Get(ctx context.Context, tenantID, id uuid.UUID) (*Query, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, validationErr("tenant id and query id required")
	}
	var (
		out Query
		def []byte
	)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			createdBy *uuid.UUID
			ttl       int
		)
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, id, name, description, definition, cache_ttl_seconds,
			        created_by, created_at, updated_at, mode, raw_sql
			   FROM insights_queries WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err := row.Scan(
			&out.TenantID, &out.ID, &out.Name, &out.Description, &def, &ttl,
			&createdBy, &out.CreatedAt, &out.UpdatedAt, &out.Mode, &out.RawSQL,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrQueryNotFound
			}
			return err
		}
		out.CreatedBy = createdBy
		out.CacheTTLSeconds = &ttl
		return json.Unmarshal(def, &out.Definition)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every saved query for the tenant, ordered by name.
func (s *QueryStore) List(ctx context.Context, tenantID uuid.UUID) ([]Query, error) {
	if tenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	out := make([]Query, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, name, description, definition, cache_ttl_seconds,
			        created_by, created_at, updated_at, mode, raw_sql
			   FROM insights_queries WHERE tenant_id = $1 ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				q         Query
				def       []byte
				createdBy *uuid.UUID
				ttl       int
			)
			if err := rows.Scan(
				&q.TenantID, &q.ID, &q.Name, &q.Description, &def, &ttl,
				&createdBy, &q.CreatedAt, &q.UpdatedAt, &q.Mode, &q.RawSQL,
			); err != nil {
				return err
			}
			q.CreatedBy = createdBy
			q.CacheTTLSeconds = &ttl
			if err := json.Unmarshal(def, &q.Definition); err != nil {
				return err
			}
			out = append(out, q)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes a query. Returns ErrQueryNotFound if the id is
// unknown. Cache rows referencing the query are NOT cascaded — they
// expire naturally; the explicit cache invalidate path lives on
// CacheStore.InvalidateQuery for callers that need an immediate
// purge.
func (s *QueryStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return validationErr("tenant id and query id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM insights_queries WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrQueryNotFound
		}
		return nil
	})
}

// DashboardStore persists insights_dashboards + insights_dashboard_widgets.
type DashboardStore struct {
	pool *pgxpool.Pool
}

// NewDashboardStore wires the store from the shared pool.
func NewDashboardStore(pool *pgxpool.Pool) *DashboardStore {
	return &DashboardStore{pool: pool}
}

// Create inserts a new dashboard. Widget rows are written by separate
// upsert calls so the caller can mutate widgets independently of the
// outer dashboard layout.
func (s *DashboardStore) Create(ctx context.Context, d Dashboard) (*Dashboard, error) {
	if d.TenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	if d.Name == "" {
		return nil, validationErr("dashboard name required")
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if len(d.Layout) == 0 {
		d.Layout = json.RawMessage(`{}`)
	}
	if d.AutoRefreshSeconds < 0 {
		return nil, validationErr("auto_refresh_seconds must be >= 0")
	}
	out := d
	err := dbutil.WithTenantTx(ctx, s.pool, d.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if d.CreatedBy != nil {
			createdBy = *d.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO insights_dashboards
			   (tenant_id, id, name, description, layout, auto_refresh_seconds, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING created_at, updated_at`,
			d.TenantID, d.ID, d.Name, d.Description, []byte(d.Layout),
			d.AutoRefreshSeconds, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insights: create dashboard: %w", err)
	}
	return &out, nil
}

// Update replaces the dashboard's name, description, layout, and
// auto-refresh interval.
func (s *DashboardStore) Update(ctx context.Context, d Dashboard) (*Dashboard, error) {
	if d.TenantID == uuid.Nil || d.ID == uuid.Nil {
		return nil, validationErr("tenant id and dashboard id required")
	}
	if d.Name == "" {
		return nil, validationErr("dashboard name required")
	}
	if d.AutoRefreshSeconds < 0 {
		return nil, validationErr("auto_refresh_seconds must be >= 0")
	}
	if len(d.Layout) == 0 {
		d.Layout = json.RawMessage(`{}`)
	}
	err := dbutil.WithTenantTx(ctx, s.pool, d.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE insights_dashboards
			    SET name = $3, description = $4, layout = $5,
			        auto_refresh_seconds = $6, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			d.TenantID, d.ID, d.Name, d.Description, []byte(d.Layout), d.AutoRefreshSeconds,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDashboardNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, d.TenantID, d.ID)
}

// Get loads the dashboard by id. The widgets slice is populated by a
// follow-up call to ListWidgets so callers that just need the layout
// avoid the extra round-trip.
func (s *DashboardStore) Get(ctx context.Context, tenantID, id uuid.UUID) (*Dashboard, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, validationErr("tenant id and dashboard id required")
	}
	var (
		out    Dashboard
		layout []byte
	)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy *uuid.UUID
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, id, name, description, layout, auto_refresh_seconds,
			        created_by, created_at, updated_at
			   FROM insights_dashboards WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err := row.Scan(
			&out.TenantID, &out.ID, &out.Name, &out.Description, &layout, &out.AutoRefreshSeconds,
			&createdBy, &out.CreatedAt, &out.UpdatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrDashboardNotFound
			}
			return err
		}
		out.CreatedBy = createdBy
		out.Layout = json.RawMessage(layout)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every dashboard for the tenant.
func (s *DashboardStore) List(ctx context.Context, tenantID uuid.UUID) ([]Dashboard, error) {
	if tenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	out := make([]Dashboard, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, name, description, layout, auto_refresh_seconds,
			        created_by, created_at, updated_at
			   FROM insights_dashboards WHERE tenant_id = $1 ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				d         Dashboard
				layout    []byte
				createdBy *uuid.UUID
			)
			if err := rows.Scan(
				&d.TenantID, &d.ID, &d.Name, &d.Description, &layout, &d.AutoRefreshSeconds,
				&createdBy, &d.CreatedAt, &d.UpdatedAt,
			); err != nil {
				return err
			}
			d.CreatedBy = createdBy
			d.Layout = json.RawMessage(layout)
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes the dashboard and cascades into its widgets.
func (s *DashboardStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return validationErr("tenant id and dashboard id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM insights_dashboard_widgets
			  WHERE tenant_id = $1 AND dashboard_id = $2`,
			tenantID, id,
		); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM insights_dashboards WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDashboardNotFound
		}
		return nil
	})
}

// UpsertWidget inserts or replaces one widget. Position + Config are
// JSONB and round-trip as raw bytes so the caller controls the shape.
func (s *DashboardStore) UpsertWidget(ctx context.Context, w DashboardWidget) (*DashboardWidget, error) {
	if w.TenantID == uuid.Nil || w.DashboardID == uuid.Nil {
		return nil, validationErr("tenant id and dashboard id required")
	}
	if err := ValidateVizType(w.VizType); err != nil {
		return nil, err
	}
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	if len(w.Position) == 0 {
		w.Position = json.RawMessage(`{}`)
	}
	if len(w.Config) == 0 {
		w.Config = json.RawMessage(`{}`)
	}
	out := w
	err := dbutil.WithTenantTx(ctx, s.pool, w.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var queryID any
		if w.QueryID != nil {
			queryID = *w.QueryID
		}
		return tx.QueryRow(ctx,
			`INSERT INTO insights_dashboard_widgets
			   (tenant_id, id, dashboard_id, query_id, viz_type, position, config)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, id) DO UPDATE
			   SET dashboard_id = EXCLUDED.dashboard_id,
			       query_id     = EXCLUDED.query_id,
			       viz_type     = EXCLUDED.viz_type,
			       position     = EXCLUDED.position,
			       config       = EXCLUDED.config,
			       updated_at   = now()
			 RETURNING created_at, updated_at`,
			w.TenantID, w.ID, w.DashboardID, queryID, w.VizType,
			[]byte(w.Position), []byte(w.Config),
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insights: upsert widget: %w", err)
	}
	return &out, nil
}

// ListWidgets returns every widget for one dashboard.
func (s *DashboardStore) ListWidgets(ctx context.Context, tenantID, dashboardID uuid.UUID) ([]DashboardWidget, error) {
	if tenantID == uuid.Nil || dashboardID == uuid.Nil {
		return nil, validationErr("tenant id and dashboard id required")
	}
	out := make([]DashboardWidget, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, dashboard_id, query_id, viz_type, position, config,
			        created_at, updated_at
			   FROM insights_dashboard_widgets
			  WHERE tenant_id = $1 AND dashboard_id = $2
			  ORDER BY created_at`,
			tenantID, dashboardID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				w        DashboardWidget
				queryID  *uuid.UUID
				position []byte
				config   []byte
			)
			if err := rows.Scan(
				&w.TenantID, &w.ID, &w.DashboardID, &queryID, &w.VizType,
				&position, &config, &w.CreatedAt, &w.UpdatedAt,
			); err != nil {
				return err
			}
			w.QueryID = queryID
			w.Position = json.RawMessage(position)
			w.Config = json.RawMessage(config)
			out = append(out, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteWidget removes a single widget from a dashboard.
func (s *DashboardStore) DeleteWidget(ctx context.Context, tenantID, dashboardID, id uuid.UUID) error {
	if tenantID == uuid.Nil || dashboardID == uuid.Nil || id == uuid.Nil {
		return validationErr("tenant id, dashboard id, and widget id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM insights_dashboard_widgets
			  WHERE tenant_id = $1 AND dashboard_id = $2 AND id = $3`,
			tenantID, dashboardID, id,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrWidgetNotFound
		}
		return nil
	})
}

// CreateShare inserts a sharing grant on a query or dashboard.
func (s *DashboardStore) CreateShare(ctx context.Context, share Share) (*Share, error) {
	if share.TenantID == uuid.Nil || share.ResourceID == uuid.Nil {
		return nil, validationErr("tenant id and resource id required")
	}
	if err := ValidateResourceType(share.ResourceType); err != nil {
		return nil, err
	}
	if err := ValidateGranteeType(share.GranteeType); err != nil {
		return nil, err
	}
	if share.Permission == "" {
		share.Permission = PermissionView
	}
	if err := ValidatePermission(share.Permission); err != nil {
		return nil, err
	}
	if share.Grantee == "" {
		return nil, validationErr("grantee required")
	}
	if share.ID == uuid.Nil {
		share.ID = uuid.New()
	}
	out := share
	err := dbutil.WithTenantTx(ctx, s.pool, share.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO insights_shares
			   (tenant_id, id, resource_type, resource_id, grantee_type, grantee, permission)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, resource_type, resource_id, grantee_type, grantee)
			 DO UPDATE SET permission = EXCLUDED.permission
			 RETURNING id, created_at`,
			share.TenantID, share.ID, share.ResourceType, share.ResourceID,
			share.GranteeType, share.Grantee, share.Permission,
		).Scan(&out.ID, &out.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insights: create share: %w", err)
	}
	return &out, nil
}

// ListShares returns every share for one resource.
func (s *DashboardStore) ListShares(ctx context.Context, tenantID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]Share, error) {
	if err := ValidateResourceType(resourceType); err != nil {
		return nil, err
	}
	if tenantID == uuid.Nil || resourceID == uuid.Nil {
		return nil, validationErr("tenant id and resource id required")
	}
	out := make([]Share, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, resource_type, resource_id, grantee_type, grantee,
			        permission, created_at
			   FROM insights_shares
			  WHERE tenant_id = $1 AND resource_type = $2 AND resource_id = $3
			  ORDER BY created_at`,
			tenantID, resourceType, resourceID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sh Share
			if err := rows.Scan(
				&sh.TenantID, &sh.ID, &sh.ResourceType, &sh.ResourceID,
				&sh.GranteeType, &sh.Grantee, &sh.Permission, &sh.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, sh)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteShare removes a share row by id, scoped to the parent
// resource. Both resourceType and resourceID must match the row
// or it is treated as not-found, so a share for query A cannot be
// deleted via DELETE /dashboards/{B}/shares/{share-id}. Without
// this scope check the URL parents are advisory only and a caller
// that knows any share id could remove it through any parent route.
func (s *DashboardStore) DeleteShare(ctx context.Context, tenantID uuid.UUID, resourceType string, resourceID, shareID uuid.UUID) error {
	if tenantID == uuid.Nil || shareID == uuid.Nil || resourceID == uuid.Nil {
		return validationErr("tenant id, resource id and share id required")
	}
	if err := ValidateResourceType(resourceType); err != nil {
		return err
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM insights_shares
			   WHERE tenant_id = $1
			     AND id = $2
			     AND resource_type = $3
			     AND resource_id = $4`,
			tenantID, shareID, resourceType, resourceID,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrShareNotFound
		}
		return nil
	})
}

// timeNow is captured here so tests can inject a clock in the future
// without rewriting the store. Today it just delegates to time.Now.
//
//nolint:unused // retained for future test injection
var timeNow = func() time.Time { return time.Now().UTC() }
