package reporting

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

// SavedReport mirrors the saved_reports table row. Definition is
// stored as JSONB; the caller round-trips the strongly-typed
// Definition so the JSON → struct conversion happens in one place.
type SavedReport struct {
	TenantID    uuid.UUID  `json:"tenant_id"`
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Definition  Definition `json:"definition"`
	CreatedBy   *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Store persists saved report definitions.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Sentinel errors surfaced to API callers.
var (
	ErrReportNotFound = errors.New("reporting: saved report not found")
)

// Create inserts a new saved report. Name uniqueness per tenant is
// enforced by the saved_reports table's UNIQUE index.
func (s *Store) Create(ctx context.Context, r SavedReport) (*SavedReport, error) {
	if r.TenantID == uuid.Nil {
		return nil, errors.New("reporting: tenant id required")
	}
	if r.Name == "" {
		return nil, errors.New("reporting: name required")
	}
	if err := r.Definition.Validate(); err != nil {
		return nil, err
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	def, err := json.Marshal(r.Definition)
	if err != nil {
		return nil, fmt.Errorf("reporting: marshal definition: %w", err)
	}
	out := r
	err = dbutil.WithTenantTx(ctx, s.pool, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if r.CreatedBy != nil {
			createdBy = *r.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO saved_reports (tenant_id, id, name, description, definition, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING created_at, updated_at`,
			r.TenantID, r.ID, r.Name, r.Description, def, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("reporting: create: %w", err)
	}
	return &out, nil
}

// Update replaces a report's name, description, and definition.
func (s *Store) Update(ctx context.Context, r SavedReport) (*SavedReport, error) {
	if r.TenantID == uuid.Nil || r.ID == uuid.Nil {
		return nil, errors.New("reporting: tenant id and report id required")
	}
	if err := r.Definition.Validate(); err != nil {
		return nil, err
	}
	def, err := json.Marshal(r.Definition)
	if err != nil {
		return nil, err
	}
	out := r
	err = dbutil.WithTenantTx(ctx, s.pool, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE saved_reports
			 SET name = $3, description = $4, definition = $5, updated_at = now()
			 WHERE tenant_id = $1 AND id = $2`,
			r.TenantID, r.ID, r.Name, r.Description, def,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrReportNotFound
		}
		return tx.QueryRow(ctx,
			`SELECT created_at, updated_at FROM saved_reports WHERE tenant_id = $1 AND id = $2`,
			r.TenantID, r.ID,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Get loads a single report or returns ErrReportNotFound.
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (*SavedReport, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("reporting: tenant id and report id required")
	}
	var out SavedReport
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var def []byte
		var createdBy *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, name, description, definition, created_by, created_at, updated_at
			 FROM saved_reports WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&out.TenantID, &out.ID, &out.Name, &out.Description, &def,
			&createdBy, &out.CreatedAt, &out.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrReportNotFound
			}
			return err
		}
		out.CreatedBy = createdBy
		return json.Unmarshal(def, &out.Definition)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every report for a tenant, ordered by name.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID) ([]SavedReport, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("reporting: tenant id required")
	}
	out := make([]SavedReport, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, name, description, definition, created_by, created_at, updated_at
			 FROM saved_reports WHERE tenant_id = $1 ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r SavedReport
			var def []byte
			var createdBy *uuid.UUID
			if err := rows.Scan(
				&r.TenantID, &r.ID, &r.Name, &r.Description, &def,
				&createdBy, &r.CreatedAt, &r.UpdatedAt,
			); err != nil {
				return err
			}
			r.CreatedBy = createdBy
			if err := json.Unmarshal(def, &r.Definition); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes a report. Returns ErrReportNotFound if the id is
// unknown so the API layer can emit 404 rather than 204 on no-op.
func (s *Store) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return errors.New("reporting: tenant id and report id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM saved_reports WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrReportNotFound
		}
		return nil
	})
}
