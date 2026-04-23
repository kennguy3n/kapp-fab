// Package base implements the Phase F "Base KApp" — the flexible, per-
// tenant ad-hoc tables surface. A base_table row describes a columnar
// schema as JSON (list of {name, type, required}). A base_row carries
// one row of arbitrary JSON keyed by the table's columns. All reads
// and writes run under tenant context so RLS keeps every tenant's
// tables invisible to every other tenant.
package base

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// ErrNotFound is returned when a requested table or row does not
// exist for the active tenant.
var ErrNotFound = errors.New("base: not found")

// Column describes a single column of a Base table. Type is a simple
// string tag — "text", "number", "boolean", "date", "reference", etc.
// The kernel stores row data as JSONB so the enforcement is advisory
// at the metadata layer; handlers/UIs consume this schema to render
// inputs and render cells.
type Column struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

// Table is a Base KApp table definition.
type Table struct {
	ID          uuid.UUID       `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Columns     []Column        `json:"columns"`
	SharedView  json.RawMessage `json:"shared_view,omitempty"`
	CreatedBy   uuid.UUID       `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Row is a row in a Base table. Data is arbitrary JSON; callers are
// expected to match the Table.Columns but the DB does not enforce.
type Row struct {
	ID        uuid.UUID       `json:"id"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	TableID   uuid.UUID       `json:"table_id"`
	Data      json.RawMessage `json:"data"`
	CreatedBy uuid.UUID       `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store persists Base tables and rows.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store over the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateTable inserts a new Base table definition.
func (s *Store) CreateTable(ctx context.Context, t Table) (*Table, error) {
	if t.TenantID == uuid.Nil || t.CreatedBy == uuid.Nil {
		return nil, errors.New("base: tenant and creator required")
	}
	if strings.TrimSpace(t.Slug) == "" || strings.TrimSpace(t.Name) == "" {
		return nil, errors.New("base: slug and name required")
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.Columns == nil {
		t.Columns = []Column{}
	}
	if len(t.SharedView) == 0 {
		t.SharedView = json.RawMessage("{}")
	}
	cols, err := json.Marshal(t.Columns)
	if err != nil {
		return nil, fmt.Errorf("base: marshal columns: %w", err)
	}

	err = dbutil.WithTenantTx(ctx, s.pool, t.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO base_tables
			     (id, tenant_id, slug, name, description, columns,
			      shared_view, created_by, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now(), now())
			 RETURNING created_at, updated_at`,
			t.ID, t.TenantID, t.Slug, t.Name, nullIfEmpty(t.Description),
			cols, []byte(t.SharedView), t.CreatedBy,
		).Scan(&t.CreatedAt, &t.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("base: insert table: %w", err)
	}
	return &t, nil
}

// UpdateTable replaces columns / shared view / description in place.
func (s *Store) UpdateTable(ctx context.Context, t Table) (*Table, error) {
	if t.TenantID == uuid.Nil || t.ID == uuid.Nil {
		return nil, errors.New("base: tenant and id required")
	}
	if t.Columns == nil {
		t.Columns = []Column{}
	}
	cols, err := json.Marshal(t.Columns)
	if err != nil {
		return nil, fmt.Errorf("base: marshal columns: %w", err)
	}
	if len(t.SharedView) == 0 {
		t.SharedView = json.RawMessage("{}")
	}
	err = dbutil.WithTenantTx(ctx, s.pool, t.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE base_tables
			    SET name        = $3,
			        description = $4,
			        columns     = $5,
			        shared_view = $6,
			        updated_at  = now()
			  WHERE tenant_id = $1 AND id = $2`,
			t.TenantID, t.ID, t.Name, nullIfEmpty(t.Description), cols, []byte(t.SharedView),
		)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetTable(ctx, t.TenantID, t.ID)
}

// GetTable returns a table by id.
func (s *Store) GetTable(ctx context.Context, tenantID, id uuid.UUID) (*Table, error) {
	var t Table
	var cols []byte
	var sv []byte
	var desc *string
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, slug, name, description, columns,
			        shared_view, created_by, created_at, updated_at
			   FROM base_tables
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(&t.ID, &t.TenantID, &t.Slug, &t.Name, &desc, &cols,
			&sv, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if desc != nil {
		t.Description = *desc
	}
	if len(cols) > 0 {
		_ = json.Unmarshal(cols, &t.Columns)
	}
	if len(sv) > 0 {
		t.SharedView = json.RawMessage(sv)
	}
	return &t, nil
}

// ListTables returns every Base table visible to the tenant.
func (s *Store) ListTables(ctx context.Context, tenantID uuid.UUID) ([]Table, error) {
	var out []Table
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, slug, name, description, columns,
			        shared_view, created_by, created_at, updated_at
			   FROM base_tables
			  WHERE tenant_id = $1
			  ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Table
			var cols []byte
			var sv []byte
			var desc *string
			if err := rows.Scan(&t.ID, &t.TenantID, &t.Slug, &t.Name, &desc, &cols,
				&sv, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
				return err
			}
			if desc != nil {
				t.Description = *desc
			}
			if len(cols) > 0 {
				_ = json.Unmarshal(cols, &t.Columns)
			}
			if len(sv) > 0 {
				t.SharedView = json.RawMessage(sv)
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateRow inserts a row into a Base table.
func (s *Store) CreateRow(ctx context.Context, row Row) (*Row, error) {
	if row.TenantID == uuid.Nil || row.TableID == uuid.Nil || row.CreatedBy == uuid.Nil {
		return nil, errors.New("base: tenant, table, creator required")
	}
	if row.ID == uuid.Nil {
		row.ID = uuid.New()
	}
	if len(row.Data) == 0 {
		row.Data = json.RawMessage("{}")
	}
	err := dbutil.WithTenantTx(ctx, s.pool, row.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Confirm the table exists under the same tenant — otherwise
		// a caller can smuggle rows under a non-existent table id
		// and confuse downstream listings.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM base_tables WHERE tenant_id = $1 AND id = $2)`,
			row.TenantID, row.TableID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return tx.QueryRow(ctx,
			`INSERT INTO base_rows
			     (id, tenant_id, table_id, data, created_by, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5, now(), now())
			 RETURNING created_at, updated_at`,
			row.ID, row.TenantID, row.TableID, []byte(row.Data), row.CreatedBy,
		).Scan(&row.CreatedAt, &row.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// UpdateRow replaces a row's data. Handlers are responsible for
// shape-checking against the table columns.
func (s *Store) UpdateRow(ctx context.Context, row Row) (*Row, error) {
	if row.TenantID == uuid.Nil || row.ID == uuid.Nil {
		return nil, errors.New("base: tenant and id required")
	}
	if len(row.Data) == 0 {
		row.Data = json.RawMessage("{}")
	}
	err := dbutil.WithTenantTx(ctx, s.pool, row.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE base_rows
			    SET data       = $3,
			        updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			row.TenantID, row.ID, []byte(row.Data),
		)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetRow(ctx, row.TenantID, row.ID)
}

// GetRow returns a row by id.
func (s *Store) GetRow(ctx context.Context, tenantID, id uuid.UUID) (*Row, error) {
	var r Row
	var data []byte
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, table_id, data, created_by, created_at, updated_at
			   FROM base_rows
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(&r.ID, &r.TenantID, &r.TableID, &data, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(data) > 0 {
		r.Data = json.RawMessage(data)
	}
	return &r, nil
}

// ListRows returns rows for the table, ordered by most-recently updated.
func (s *Store) ListRows(ctx context.Context, tenantID, tableID uuid.UUID, limit, offset int) ([]Row, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var out []Row
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, table_id, data, created_by, created_at, updated_at
			   FROM base_rows
			  WHERE tenant_id = $1 AND table_id = $2
			  ORDER BY updated_at DESC
			  LIMIT $3 OFFSET $4`,
			tenantID, tableID, limit, offset,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Row
			var data []byte
			if err := rows.Scan(&r.ID, &r.TenantID, &r.TableID, &data,
				&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
				return err
			}
			if len(data) > 0 {
				r.Data = json.RawMessage(data)
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

// DeleteRow removes a row under tenant context.
func (s *Store) DeleteRow(ctx context.Context, tenantID, id uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`DELETE FROM base_rows WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
