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

// User mirrors a row in the `users` table — the globally-unique identity
// used across every tenant membership the user holds.
type User struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
}

// UserTenant mirrors a row in the `user_tenants` table, binding a user to a
// tenant with a role and a membership status.
type UserTenant struct {
	UserID   uuid.UUID `json:"user_id"`
	TenantID uuid.UUID `json:"tenant_id"`
	Role     string    `json:"role"`
	Status   string    `json:"status"`
}

// UserStore is the PostgreSQL-backed store for users + user_tenants. The
// `users` table is global (not tenant-scoped); `user_tenants` is tenant-
// scoped with RLS but writes here come from the control plane, so this
// store uses the shared pool directly rather than SET LOCAL app.tenant_id.
// Later phases will move the `user_tenants` writes behind a tenant-scoped
// interface once self-service invites land.
type UserStore struct {
	pool *pgxpool.Pool
}

// NewUserStore binds a UserStore to the shared pool.
func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{pool: pool}
}

// CreateUser inserts a new user row. Email uniqueness is enforced by the
// database; a conflict returns ErrEmailTaken.
func (s *UserStore) CreateUser(ctx context.Context, u User) (*User, error) {
	if u.Email == "" {
		return nil, errors.New("tenant: user email required")
	}
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	var out User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (id, email, name)
		 VALUES ($1, $2, $3)
		 RETURNING id, email, name`,
		u.ID, u.Email, u.Name,
	).Scan(&out.ID, &out.Email, &out.Name)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("tenant: insert user: %w", err)
	}
	return &out, nil
}

// GetUser returns the user with the given id or ErrNotFound.
func (s *UserStore) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.Name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get user: %w", err)
	}
	return &u, nil
}

// AddUserToTenant binds a user to a tenant with a role. The membership is
// created in the `active` state. Duplicate (user, tenant) pairs return
// ErrMembershipExists.
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
	_, err := s.pool.Exec(ctx,
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
}

// GetUserTenants returns every tenant membership for the given user.
func (s *UserStore) GetUserTenants(
	ctx context.Context,
	userID uuid.UUID,
) ([]UserTenant, error) {
	rows, err := s.pool.Query(ctx,
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
func (s *UserStore) GetTenantUsers(
	ctx context.Context,
	tenantID uuid.UUID,
) ([]UserTenant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id, tenant_id, role, status
		 FROM user_tenants
		 WHERE tenant_id = $1
		 ORDER BY user_id`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("tenant: list tenant users: %w", err)
	}
	defer rows.Close()
	return scanMemberships(rows)
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
	ErrEmailTaken       = errors.New("tenant: user email already taken")
	ErrMembershipExists = errors.New("tenant: user is already a member of this tenant")
)
