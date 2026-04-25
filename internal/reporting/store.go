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
	TenantID    uuid.UUID    `json:"tenant_id"`
	ID          uuid.UUID    `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Definition  Definition   `json:"definition"`
	Visibility  string       `json:"visibility"`
	SharedWith  []ShareEntry `json:"shared_with"`
	CreatedBy   *uuid.UUID   `json:"created_by,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// ShareEntry is one element of saved_reports.shared_with. Type is
// either "role" (ID is a role name) or "user" (ID is a user UUID
// string). Validated at API entry so the JSONB column never holds
// an unknown shape.
type ShareEntry struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Visibility constants. Match the CHECK constraint in
// migrations/000034_report_sharing.sql.
const (
	VisibilityPrivate = "private"
	VisibilityShared  = "shared"
	VisibilityPublic  = "public"
)

// ShareTypeRole / ShareTypeUser enumerate the legal ShareEntry.Type
// values.
const (
	ShareTypeRole = "role"
	ShareTypeUser = "user"
)

// ValidateShareEntries rejects malformed share entries before the
// JSONB write so the column always parses on the read side.
func ValidateShareEntries(entries []ShareEntry) error {
	for _, e := range entries {
		switch e.Type {
		case ShareTypeRole, ShareTypeUser:
		default:
			return fmt.Errorf("reporting: share entry type %q invalid", e.Type)
		}
		if e.ID == "" {
			return errors.New("reporting: share entry id required")
		}
	}
	return nil
}

// ValidateVisibility rejects unknown visibility values.
func ValidateVisibility(v string) error {
	switch v {
	case VisibilityPrivate, VisibilityShared, VisibilityPublic:
		return nil
	case "":
		return nil
	default:
		return fmt.Errorf("reporting: visibility %q invalid", v)
	}
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

// defaultIfEmpty returns fallback when v is the zero value, so
// callers can keep the per-row default ("private") consistent
// without leaking the constant into every call site.
func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

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
		sharedJSON, err := json.Marshal(out.SharedWith)
		if err != nil {
			return fmt.Errorf("reporting: marshal shared_with: %w", err)
		}
		return tx.QueryRow(ctx,
			`INSERT INTO saved_reports (tenant_id, id, name, description, definition, visibility, shared_with, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING created_at, updated_at`,
			r.TenantID, r.ID, r.Name, r.Description, def,
			defaultIfEmpty(out.Visibility, VisibilityPrivate),
			sharedJSON, createdBy,
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
		// Read back visibility / shared_with too — those columns
		// are owned by SetSharing, not Update, so we don't mutate
		// them here but we MUST scan them back so the response the
		// handler returns reflects the row's actual state instead
		// of the empty defaults on the input struct.
		var shared []byte
		if err := tx.QueryRow(ctx,
			`SELECT created_at, updated_at, visibility, shared_with
			   FROM saved_reports
			  WHERE tenant_id = $1 AND id = $2`,
			r.TenantID, r.ID,
		).Scan(&out.CreatedAt, &out.UpdatedAt, &out.Visibility, &shared); err != nil {
			return err
		}
		if len(shared) > 0 {
			if err := json.Unmarshal(shared, &out.SharedWith); err != nil {
				return err
			}
		}
		return nil
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
		var shared []byte
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, name, description, definition, visibility, shared_with, created_by, created_at, updated_at
			 FROM saved_reports WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&out.TenantID, &out.ID, &out.Name, &out.Description, &def,
			&out.Visibility, &shared, &createdBy, &out.CreatedAt, &out.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrReportNotFound
			}
			return err
		}
		out.CreatedBy = createdBy
		if err := json.Unmarshal(def, &out.Definition); err != nil {
			return err
		}
		if len(shared) > 0 {
			if err := json.Unmarshal(shared, &out.SharedWith); err != nil {
				return err
			}
		}
		return nil
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
			`SELECT tenant_id, id, name, description, definition, visibility, shared_with, created_by, created_at, updated_at
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
			var shared []byte
			var createdBy *uuid.UUID
			if err := rows.Scan(
				&r.TenantID, &r.ID, &r.Name, &r.Description, &def,
				&r.Visibility, &shared, &createdBy, &r.CreatedAt, &r.UpdatedAt,
			); err != nil {
				return err
			}
			r.CreatedBy = createdBy
			if err := json.Unmarshal(def, &r.Definition); err != nil {
				return err
			}
			if len(shared) > 0 {
				if err := json.Unmarshal(shared, &r.SharedWith); err != nil {
					return err
				}
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

// ListVisible returns reports the supplied user/roles can see in
// the tenant. Visibility rules:
//
//   - public        → always visible
//   - owner         → always visible (created_by = userID)
//   - shared        → visible iff shared_with contains
//     {type:"user", id: userID} or
//     {type:"role", id: <one of roles>}
//   - private/other → not visible
//
// The query uses RLS so cross-tenant rows are unreachable; the
// visibility predicate is a per-row gate inside the tenant. Roles
// are passed verbatim — the caller is responsible for resolving
// which role names belong to the user.
func (s *Store) ListVisible(ctx context.Context, tenantID, userID uuid.UUID, roles []string) ([]SavedReport, error) {
	all, err := s.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	roleSet := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		roleSet[r] = struct{}{}
	}
	out := make([]SavedReport, 0, len(all))
	for _, r := range all {
		if r.canSee(userID, roleSet) {
			out = append(out, r)
		}
	}
	return out, nil
}

// canSee mirrors the access-control predicate ListVisible would
// otherwise inline three times. Lives on SavedReport so unit tests
// can probe it without the Store.
func (r *SavedReport) canSee(userID uuid.UUID, roles map[string]struct{}) bool {
	switch defaultIfEmpty(r.Visibility, VisibilityPrivate) {
	case VisibilityPublic:
		return true
	case VisibilityShared:
		if r.CreatedBy != nil && *r.CreatedBy == userID {
			return true
		}
		userIDStr := userID.String()
		for _, e := range r.SharedWith {
			switch e.Type {
			case ShareTypeUser:
				if e.ID == userIDStr {
					return true
				}
			case ShareTypeRole:
				if _, ok := roles[e.ID]; ok {
					return true
				}
			}
		}
		return false
	default: // private (or unknown — fail closed)
		return r.CreatedBy != nil && *r.CreatedBy == userID
	}
}

// SetSharing replaces the visibility + shared_with on an existing
// report. Used by the PATCH /reports/{id}/share endpoint.
func (s *Store) SetSharing(ctx context.Context, tenantID, id uuid.UUID, visibility string, shared []ShareEntry) (*SavedReport, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("reporting: tenant id and report id required")
	}
	if err := ValidateVisibility(visibility); err != nil {
		return nil, err
	}
	if err := ValidateShareEntries(shared); err != nil {
		return nil, err
	}
	sharedJSON, err := json.Marshal(shared)
	if err != nil {
		return nil, err
	}
	err = dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE saved_reports
			    SET visibility = $3, shared_with = $4, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, defaultIfEmpty(visibility, VisibilityPrivate), sharedJSON,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrReportNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, tenantID, id)
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
