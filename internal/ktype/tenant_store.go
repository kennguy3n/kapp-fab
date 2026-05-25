package ktype

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// CustomNamePrefix is the namespace tenant-authored KTypes must
// occupy. All low-code KTypes are written as `custom.<slug>` so the
// platform can distinguish them from developer-shipped KTypes in
// every consumer (record store, agent tools, audit, exports, etc.).
const CustomNamePrefix = "custom."

// DefaultCustomFieldLimit is the maximum number of fields a single
// tenant-authored KType may declare. The limit exists so a tenant
// can't blow up form rendering or database row size by authoring a
// 10,000-field document, and so the per-record JSONB stays within
// sensible bounds. The integration tests pin this value; plans /
// quotas may override it in the future, in which case the override
// is plumbed through TenantStoreOption.
const DefaultCustomFieldLimit = 50

// ErrInvalidCustomName is returned when a caller attempts to upsert
// a tenant-authored KType whose name does not match the custom.<slug>
// pattern. The pattern also enforces lower-case, ASCII slug — the
// REST handler returns 400 on this sentinel so the UI can surface a
// helpful inline message.
var ErrInvalidCustomName = errors.New("ktype: custom KType name must match 'custom.<slug>'")

// ErrTooManyFields is returned when a custom KType definition
// declares more fields than DefaultCustomFieldLimit (or the limit
// supplied via TenantStoreOption.FieldLimit). Surfaces as 400 from
// the API so the UI can show a precise "limit reached" message.
var ErrTooManyFields = errors.New("ktype: custom KType exceeds field limit")

// ErrUnsupportedFieldType is returned when a custom KType field
// declares a type outside the safe subset. Posting hooks, custom Go
// expressions, calculated fields, and any non-data field kind stay
// developer-only — they require shipping code in internal/<module>/.
var ErrUnsupportedFieldType = errors.New("ktype: custom KType uses unsupported field type")

// SafeCustomFieldTypes is the closed set of field types a custom
// KType may use. Matches the validator/ValidateData type switch
// (string/number/boolean/date/enum/ref/text), plus email/phone/url
// which are validated via pattern. Object and array are NOT in this
// list — they let an author smuggle arbitrary structure into the
// schema and bypass per-field validation; if a tenant genuinely
// needs nested data, they declare a separate KType and a ref field.
var SafeCustomFieldTypes = map[string]bool{
	"string":   true,
	"text":     true,
	"number":   true,
	"integer":  true,
	"float":    true,
	"decimal":  true,
	"boolean":  true,
	"date":     true,
	"datetime": true,
	"enum":     true,
	"ref":      true,
	"email":    true,
	"phone":    true,
	"url":      true,
}

// customNamePattern mirrors the CHECK constraint
// `tenant_ktypes_name_chk` in migrations/000061_tenant_ktypes.sql.
// Kept in lock-step so the Go layer fails fast with a useful error
// before the DB does.
var customNamePattern = regexp.MustCompile(`^custom\.[a-z][a-z0-9_]*$`)

// CustomStatus values: only 'active' rows back record creates, but
// the row may live in 'draft' (editable in the builder, can't back
// records) or 'archived' (frozen, existing records still readable
// but no new ones).
const (
	CustomStatusDraft    = "draft"
	CustomStatusActive   = "active"
	CustomStatusArchived = "archived"
)

// TenantKType is the persisted row for a tenant-authored KType.
// Mirrors the tenant_ktypes table columns.
type TenantKType struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	Name        string          `json:"name"`
	Version     int             `json:"version"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	CreatedBy   uuid.UUID       `json:"created_by"`
}

// TenantStore is the persistence layer for tenant-authored
// (low-code) KTypes. It is intentionally a thin wrapper around the
// pool + dbutil.WithTenantTx so RLS does the heavy lifting on read
// and write paths.
type TenantStore struct {
	pool       *pgxpool.Pool
	fieldLimit int
}

// TenantStoreOption tunes per-instance behaviour.
type TenantStoreOption func(*TenantStore)

// WithFieldLimit overrides the default 50-field cap on
// tenant-authored KTypes. Plan-tiered quotas should compose this
// option when constructing a store for a tenant they have already
// confirmed has a larger plan.
func WithFieldLimit(n int) TenantStoreOption {
	return func(s *TenantStore) {
		if n > 0 {
			s.fieldLimit = n
		}
	}
}

// NewTenantStore wires a TenantStore against the shared pool.
func NewTenantStore(pool *pgxpool.Pool, opts ...TenantStoreOption) *TenantStore {
	s := &TenantStore{pool: pool, fieldLimit: DefaultCustomFieldLimit}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// FieldLimit exposes the active per-tenant field cap so handlers
// can include it in 400 responses or surface it in the builder UI.
func (s *TenantStore) FieldLimit() int { return s.fieldLimit }

// IsCustomName reports whether `name` is in the custom.<slug>
// namespace. Used by callers (record store, agent tool surfaces)
// to short-circuit the lookup path.
func IsCustomName(name string) bool {
	return strings.HasPrefix(name, CustomNamePrefix)
}

// validateCustomSchema rejects schemas that use unsupported field
// types, exceed the field cap, or otherwise leak developer-only
// surface area (posting hooks, custom hooks, agent tools with
// custom handlers). Returns a precise error so the API returns 400
// with a useful message instead of a generic DB error.
func (s *TenantStore) validateCustomSchema(schema json.RawMessage) error {
	var parsed struct {
		Name    string      `json:"name"`
		Version int         `json:"version"`
		Fields  []FieldSpec `json:"fields"`
		// Reject hostile sections explicitly — if a future schema
		// version introduces them, the validator + this list must
		// be updated together.
		PostingHook  json.RawMessage `json:"posting_hook,omitempty"`
		PostingHooks json.RawMessage `json:"posting_hooks,omitempty"`
		Computed     json.RawMessage `json:"computed,omitempty"`
		Calculations json.RawMessage `json:"calculations,omitempty"`
		AgentTools   json.RawMessage `json:"agent_tools,omitempty"`
		Triggers     json.RawMessage `json:"triggers,omitempty"`
	}
	if err := json.Unmarshal(schema, &parsed); err != nil {
		return fmt.Errorf("ktype: parse custom schema: %w", err)
	}
	if len(parsed.Fields) == 0 {
		return errors.New("ktype: custom KType requires at least one field")
	}
	if len(parsed.Fields) > s.fieldLimit {
		return fmt.Errorf("%w: %d fields exceeds limit of %d", ErrTooManyFields, len(parsed.Fields), s.fieldLimit)
	}
	for i := range parsed.Fields {
		f := &parsed.Fields[i]
		if f.Name == "" {
			return errors.New("ktype: custom field name required")
		}
		if !SafeCustomFieldTypes[f.Type] {
			return fmt.Errorf("%w: %q", ErrUnsupportedFieldType, f.Type)
		}
		if f.Type == "enum" && len(f.Values) == 0 {
			return fmt.Errorf("ktype: enum field %q requires values", f.Name)
		}
		if f.Type == "ref" && f.Ref == "" && f.KType == "" {
			return fmt.Errorf("ktype: ref field %q requires ref ktype", f.Name)
		}
	}
	if len(parsed.PostingHook) > 0 || len(parsed.PostingHooks) > 0 {
		return errors.New("ktype: custom KType may not declare posting_hook (developer-only)")
	}
	if len(parsed.Computed) > 0 || len(parsed.Calculations) > 0 {
		return errors.New("ktype: custom KType may not declare computed/calculations (developer-only)")
	}
	if len(parsed.AgentTools) > 0 {
		return errors.New("ktype: custom KType may not declare agent_tools (auto-generated only)")
	}
	if len(parsed.Triggers) > 0 {
		return errors.New("ktype: custom KType may not declare triggers (developer-only)")
	}
	return nil
}

// Upsert inserts or replaces a tenant-authored KType. The name must
// be in the custom.<slug> namespace; the schema must use only the
// safe field-type subset and stay within the configured field cap.
// Posting hooks, computed fields, custom agent tools, and triggers
// are rejected — those are developer-only surfaces.
//
// status defaults to 'draft' when empty. Callers transitioning a
// KType to 'active' should set status explicitly so the rule that
// only active KTypes back record creates is observable.
func (s *TenantStore) Upsert(ctx context.Context, kt TenantKType) (*TenantKType, error) {
	if kt.TenantID == uuid.Nil {
		return nil, errors.New("ktype: tenant id required")
	}
	if !customNamePattern.MatchString(kt.Name) {
		return nil, ErrInvalidCustomName
	}
	if kt.Version <= 0 {
		kt.Version = 1
	}
	if kt.Title == "" {
		return nil, errors.New("ktype: title required")
	}
	if kt.CreatedBy == uuid.Nil {
		return nil, errors.New("ktype: created_by required")
	}
	if !json.Valid(kt.Schema) {
		return nil, errors.New("ktype: schema is not valid JSON")
	}
	if err := s.validateCustomSchema(kt.Schema); err != nil {
		return nil, err
	}
	if kt.Status == "" {
		kt.Status = CustomStatusDraft
	}
	switch kt.Status {
	case CustomStatusDraft, CustomStatusActive, CustomStatusArchived:
	default:
		return nil, fmt.Errorf("ktype: invalid status %q", kt.Status)
	}

	var out TenantKType
	err := dbutil.WithTenantTx(ctx, s.pool, kt.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO tenant_ktypes
			    (tenant_id, name, version, title, description, schema, status, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (tenant_id, name, version) DO UPDATE
			    SET title = EXCLUDED.title,
			        description = EXCLUDED.description,
			        schema = EXCLUDED.schema,
			        status = EXCLUDED.status,
			        updated_at = NOW()
			 RETURNING tenant_id, name, version, title, description, schema, status, created_at, updated_at, created_by`,
			kt.TenantID, kt.Name, kt.Version, kt.Title, kt.Description,
			kt.Schema, kt.Status, kt.CreatedBy,
		).Scan(&out.TenantID, &out.Name, &out.Version, &out.Title, &out.Description,
			&out.Schema, &out.Status, &out.CreatedAt, &out.UpdatedAt, &out.CreatedBy)
	})
	if err != nil {
		return nil, fmt.Errorf("ktype: upsert custom: %w", err)
	}
	return &out, nil
}

// Get returns the highest version of the named custom KType for
// the tenant. Version 0 means "latest" — matches PGRegistry.Get
// semantics so the record store can swap the lookup path
// transparently for custom.* names.
func (s *TenantStore) Get(ctx context.Context, tenantID uuid.UUID, name string, version int) (*TenantKType, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ktype: tenant id required")
	}
	if !IsCustomName(name) {
		return nil, ErrInvalidCustomName
	}
	var out TenantKType
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if version > 0 {
			return tx.QueryRow(ctx,
				`SELECT tenant_id, name, version, title, description, schema, status, created_at, updated_at, created_by
				   FROM tenant_ktypes
				  WHERE tenant_id = $1 AND name = $2 AND version = $3`,
				tenantID, name, version,
			).Scan(&out.TenantID, &out.Name, &out.Version, &out.Title, &out.Description,
				&out.Schema, &out.Status, &out.CreatedAt, &out.UpdatedAt, &out.CreatedBy)
		}
		return tx.QueryRow(ctx,
			`SELECT tenant_id, name, version, title, description, schema, status, created_at, updated_at, created_by
			   FROM tenant_ktypes
			  WHERE tenant_id = $1 AND name = $2
			  ORDER BY version DESC
			  LIMIT 1`,
			tenantID, name,
		).Scan(&out.TenantID, &out.Name, &out.Version, &out.Title, &out.Description,
			&out.Schema, &out.Status, &out.CreatedAt, &out.UpdatedAt, &out.CreatedBy)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("ktype: get custom: %w", err)
	}
	return &out, nil
}

// List returns every custom KType for the tenant, ordered by name.
// Drafts are included alongside active and archived rows so the
// builder UI can render them all.
func (s *TenantStore) List(ctx context.Context, tenantID uuid.UUID) ([]TenantKType, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ktype: tenant id required")
	}
	var out []TenantKType
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, name, version, title, description, schema, status, created_at, updated_at, created_by
			   FROM tenant_ktypes
			  WHERE tenant_id = $1
			  ORDER BY name, version DESC`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kt TenantKType
			if err := rows.Scan(&kt.TenantID, &kt.Name, &kt.Version, &kt.Title, &kt.Description,
				&kt.Schema, &kt.Status, &kt.CreatedAt, &kt.UpdatedAt, &kt.CreatedBy); err != nil {
				return err
			}
			out = append(out, kt)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("ktype: list custom: %w", err)
	}
	return out, nil
}

// SetStatus transitions the custom KType to the supplied status.
// The DB CHECK constraint enforces the value set; this method only
// validates that the transition target is one of the known
// constants so a typo in the API doesn't reach SQL.
func (s *TenantStore) SetStatus(ctx context.Context, tenantID uuid.UUID, name string, version int, status string) error {
	if tenantID == uuid.Nil {
		return errors.New("ktype: tenant id required")
	}
	if !IsCustomName(name) {
		return ErrInvalidCustomName
	}
	switch status {
	case CustomStatusDraft, CustomStatusActive, CustomStatusArchived:
	default:
		return fmt.Errorf("ktype: invalid status %q", status)
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE tenant_ktypes
			    SET status = $4, updated_at = NOW()
			  WHERE tenant_id = $1 AND name = $2 AND version = $3`,
			tenantID, name, version, status,
		)
		if err != nil {
			return fmt.Errorf("ktype: set custom status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
