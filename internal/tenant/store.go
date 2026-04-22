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
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tenants (id, slug, name, cell, status, plan, quota)
		 VALUES ($1, $2, $3, $4, 'active', $5, $6)
		 RETURNING id, slug, name, cell, status, plan, quota, created_at, updated_at`,
		id, input.Slug, input.Name, input.Cell, input.Plan, quota,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, ErrSlugTaken
		}
		return nil, fmt.Errorf("tenant: insert: %w", err)
	}
	return &t, nil
}

// Get returns the tenant with the given id or ErrNotFound.
func (s *PGStore) Get(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	var t Tenant
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at
		 FROM tenants WHERE id = $1`, id,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get: %w", err)
	}
	return &t, nil
}

// GetBySlug returns the tenant with the given slug or ErrNotFound.
func (s *PGStore) GetBySlug(ctx context.Context, slug string) (*Tenant, error) {
	var t Tenant
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at
		 FROM tenants WHERE slug = $1`, slug,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get by slug: %w", err)
	}
	return &t, nil
}

// List returns all tenants ordered by slug. Intended for control-plane
// admin tooling; no filtering is applied.
func (s *PGStore) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at
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
		if err := rows.Scan(
			&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status,
			&t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("tenant: list scan: %w", err)
		}
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
