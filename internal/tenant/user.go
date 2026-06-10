package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// User mirrors a row in the `users` table — the globally-unique identity
// used across every tenant membership the user holds. KChatUserID is the
// stable external identifier from KChat and is required by the schema.
type User struct {
	ID          uuid.UUID `json:"id"`
	KChatUserID string    `json:"kchat_user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`

	// IAMUserID is the iam-core user identifier this Kapp user is
	// mirrored to, populated by user sync when the iam-core
	// integration is enabled (migration 000082). Empty for users
	// that predate the integration, were never synced, or in
	// deployments that never enable iam-core. Nothing in the legacy
	// KChat path reads it.
	IAMUserID string `json:"iam_user_id,omitempty"`
}

// UserTenant mirrors a row in the `user_tenants` table, binding a user to a
// tenant with a role and a membership status. Status is constrained to
// 'active' | 'invited' | 'suspended' by the schema.
type UserTenant struct {
	UserID   uuid.UUID `json:"user_id"`
	TenantID uuid.UUID `json:"tenant_id"`
	Role     string    `json:"role"`
	Status   string    `json:"status"`
}

// UserStore is the PostgreSQL-backed store for users + user_tenants.
//
// `users` is a global table (no RLS), so its reads/writes go through the
// shared pool directly. `user_tenants` is tenant-scoped with RLS, so every
// operation that touches it is wrapped in dbutil.WithTenantTx(tenantID)
// which issues `SET LOCAL app.tenant_id` for the transaction. The one
// exception is GetUserTenants, which is an intentionally cross-tenant
// control-plane read (see its doc comment) and runs on the optional
// adminPool that connects as `kapp_admin` (BYPASSRLS).
type UserStore struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
}

// NewUserStore binds a UserStore to the shared (tenant-scoped) pool. The
// admin pool is left unset, so GetUserTenants will return an empty slice
// unless the caller upgrades with WithAdminPool.
func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{pool: pool}
}

// WithAdminPool returns a copy of s that uses adminPool for control-plane
// cross-tenant reads. adminPool must be connected as a role with BYPASSRLS
// (see migrations/000002_admin_role.sql:kapp_admin). If adminPool is nil,
// the store continues to use the shared tenant-scoped pool (and
// GetUserTenants returns no rows under the default `kapp_app` role).
func (s *UserStore) WithAdminPool(adminPool *pgxpool.Pool) *UserStore {
	cp := *s
	cp.adminPool = adminPool
	return &cp
}

// CreateUser inserts a new user row. KChatUserID is required and is the
// only UNIQUE column on `users` in the current schema; a conflict on it
// returns ErrKChatUserIDTaken. Email has no UNIQUE constraint today, so
// duplicate emails are accepted at the DB level.
func (s *UserStore) CreateUser(ctx context.Context, u User) (*User, error) {
	if u.KChatUserID == "" {
		return nil, errors.New("tenant: kchat_user_id required")
	}
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	var out User
	// email and display_name are nullable in the schema. Write NULL for
	// empty strings via nullIfEmpty, then COALESCE the RETURNING clause
	// back to '' so pgx can scan into a plain string field.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (id, kchat_user_id, email, display_name)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, kchat_user_id, COALESCE(email, ''), COALESCE(display_name, '')`,
		u.ID, u.KChatUserID, nullIfEmpty(u.Email), nullIfEmpty(u.DisplayName),
	).Scan(&out.ID, &out.KChatUserID, &out.Email, &out.DisplayName)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation &&
			pgErr.ConstraintName == "users_kchat_user_id_key" {
			return nil, ErrKChatUserIDTaken
		}
		return nil, fmt.Errorf("tenant: insert user: %w", err)
	}
	return &out, nil
}

// GetUserByKChatID resolves a user by their kchat_user_id (the
// upstream KChat identifier) and returns ErrNotFound if no such user
// exists. The lookup is intentionally cross-tenant — the `users`
// table is global with no RLS — so the regular kapp_app pool is
// sufficient even when no tenant context is set. Used by the
// kchat-bridge presence webhook to map an incoming presence event
// onto a kapp user before resolving their tenant memberships.
func (s *UserStore) GetUserByKChatID(ctx context.Context, kchatUserID string) (*User, error) {
	if kchatUserID == "" {
		return nil, errors.New("tenant: kchat_user_id required")
	}
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, kchat_user_id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(iam_user_id, '')
		 FROM users WHERE kchat_user_id = $1`, kchatUserID,
	).Scan(&u.ID, &u.KChatUserID, &u.Email, &u.DisplayName, &u.IAMUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get user by kchat id: %w", err)
	}
	return &u, nil
}

// GetUser returns the user with the given id or ErrNotFound.
func (s *UserStore) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, kchat_user_id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(iam_user_id, '')
		 FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.KChatUserID, &u.Email, &u.DisplayName, &u.IAMUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get user: %w", err)
	}
	return &u, nil
}

// SetIAMUserID persists the iam-core user mapping on the users row
// after user sync provisions the user in iam-core. Like
// PGStore.SetIAMTenantID it only writes when the column is currently
// NULL so a re-sync never repoints an already-mapped user at a
// different iam-core identity; re-setting the same value is a benign
// success and a conflicting value returns ErrIAMUserAlreadyMapped.
// `users` is a global (non-RLS) table so the write runs on the shared
// pool directly.
func (s *UserStore) SetIAMUserID(ctx context.Context, id uuid.UUID, iamUserID string) error {
	if id == uuid.Nil {
		return errors.New("tenant: user id required")
	}
	if iamUserID == "" {
		return errors.New("tenant: iam_user_id is required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users
		    SET iam_user_id = $1
		  WHERE id = $2
		    AND iam_user_id IS NULL`,
		iamUserID, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set iam user id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		existing, getErr := s.GetUser(ctx, id)
		if getErr != nil {
			return getErr
		}
		if existing.IAMUserID == iamUserID {
			return nil
		}
		return ErrIAMUserAlreadyMapped
	}
	return nil
}

// nullIfEmpty returns nil for an empty string so that NULL is stored for
// optional text columns rather than an empty string.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AddUserToTenant binds a user to a tenant with a role. The membership is
// created in the `active` state. Duplicate (user, tenant) pairs return
// ErrMembershipExists.
//
// user_tenants is RLS-protected, so the INSERT runs inside a transaction
// with app.tenant_id = tenantID.
func (s *UserStore) AddUserToTenant(
	ctx context.Context,
	userID, tenantID uuid.UUID,
	role string,
) error {
	if userID == uuid.Nil || tenantID == uuid.Nil {
		return errors.New("tenant: user and tenant id required")
	}
	if role == "" {
		return errors.New("tenant: role required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO user_tenants (user_id, tenant_id, role, status)
			 VALUES ($1, $2, $3, 'active')`,
			userID, tenantID, role,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
				return ErrMembershipExists
			}
			return fmt.Errorf("tenant: add user to tenant: %w", err)
		}
		return nil
	})
}

// GetUserTenants returns every tenant membership for the given user across
// all tenants. This is an intentionally cross-tenant control-plane read —
// there is no single tenant id to set app.tenant_id to.
//
// The query runs on the admin pool (role `kapp_admin`, BYPASSRLS — see
// migrations/000002_admin_role.sql) when configured via WithAdminPool.
// Without it, the store falls back to the shared `kapp_app` pool; under
// the default RLS policy that connection sees no rows because
// `app.tenant_id` is NULL for a non-BYPASSRLS role, which is the
// documented behaviour for stores constructed without an admin pool.
func (s *UserStore) GetUserTenants(
	ctx context.Context,
	userID uuid.UUID,
) ([]UserTenant, error) {
	pool := s.adminPool
	if pool == nil {
		pool = s.pool
	}
	rows, err := pool.Query(ctx,
		`SELECT user_id, tenant_id, role, status
		 FROM user_tenants
		 WHERE user_id = $1
		 ORDER BY tenant_id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("tenant: list user tenants: %w", err)
	}
	defer rows.Close()
	return scanMemberships(rows)
}

// GetTenantUsers returns every user membership for the given tenant.
// user_tenants is RLS-protected, so the SELECT runs inside a transaction
// with app.tenant_id = tenantID.
func (s *UserStore) GetTenantUsers(
	ctx context.Context,
	tenantID uuid.UUID,
) ([]UserTenant, error) {
	var out []UserTenant
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT user_id, tenant_id, role, status
			 FROM user_tenants
			 WHERE tenant_id = $1
			 ORDER BY user_id`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("tenant: list tenant users: %w", err)
		}
		defer rows.Close()
		out, err = scanMemberships(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func scanMemberships(rows pgx.Rows) ([]UserTenant, error) {
	out := make([]UserTenant, 0)
	for rows.Next() {
		var m UserTenant
		if err := rows.Scan(&m.UserID, &m.TenantID, &m.Role, &m.Status); err != nil {
			return nil, fmt.Errorf("tenant: scan membership: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenant: membership rows: %w", err)
	}
	return out, nil
}

// Sentinel errors specific to the user/membership surface.
var (
	ErrKChatUserIDTaken = errors.New("tenant: kchat_user_id already taken")
	ErrMembershipExists = errors.New("tenant: user is already a member of this tenant")
	// ErrIAMUserAlreadyMapped is returned by SetIAMUserID when the
	// user already carries a DIFFERENT iam-core mapping (mirror of
	// ErrIAMTenantAlreadyMapped on the tenant side).
	ErrIAMUserAlreadyMapped = errors.New("tenant: iam user id already mapped")
)
