package print

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

// Template mirrors one row of print_templates.
type Template struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	KType        string    `json:"ktype"`
	Name         string    `json:"name"`
	HTMLTemplate string    `json:"html_template"`
	IsDefault    bool      `json:"is_default"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ErrTemplateNotFound signals the row is missing for this
// (tenant, ktype). Callers treat it as a cue to fall back to the
// embedded default template.
var ErrTemplateNotFound = errors.New("print: template not found")

// TemplateStore persists per-tenant print template overrides.
type TemplateStore struct {
	pool *pgxpool.Pool
}

// NewTemplateStore binds a store to the shared pool.
func NewTemplateStore(pool *pgxpool.Pool) *TemplateStore {
	return &TemplateStore{pool: pool}
}

// GetDefault returns the row flagged is_default for this KType,
// or ErrTemplateNotFound when no override exists.
func (s *TemplateStore) GetDefault(ctx context.Context, tenantID uuid.UUID, ktypeName string) (*Template, error) {
	var t Template
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, tenant_id, ktype, name, html_template, is_default, created_at, updated_at
			  FROM print_templates
			 WHERE tenant_id = $1 AND ktype = $2 AND is_default = TRUE
			 LIMIT 1`,
			tenantID, ktypeName,
		).Scan(
			&t.ID, &t.TenantID, &t.KType, &t.Name, &t.HTMLTemplate,
			&t.IsDefault, &t.CreatedAt, &t.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTemplateNotFound
		}
		return nil, fmt.Errorf("print: load default template: %w", err)
	}
	return &t, nil
}

// List returns every print template registered for the tenant,
// ordered by ktype then name.
func (s *TemplateStore) List(ctx context.Context, tenantID uuid.UUID) ([]Template, error) {
	out := make([]Template, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, ktype, name, html_template, is_default, created_at, updated_at
			  FROM print_templates
			 WHERE tenant_id = $1
			 ORDER BY ktype, name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Template
			if err := rows.Scan(
				&t.ID, &t.TenantID, &t.KType, &t.Name, &t.HTMLTemplate,
				&t.IsDefault, &t.CreatedAt, &t.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("print: list templates: %w", err)
	}
	return out, nil
}

// CreateInput is the payload accepted by Create.
type CreateInput struct {
	KType        string `json:"ktype"`
	Name         string `json:"name"`
	HTMLTemplate string `json:"html_template"`
	IsDefault    bool   `json:"is_default"`
}

// Create inserts a new print template. When IsDefault is true the
// insert is preceded by a `UPDATE ... SET is_default=FALSE` over
// every row with the same ktype so the partial-unique index holds.
func (s *TemplateStore) Create(ctx context.Context, tenantID uuid.UUID, in CreateInput) (*Template, error) {
	if in.KType == "" || in.Name == "" || in.HTMLTemplate == "" {
		return nil, errors.New("print: ktype, name, html_template required")
	}
	t := Template{
		ID:           uuid.New(),
		TenantID:     tenantID,
		KType:        in.KType,
		Name:         in.Name,
		HTMLTemplate: in.HTMLTemplate,
		IsDefault:    in.IsDefault,
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if in.IsDefault {
			if _, err := tx.Exec(ctx,
				`UPDATE print_templates SET is_default = FALSE WHERE tenant_id = $1 AND ktype = $2`,
				tenantID, in.KType,
			); err != nil {
				return fmt.Errorf("print: clear default: %w", err)
			}
		}
		return tx.QueryRow(ctx, `
			INSERT INTO print_templates (id, tenant_id, ktype, name, html_template, is_default)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING created_at, updated_at`,
			t.ID, t.TenantID, t.KType, t.Name, t.HTMLTemplate, t.IsDefault,
		).Scan(&t.CreatedAt, &t.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("print: insert template: %w", err)
	}
	return &t, nil
}

// Delete removes the template.
func (s *TemplateStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM print_templates WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrTemplateNotFound
		}
		return nil
	})
}
