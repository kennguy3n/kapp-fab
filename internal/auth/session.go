package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSessionNotFound is returned when a session lookup misses or the
// row has been revoked / expired.
var ErrSessionNotFound = errors.New("auth: session not found")

// ErrSessionLimit is returned when a tenant has reached its configured
// session cap. The limit is read from tenants.quota -> "max_sessions"
// and defaults to DefaultMaxSessions when absent.
var ErrSessionLimit = errors.New("auth: tenant session limit reached")

// ErrRefreshReuse is returned by RotateRefresh when the presented
// refresh JTI no longer matches the session's current refresh_jti —
// i.e. the token was already consumed by an earlier rotation. This is
// the refresh-token reuse / replay signal: the caller revokes the
// whole session family and rejects the request. Distinguished from
// ErrSessionNotFound (the session is simply gone/revoked/expired) so
// the caller can react to an active attack versus an ordinary stale
// session.
var ErrRefreshReuse = errors.New("auth: refresh token reuse detected")

// DefaultMaxSessions is the per-tenant concurrent-session cap applied
// when tenants.quota does not override it. Generous enough for a
// small team; a quota bump is the escape hatch.
const DefaultMaxSessions = 200

// Session captures one active auth context: the user, the tenant
// they're scoped to, and the refresh token family. Revoked sessions
// stay in the table (with revoked_at set) so GET /sessions can show
// a history.
type Session struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	UserID       uuid.UUID
	RefreshJTI   string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	LastUsedAt   time.Time
	UserAgent    string
	IPAddress    string
}

// SessionStore persists and inspects sessions. The PG implementation
// lives below; the interface is kept narrow so a Redis-backed cache
// can plug in later without touching callers.
type SessionStore interface {
	Create(ctx context.Context, s Session) (*Session, error)
	Get(ctx context.Context, tenantID, sessionID uuid.UUID) (*Session, error)
	Revoke(ctx context.Context, tenantID, sessionID uuid.UUID) error
	RevokeByUser(ctx context.Context, tenantID, userID uuid.UUID) error
	RevokeByTenant(ctx context.Context, tenantID uuid.UUID) error
	Touch(ctx context.Context, tenantID, sessionID uuid.UUID, now time.Time) error
	ActiveCount(ctx context.Context, tenantID uuid.UUID) (int, error)
	// RotateRefresh atomically swaps the session's refresh_jti from
	// oldJTI to newJTI as part of refresh-token rotation. It is a
	// compare-and-swap: the UPDATE only fires when the row's current
	// refresh_jti still equals oldJTI (and the session is live), so a
	// concurrent rotation or a replayed/stale token cannot both
	// succeed. Returns ErrRefreshReuse when no live row matches
	// oldJTI — the reuse signal the caller turns into a family
	// revocation.
	RotateRefresh(ctx context.Context, tenantID, sessionID uuid.UUID, oldJTI, newJTI string) error
}

// PGSessionStore is the PostgreSQL implementation. The sessions table
// is tenant-scoped with RLS; we rely on SET LOCAL app.tenant_id on
// every call (done by dbutil.WithTenantTx) to enforce isolation.
type PGSessionStore struct {
	pool *pgxpool.Pool
	// tenantQuota loads the raw JSONB quota for a tenant so we can
	// read max_sessions without depending on the tenant package.
	// A nil loader defaults every tenant to DefaultMaxSessions.
	tenantQuota func(ctx context.Context, tenantID uuid.UUID) (json.RawMessage, error)
}

// NewPGSessionStore returns a store backed by the shared pool.
func NewPGSessionStore(pool *pgxpool.Pool) *PGSessionStore {
	return &PGSessionStore{pool: pool}
}

// WithQuotaLoader wires a function that returns the tenant's raw
// quota JSONB. Used by the create path to enforce the concurrent
// session cap.
func (s *PGSessionStore) WithQuotaLoader(f func(ctx context.Context, tenantID uuid.UUID) (json.RawMessage, error)) *PGSessionStore {
	s.tenantQuota = f
	return s
}

// Create inserts a new session row after verifying the tenant has
// room under its configured cap. Concurrent inserts can race past
// the check-then-insert window; we accept the soft overage because
// the race's upper bound is (N writers × check latency) and the
// cap itself is a soft usage policy, not a security boundary.
func (s *PGSessionStore) Create(ctx context.Context, sess Session) (*Session, error) {
	if sess.TenantID == uuid.Nil || sess.UserID == uuid.Nil {
		return nil, errors.New("auth: tenant id and user id required")
	}
	if sess.ExpiresAt.IsZero() {
		return nil, errors.New("auth: expires_at required")
	}
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	if sess.IssuedAt.IsZero() {
		sess.IssuedAt = time.Now().UTC()
	}
	if sess.LastUsedAt.IsZero() {
		sess.LastUsedAt = sess.IssuedAt
	}

	max := s.maxSessions(ctx, sess.TenantID)
	active, err := s.ActiveCount(ctx, sess.TenantID)
	if err != nil {
		return nil, err
	}
	if active >= max {
		return nil, ErrSessionLimit
	}

	var out Session
	err = withTenantTx(ctx, s.pool, sess.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO sessions
			     (id, tenant_id, user_id, refresh_jti, issued_at,
			      expires_at, last_used_at, user_agent, ip_address)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 RETURNING id, tenant_id, user_id, refresh_jti,
			           issued_at, expires_at, revoked_at,
			           last_used_at, user_agent, ip_address`,
			sess.ID, sess.TenantID, sess.UserID, sess.RefreshJTI,
			sess.IssuedAt, sess.ExpiresAt, sess.LastUsedAt,
			sess.UserAgent, sess.IPAddress,
		).Scan(
			&out.ID, &out.TenantID, &out.UserID, &out.RefreshJTI,
			&out.IssuedAt, &out.ExpiresAt, &out.RevokedAt,
			&out.LastUsedAt, &out.UserAgent, &out.IPAddress,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("auth: insert session: %w", err)
	}
	return &out, nil
}

// Get returns an active session or ErrSessionNotFound. Revoked or
// expired rows are treated as absent.
func (s *PGSessionStore) Get(ctx context.Context, tenantID, sessionID uuid.UUID) (*Session, error) {
	var out Session
	err := withTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, user_id, refresh_jti,
			        issued_at, expires_at, revoked_at,
			        last_used_at, user_agent, ip_address
			   FROM sessions
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, sessionID,
		).Scan(
			&out.ID, &out.TenantID, &out.UserID, &out.RefreshJTI,
			&out.IssuedAt, &out.ExpiresAt, &out.RevokedAt,
			&out.LastUsedAt, &out.UserAgent, &out.IPAddress,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	if out.RevokedAt != nil {
		return nil, ErrSessionNotFound
	}
	if !out.ExpiresAt.IsZero() && time.Now().After(out.ExpiresAt) {
		return nil, ErrSessionNotFound
	}
	return &out, nil
}

// Revoke marks a single session as revoked. Idempotent.
func (s *PGSessionStore) Revoke(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return withTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now()
			  WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`,
			tenantID, sessionID,
		)
		return err
	})
}

// RevokeByUser revokes every active session for one user inside the
// tenant. Used on password reset / forced logout.
func (s *PGSessionStore) RevokeByUser(ctx context.Context, tenantID, userID uuid.UUID) error {
	return withTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now()
			  WHERE tenant_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
			tenantID, userID,
		)
		return err
	})
}

// RevokeByTenant revokes every active session for the tenant. Called
// when a tenant is suspended so outstanding JWTs stop authenticating.
func (s *PGSessionStore) RevokeByTenant(ctx context.Context, tenantID uuid.UUID) error {
	return withTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now()
			  WHERE tenant_id = $1 AND revoked_at IS NULL`,
			tenantID,
		)
		return err
	})
}

// Touch bumps last_used_at. Best-effort; callers shouldn't fail a
// request because a touch write failed.
func (s *PGSessionStore) Touch(ctx context.Context, tenantID, sessionID uuid.UUID, now time.Time) error {
	return withTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE sessions SET last_used_at = $1
			  WHERE tenant_id = $2 AND id = $3 AND revoked_at IS NULL`,
			now, tenantID, sessionID,
		)
		return err
	})
}

// RotateRefresh atomically consumes the presented refresh JTI and
// installs a freshly-minted one, implementing single-use refresh
// tokens. The UPDATE is a compare-and-swap guarded on
// refresh_jti = oldJTI (plus the live-session predicates), so it is
// safe under concurrency: of two requests presenting the same token,
// at most one observes RowsAffected == 1 and the loser gets
// ErrRefreshReuse. A replayed token whose JTI was already rotated away
// likewise matches no row and returns ErrRefreshReuse, which the SSO
// layer escalates to a family revocation.
func (s *PGSessionStore) RotateRefresh(ctx context.Context, tenantID, sessionID uuid.UUID, oldJTI, newJTI string) error {
	if oldJTI == "" || newJTI == "" {
		return errors.New("auth: rotate refresh requires both old and new jti")
	}
	return withTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE sessions
			    SET refresh_jti = $1, last_used_at = now()
			  WHERE tenant_id = $2 AND id = $3 AND refresh_jti = $4
			    AND revoked_at IS NULL AND expires_at > now()`,
			newJTI, tenantID, sessionID, oldJTI,
		)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrRefreshReuse
		}
		return nil
	})
}

// ActiveCount returns the count of live sessions for the tenant.
// Used by Create to enforce the per-tenant cap.
func (s *PGSessionStore) ActiveCount(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := withTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM sessions
			  WHERE tenant_id = $1 AND revoked_at IS NULL AND expires_at > now()`,
			tenantID,
		).Scan(&n)
	})
	return n, err
}

// maxSessions consults the tenants.quota JSONB for a max_sessions
// override. Any parse failure falls back to DefaultMaxSessions so a
// bad quota config never locks the tenant out.
func (s *PGSessionStore) maxSessions(ctx context.Context, tenantID uuid.UUID) int {
	if s.tenantQuota == nil {
		return DefaultMaxSessions
	}
	raw, err := s.tenantQuota(ctx, tenantID)
	if err != nil || len(raw) == 0 {
		return DefaultMaxSessions
	}
	var wrapper struct {
		MaxSessions int `json:"max_sessions"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return DefaultMaxSessions
	}
	if wrapper.MaxSessions <= 0 {
		return DefaultMaxSessions
	}
	return wrapper.MaxSessions
}
