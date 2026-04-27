package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors returned by PGStore. Callers map these to HTTP status codes
// in transport layers (e.g. NotFound → 404, SlugTaken → 409, InvalidTransition
// → 409).
var (
	ErrNotFound          = errors.New("tenant: not found")
	ErrSlugTaken         = errors.New("tenant: slug already taken")
	ErrInvalidTransition = errors.New("tenant: invalid status transition")
)

// PGStore is the PostgreSQL-backed implementation of Service. It operates on
// the control-plane `tenants` table, which is NOT tenant-scoped and does not
// have RLS — so these operations run against the shared pool without needing
// `SET LOCAL app.tenant_id`.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a PGStore backed by the supplied pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const (
	pgUniqueViolation = "23505"
)

// Create inserts a new tenant row. Slug uniqueness is enforced by the
// database; a conflict is translated to ErrSlugTaken.
func (s *PGStore) Create(ctx context.Context, input CreateInput) (*Tenant, error) {
	if input.Slug == "" || input.Name == "" || input.Cell == "" || input.Plan == "" {
		return nil, errors.New("tenant: slug, name, cell, and plan are required")
	}
	id := uuid.New()
	quota := input.Quota
	if len(quota) == 0 {
		quota = []byte("{}")
	}

	var t Tenant
	var zkAccess, zkSecret, zkBucket *string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tenants (id, slug, name, cell, status, plan, quota)
		 VALUES ($1, $2, $3, $4, 'active', $5, $6)
		 RETURNING id, slug, name, cell, status, plan, quota, created_at, updated_at,
		           zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD')`,
		id, input.Slug, input.Name, input.Cell, input.Plan, quota,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
		&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, ErrSlugTaken
		}
		return nil, fmt.Errorf("tenant: insert: %w", err)
	}
	assignZK(&t, zkAccess, zkSecret, zkBucket)
	return &t, nil
}

// Get returns the tenant with the given id or ErrNotFound.
func (s *PGStore) Get(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	var t Tenant
	var zkAccess, zkSecret, zkBucket *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at,
		        zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD')
		 FROM tenants WHERE id = $1`, id,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
		&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get: %w", err)
	}
	assignZK(&t, zkAccess, zkSecret, zkBucket)
	return &t, nil
}

// Timezone returns the tenant's IANA timezone identifier (e.g.
// "America/New_York") used to interpret wall-clock fields such as
// hr.shift_type.start_time. The column is backfilled to "UTC" by
// migration 000047, so a never-configured tenant always resolves
// to UTC rather than NULL or empty. ErrNotFound is returned when
// the tenant id doesn't exist.
func (s *PGStore) Timezone(ctx context.Context, id uuid.UUID) (string, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(timezone, 'UTC') FROM tenants WHERE id = $1`, id).Scan(&tz)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("tenant: timezone: %w", err)
	}
	if tz == "" {
		tz = "UTC"
	}
	return tz, nil
}

// GetBySlug returns the tenant with the given slug or ErrNotFound.
func (s *PGStore) GetBySlug(ctx context.Context, slug string) (*Tenant, error) {
	var t Tenant
	var zkAccess, zkSecret, zkBucket *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at,
		        zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD')
		 FROM tenants WHERE slug = $1`, slug,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
		&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get by slug: %w", err)
	}
	assignZK(&t, zkAccess, zkSecret, zkBucket)
	return &t, nil
}

// List returns all tenants ordered by slug. Intended for control-plane
// admin tooling; no filtering is applied.
func (s *PGStore) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at,
		        zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD')
		 FROM tenants
		 ORDER BY slug ASC`)
	if err != nil {
		return nil, fmt.Errorf("tenant: list: %w", err)
	}
	defer rows.Close()

	// Preallocate an empty (non-nil) slice so the JSON response is `[]`
	// rather than `null` when no rows exist.
	out := make([]Tenant, 0)
	for rows.Next() {
		var t Tenant
		var zkAccess, zkSecret, zkBucket *string
		if err := rows.Scan(
			&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status,
			&t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
			&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency,
		); err != nil {
			return nil, fmt.Errorf("tenant: list scan: %w", err)
		}
		assignZK(&t, zkAccess, zkSecret, zkBucket)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenant: list rows: %w", err)
	}
	return out, nil
}

// Suspend transitions active → suspended.
func (s *PGStore) Suspend(ctx context.Context, id uuid.UUID) error {
	return s.transition(ctx, id, StatusActive, StatusSuspended)
}

// Activate transitions suspended → active. Use this to un-suspend a tenant.
func (s *PGStore) Activate(ctx context.Context, id uuid.UUID) error {
	return s.transition(ctx, id, StatusSuspended, StatusActive)
}

// Archive transitions suspended → archived.
func (s *PGStore) Archive(ctx context.Context, id uuid.UUID) error {
	return s.transition(ctx, id, StatusSuspended, StatusArchived)
}

// Delete transitions any non-deleting state → deleting. Actual purge is async.
func (s *PGStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET status = $1, updated_at = now()
		 WHERE id = $2 AND status <> $1`,
		StatusDeleting, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the tenant does not exist or it is already deleting; check
		// which so callers get the right error.
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrInvalidTransition
	}
	return nil
}

// UpdatePlan atomically updates the tenant's plan name and quota
// JSON. Used by the /tenants/{id}/plan endpoint. Returns ErrNotFound
// when no row matches.
func (s *PGStore) UpdatePlan(ctx context.Context, id uuid.UUID, plan string, quota []byte) error {
	if plan == "" {
		return errors.New("tenant: plan required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET plan = $1, quota = $2, updated_at = now()
		 WHERE id = $3`,
		plan, quota, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: update plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) transition(ctx context.Context, id uuid.UUID, from, to Status) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET status = $1, updated_at = now()
		 WHERE id = $2 AND status = $3`,
		to, id, from,
	)
	if err != nil {
		return fmt.Errorf("tenant: transition: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrInvalidTransition
	}
	return nil
}

// assignZK copies the nullable ZK columns onto the Tenant struct.
// Pointers come straight off pgx.Scan and may be nil for tenants
// created before migration 000027 ran.
func assignZK(t *Tenant, access, secret, bucket *string) {
	if access != nil {
		t.ZKAccessKey = *access
	}
	if secret != nil {
		t.ZKSecretKey = *secret
	}
	if bucket != nil {
		t.ZKBucket = *bucket
	}
}

// SetBaseCurrency updates the tenant's functional currency. Called by
// the wizard once at setup time and by the admin tenant-edit form.
// The value must be a 3-letter ISO-4217 code; the column has a CHECK
// of length 3 in migration 000029.
func (s *PGStore) SetBaseCurrency(ctx context.Context, id uuid.UUID, code string) error {
	if len(code) != 3 {
		return errors.New("tenant: base_currency must be a 3-letter ISO-4217 code")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET base_currency = $1, updated_at = now() WHERE id = $2`,
		code, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set base currency: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetZKCredentials persists the per-tenant ZK Object Fabric HMAC
// credentials on the tenants row. Called by the setup wizard after
// it provisions the tenant on the ZK fabric console at :8081 (and
// by integration tests that pre-seed credentials directly). Returns
// ErrNotFound when the tenant id does not exist.
func (s *PGStore) SetZKCredentials(ctx context.Context, id uuid.UUID, accessKey, secretKey, bucket string) error {
	if accessKey == "" || secretKey == "" || bucket == "" {
		return errors.New("tenant: zk access_key, secret_key, and bucket are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants
		    SET zk_access_key = $1,
		        zk_secret_key = $2,
		        zk_bucket     = $3,
		        updated_at    = now()
		  WHERE id = $4`,
		accessKey, secretKey, bucket, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set zk credentials: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
