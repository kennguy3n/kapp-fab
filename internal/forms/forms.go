// Package forms implements the public-facing Forms KApp: tenant-scoped
// record-creation endpoints that accept submissions from anonymous or
// loosely-auth'd users. The tenant context is derived from the form
// configuration rather than from a request header, so the submission
// path MUST look the form up on the admin pool (BYPASSRLS) and then
// re-enter the app pool with the correct tenant context to create the
// KRecord. This two-step dance is what keeps the public URL safe to
// share while still enforcing RLS end-to-end on the write.
package forms

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
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// Sentinel errors surfaced by the API handlers.
var (
	ErrFormNotFound = errors.New("forms: not found")
	ErrFormDisabled = errors.New("forms: submission disabled")
)

// Form mirrors a row in the `forms` table.
type Form struct {
	ID        uuid.UUID       `json:"id"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	KType     string          `json:"ktype"`
	Config    json.RawMessage `json:"config"`
	Status    string          `json:"status"`
	CreatedBy *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Config is the decoded form config JSONB. Kept small for Phase B; richer
// features (branding, conditional logic, captcha) live on the config
// blob and are interpreted client-side.
type Config struct {
	AllowAnonymous bool   `json:"allow_anonymous"`
	RequireAuth    bool   `json:"require_auth"`
	RedirectURL    string `json:"redirect_url,omitempty"`
	Title          string `json:"title,omitempty"`
	Description    string `json:"description,omitempty"`
}

// Store wraps form CRUD and submission. Two pools are required:
//   - pool:      app role, RLS-enforced, used for authenticated reads/writes
//   - adminPool: admin role, BYPASSRLS, used only to look up a form by id
//                during public submission (i.e. when the submitter does
//                not yet know which tenant they belong to)
// Callers that do not need public submission can pass nil for adminPool;
// GetPublic will then fall back to the shared pool and return nothing
// under the default-deny RLS policy (which is the safe default).
type Store struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
	registry  ktype.Registry
	records   *record.PGStore
	now       func() time.Time
}

// NewStore wires the form store dependencies.
func NewStore(pool *pgxpool.Pool, registry ktype.Registry, records *record.PGStore) *Store {
	return &Store{pool: pool, registry: registry, records: records, now: time.Now}
}

// WithAdminPool attaches an admin (BYPASSRLS) pool used by GetPublic to
// look up a form by id without requiring the caller to know the tenant.
func (s *Store) WithAdminPool(admin *pgxpool.Pool) *Store {
	s.adminPool = admin
	return s
}

// Create registers a new form under the caller's tenant.
func (s *Store) Create(ctx context.Context, tenantID uuid.UUID, ktypeName string, config Config, createdBy uuid.UUID) (*Form, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("forms: tenant id required")
	}
	if ktypeName == "" {
		return nil, errors.New("forms: ktype required")
	}
	if _, err := s.registry.Get(ctx, ktypeName, 0); err != nil {
		return nil, fmt.Errorf("forms: ktype lookup: %w", err)
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("forms: marshal config: %w", err)
	}
	form := &Form{
		ID:        uuid.New(),
		TenantID:  tenantID,
		KType:     ktypeName,
		Config:    configJSON,
		Status:    "active",
		CreatedBy: &createdBy,
		CreatedAt: s.now().UTC(),
		UpdatedAt: s.now().UTC(),
	}
	err = dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO forms (id, tenant_id, ktype, config, status, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			form.ID, form.TenantID, form.KType, form.Config, form.Status,
			form.CreatedBy, form.CreatedAt, form.UpdatedAt,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("forms: insert: %w", err)
	}
	return form, nil
}

// Get retrieves a form by id within the caller's tenant. Returns
// ErrFormNotFound if missing or RLS-hidden.
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (*Form, error) {
	var out Form
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanForm(tx.QueryRow(ctx,
			`SELECT id, tenant_id, ktype, config, status, created_by, created_at, updated_at
			 FROM forms WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFormNotFound
		}
		return nil, fmt.Errorf("forms: get: %w", err)
	}
	return &out, nil
}

// GetPublic fetches a form by id without requiring tenant context. Used
// by the public GET /forms/{id} endpoint so the renderer knows which
// KType schema to fetch. This must NOT be exposed to the submit path
// directly — submit goes through Submit which re-enters with the
// tenant context so the KRecord insert is RLS-protected.
func (s *Store) GetPublic(ctx context.Context, id uuid.UUID) (*Form, error) {
	pool := s.adminPool
	if pool == nil {
		pool = s.pool
	}
	row := pool.QueryRow(ctx,
		`SELECT id, tenant_id, ktype, config, status, created_by, created_at, updated_at
		 FROM forms WHERE id = $1`,
		id,
	)
	var out Form
	if err := scanForm(row, &out); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFormNotFound
		}
		return nil, fmt.Errorf("forms: get public: %w", err)
	}
	if out.Status != "active" {
		return nil, ErrFormDisabled
	}
	return &out, nil
}

// Submit creates a KRecord for the form's target KType under the form's
// tenant. The submitter does not need to know or prove the tenant id;
// it is derived from the (admin-looked-up) form. The resulting record
// insert still runs under the app role + SET LOCAL app.tenant_id so
// RLS is enforced end-to-end on the write.
//
// submitterID is optional — if the form allows anonymous submissions, a
// deterministic sentinel UUID stands in so the KRecord's created_by is
// not null (RLS and audit policies require a non-null actor).
func (s *Store) Submit(
	ctx context.Context,
	id uuid.UUID,
	data map[string]any,
	submitterID *uuid.UUID,
) (*record.KRecord, error) {
	form, err := s.GetPublic(ctx, id)
	if err != nil {
		return nil, err
	}
	var cfg Config
	_ = json.Unmarshal(form.Config, &cfg)
	if cfg.RequireAuth && (submitterID == nil || *submitterID == uuid.Nil) {
		return nil, fmt.Errorf("%w: authentication required", ErrFormDisabled)
	}
	if !cfg.AllowAnonymous && (submitterID == nil || *submitterID == uuid.Nil) {
		return nil, fmt.Errorf("%w: anonymous submission disabled", ErrFormDisabled)
	}
	actor := AnonymousSubmitter
	if submitterID != nil && *submitterID != uuid.Nil {
		actor = *submitterID
	}
	kt, err := s.registry.Get(ctx, form.KType, 0)
	if err != nil {
		return nil, fmt.Errorf("forms: ktype lookup: %w", err)
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("forms: marshal data: %w", err)
	}
	return s.records.Create(ctx, record.KRecord{
		TenantID:     form.TenantID,
		KType:        form.KType,
		KTypeVersion: kt.Version,
		Data:         dataJSON,
		CreatedBy:    actor,
	})
}

// AnonymousSubmitter is the deterministic sentinel UUID recorded as
// created_by on anonymous submissions. A non-nil id is required by the
// KRecord store; reusing a single sentinel keeps the audit log
// recognizably "public form" without inventing per-request identities.
var AnonymousSubmitter = uuid.MustParse("00000000-0000-0000-0000-0000000000f0")

func scanForm(row pgx.Row, out *Form) error {
	return row.Scan(
		&out.ID, &out.TenantID, &out.KType, &out.Config, &out.Status,
		&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt,
	)
}
