package record

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

// ErrViewNotFound is returned by ViewStore when the view id does not
// resolve for the caller's tenant. It mirrors ErrNotFound on the
// record store so handlers can apply uniform 404 mapping.
var ErrViewNotFound = errors.New("record: saved view not found")

// SavedView captures the filter/sort/column state a user wants to
// pin to a particular KType list page. Views are scoped to one
// tenant and, by default, to one user — the `shared` flag opts a
// view into tenant-wide visibility so e.g. the ops lead can publish
// "Overdue invoices" to the whole tenant.
//
// Filters/Columns are kept as json.RawMessage so the frontend can
// evolve its schema without a migration. The backend only persists
// and returns them verbatim — filter semantics are interpreted on
// the client, mirroring how BaseTable handles saved layouts.
type SavedView struct {
	TenantID  uuid.UUID       `json:"tenant_id"`
	ID        uuid.UUID       `json:"id"`
	UserID    uuid.UUID       `json:"user_id"`
	KType     string          `json:"ktype"`
	Name      string          `json:"name"`
	Filters   json.RawMessage `json:"filters"`
	Sort      string          `json:"sort"`
	Columns   json.RawMessage `json:"columns"`
	IsDefault bool            `json:"is_default"`
	Shared    bool            `json:"shared"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ViewPatch is the set of fields a PATCH /views/{id} call may
// mutate. Unset pointers leave the existing column untouched — this
// shape lets the frontend toggle `is_default` without resending the
// whole JSON blob.
type ViewPatch struct {
	Name      *string         `json:"name,omitempty"`
	Filters   json.RawMessage `json:"filters,omitempty"`
	Sort      *string         `json:"sort,omitempty"`
	Columns   json.RawMessage `json:"columns,omitempty"`
	IsDefault *bool           `json:"is_default,omitempty"`
	Shared    *bool           `json:"shared,omitempty"`
}

// ViewStore encapsulates the CRUD surface for saved_views. Every
// call goes through dbutil.WithTenantTx so the row is filtered by
// the `tenant_isolation` RLS policy — a view stored by tenant A is
// unreachable from tenant B even if the caller guesses the UUID.
type ViewStore struct {
	pool *pgxpool.Pool
}

// NewViewStore wires a ViewStore around the shared pool.
func NewViewStore(pool *pgxpool.Pool) *ViewStore {
	return &ViewStore{pool: pool}
}

// List returns the saved views visible to `userID` for a given
// KType: their own views plus any view another user in the tenant
// has shared. Results are ordered by `is_default DESC, name ASC`
// so the caller's default view lands at the top of the dropdown.
func (s *ViewStore) List(ctx context.Context, tenantID, userID uuid.UUID, ktype string) ([]SavedView, error) {
	if tenantID == uuid.Nil || userID == uuid.Nil {
		return nil, errors.New("record: tenant and user id required")
	}
	if ktype == "" {
		return nil, errors.New("record: ktype required")
	}
	out := make([]SavedView, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, user_id, ktype, name, filters, sort, columns,
			        is_default, shared, created_at, updated_at
			   FROM saved_views
			  WHERE tenant_id = $1
			    AND ktype = $2
			    AND (user_id = $3 OR shared = TRUE)
			  ORDER BY is_default DESC, name ASC`,
			tenantID, ktype, userID,
		)
		if err != nil {
			return fmt.Errorf("record: list views: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanView(rows)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Get loads a single view by id, scoped by tenant + visibility.
// The RLS policy enforces tenant isolation; the AND (...) predicate
// enforces the user-vs-shared rule.
func (s *ViewStore) Get(ctx context.Context, tenantID, userID, id uuid.UUID) (*SavedView, error) {
	if tenantID == uuid.Nil || userID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("record: tenant, user, and view id required")
	}
	var v SavedView
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, id, user_id, ktype, name, filters, sort, columns,
			        is_default, shared, created_at, updated_at
			   FROM saved_views
			  WHERE tenant_id = $1
			    AND id = $2
			    AND (user_id = $3 OR shared = TRUE)`,
			tenantID, id, userID,
		)
		var err error
		v, err = scanViewRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrViewNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Create inserts a new view. When IsDefault is true, the store
// clears the flag on any other view the same user has for this
// KType so there is at most one default per (user, ktype) pair.
func (s *ViewStore) Create(ctx context.Context, v SavedView) (*SavedView, error) {
	if v.TenantID == uuid.Nil || v.UserID == uuid.Nil {
		return nil, errors.New("record: tenant and user id required")
	}
	if v.KType == "" || v.Name == "" {
		return nil, errors.New("record: ktype and name required")
	}
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	v.Filters = nonNilJSON(v.Filters, `{}`)
	v.Columns = nonNilJSON(v.Columns, `[]`)

	var out SavedView
	err := dbutil.WithTenantTx(ctx, s.pool, v.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if v.IsDefault {
			if err := clearDefault(ctx, tx, v.TenantID, v.UserID, v.KType, uuid.Nil); err != nil {
				return err
			}
		}
		row := tx.QueryRow(ctx,
			`INSERT INTO saved_views
			   (tenant_id, id, user_id, ktype, name, filters, sort, columns, is_default, shared)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			 RETURNING tenant_id, id, user_id, ktype, name, filters, sort, columns,
			           is_default, shared, created_at, updated_at`,
			v.TenantID, v.ID, v.UserID, v.KType, v.Name,
			v.Filters, v.Sort, v.Columns, v.IsDefault, v.Shared,
		)
		var err error
		out, err = scanViewRow(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Update applies a partial patch. Only the view's owner may mutate
// it — shared visibility lets other users read, not overwrite. We
// enforce ownership in SQL (WHERE user_id = $N) rather than reading
// the row first so a concurrent delete cannot race us.
func (s *ViewStore) Update(ctx context.Context, tenantID, userID, id uuid.UUID, patch ViewPatch) (*SavedView, error) {
	if tenantID == uuid.Nil || userID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("record: tenant, user, and view id required")
	}
	var out SavedView
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if patch.IsDefault != nil && *patch.IsDefault {
			row := tx.QueryRow(ctx,
				`SELECT ktype FROM saved_views
				  WHERE tenant_id = $1 AND id = $2 AND user_id = $3`,
				tenantID, id, userID,
			)
			var ktype string
			if err := row.Scan(&ktype); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrViewNotFound
				}
				return err
			}
			if err := clearDefault(ctx, tx, tenantID, userID, ktype, id); err != nil {
				return err
			}
		}
		row := tx.QueryRow(ctx,
			`UPDATE saved_views SET
			   name       = COALESCE($4, name),
			   filters    = COALESCE($5, filters),
			   sort       = COALESCE($6, sort),
			   columns    = COALESCE($7, columns),
			   is_default = COALESCE($8, is_default),
			   shared     = COALESCE($9, shared),
			   updated_at = now()
			 WHERE tenant_id = $1 AND id = $2 AND user_id = $3
			 RETURNING tenant_id, id, user_id, ktype, name, filters, sort, columns,
			           is_default, shared, created_at, updated_at`,
			tenantID, id, userID,
			patch.Name,
			nullableJSON(patch.Filters),
			patch.Sort,
			nullableJSON(patch.Columns),
			patch.IsDefault,
			patch.Shared,
		)
		var err error
		out, err = scanViewRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrViewNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a view; owner-only by the same argument as Update.
func (s *ViewStore) Delete(ctx context.Context, tenantID, userID, id uuid.UUID) error {
	if tenantID == uuid.Nil || userID == uuid.Nil || id == uuid.Nil {
		return errors.New("record: tenant, user, and view id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM saved_views
			  WHERE tenant_id = $1 AND id = $2 AND user_id = $3`,
			tenantID, id, userID,
		)
		if err != nil {
			return fmt.Errorf("record: delete view: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrViewNotFound
		}
		return nil
	})
}

// clearDefault drops the is_default flag on every other view this
// (user, ktype) pair owns, preserving the "one default at a time"
// invariant. `exceptID == uuid.Nil` matches every view (used on
// Create); a non-nil value excludes the row currently being promoted.
func clearDefault(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, ktype string, exceptID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE saved_views
		    SET is_default = FALSE, updated_at = now()
		  WHERE tenant_id = $1 AND user_id = $2 AND ktype = $3
		    AND id <> $4
		    AND is_default = TRUE`,
		tenantID, userID, ktype, exceptID,
	)
	if err != nil {
		return fmt.Errorf("record: clear default view: %w", err)
	}
	return nil
}

// scanView reads one SavedView from rows.Scan.
func scanView(rows pgx.Rows) (SavedView, error) {
	var v SavedView
	err := rows.Scan(
		&v.TenantID, &v.ID, &v.UserID, &v.KType, &v.Name,
		&v.Filters, &v.Sort, &v.Columns,
		&v.IsDefault, &v.Shared, &v.CreatedAt, &v.UpdatedAt,
	)
	return v, err
}

// scanViewRow reads one SavedView from a QueryRow result.
func scanViewRow(row pgx.Row) (SavedView, error) {
	var v SavedView
	err := row.Scan(
		&v.TenantID, &v.ID, &v.UserID, &v.KType, &v.Name,
		&v.Filters, &v.Sort, &v.Columns,
		&v.IsDefault, &v.Shared, &v.CreatedAt, &v.UpdatedAt,
	)
	return v, err
}

// nonNilJSON substitutes a literal default when the caller left a
// json.RawMessage nil — keeps the NOT NULL constraints on the table
// happy without forcing handlers to fill in empty blobs.
func nonNilJSON(in json.RawMessage, fallback string) json.RawMessage {
	if len(in) == 0 {
		return json.RawMessage(fallback)
	}
	return in
}

// nullableJSON maps an empty RawMessage to a typed SQL NULL so the
// COALESCE in the UPDATE falls through to the existing column value.
func nullableJSON(in json.RawMessage) any {
	if len(in) == 0 {
		return nil
	}
	return in
}
