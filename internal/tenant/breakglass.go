package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BreakGlassEntry mirrors a row in the admin_audit_log table
// (migrations/000103_admin_roles_split.sql). Every break-glass action
// — opening a session, executing a query, closing a session — records
// one entry so the platform retains an immutable, append-only trail of
// who bypassed RLS, why, for how long, and against which tenant.
type BreakGlassEntry struct {
	ID           int64           `json:"id"`
	OccurredAt   time.Time       `json:"occurred_at"`
	OperatorID   *uuid.UUID      `json:"operator_id,omitempty"`
	OperatorKind string          `json:"operator_kind"`
	Role         string          `json:"role"`
	ReasonCode   string          `json:"reason_code"`
	TargetTenant *uuid.UUID      `json:"target_tenant,omitempty"`
	TargetTable  string          `json:"target_table,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	ApprovedBy   *uuid.UUID      `json:"approved_by,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
}

// BreakGlassSession represents an active break-glass access window.
// A session is opened with a reason code, an expiry, and an optional
// approver; it is recorded in admin_audit_log and can be listed /
// inspected by operators. The session itself is stateless beyond the
// audit row — the runtime does not hold open a connection for the
// duration; each break-glass request re-authenticates and re-logs.
type BreakGlassSession struct {
	Entry BreakGlassEntry `json:"entry"`
	// Active reports whether the session has not yet expired. Computed
	// from Entry.ExpiresAt at read time; not stored.
	Active bool `json:"active"`
}

// BreakGlassStore writes and reads admin_audit_log rows. It uses the
// admin pool (kapp_admin or kapp_admin_readonly for reads) — NOT the
// kapp_breakglass role, which is reserved for the actual BYPASSRLS
// data access. The store itself never bypasses RLS; it only records
// that someone else did.
type BreakGlassStore struct {
	pool *pgxpool.Pool
}

// NewBreakGlassStore constructs a BreakGlassStore bound to the admin
// pool. The pool must connect as kapp_admin (or a role with INSERT +
// SELECT on admin_audit_log).
func NewBreakGlassStore(pool *pgxpool.Pool) *BreakGlassStore {
	return &BreakGlassStore{pool: pool}
}

// ErrReasonRequired is returned when a break-glass session is opened
// without a reason code. The reason code is the core accountability
// mechanism — without it the audit trail is meaningless.
var ErrReasonRequired = errors.New("breakglass: reason_code required")

// ErrExpiryRequired is returned when a break-glass session is opened
// without an expiry. A break-glass session must be time-boxed so an
// abandoned session does not grant indefinite access.
var ErrExpiryRequired = errors.New("breakglass: expires_at required")

// ErrExpiryTooFar is returned when a break-glass session expiry
// exceeds MaxBreakGlassDuration. The cap keeps the blast radius of a
// leaked session token bounded.
var ErrExpiryTooFar = errors.New("breakglass: expires_at exceeds maximum duration")

// MaxBreakGlassDuration is the longest a single break-glass session
// may last. 4 hours is the default; operators can tune this per
// deployment by adjusting the constant. The cap is enforced in
// OpenSession so even a compromised admin credential cannot mint a
// permanent break-glass session.
const MaxBreakGlassDuration = 4 * time.Hour

// OpenSession records a new break-glass session in admin_audit_log.
// The caller must supply a non-empty reason code and an expiry that is
// (a) in the future and (b) within MaxBreakGlassDuration of now.
// approvedBy is optional — single-approver deployments pass nil; a
// two-person-policy deployment requires a non-nil approver.
//
// The session is recorded immediately; there is no "pending" state.
// The runtime that drives the actual BYPASSRLS data access is
// expected to check that an active session exists (via ListActive)
// before opening a kapp_breakglass connection.
func (s *BreakGlassStore) OpenSession(ctx context.Context, entry BreakGlassEntry) (*BreakGlassSession, error) {
	if entry.ReasonCode == "" {
		return nil, ErrReasonRequired
	}
	if entry.ExpiresAt == nil {
		return nil, ErrExpiryRequired
	}
	now := time.Now().UTC()
	if entry.ExpiresAt.Before(now) {
		return nil, fmt.Errorf("breakglass: expires_at must be in the future")
	}
	if entry.ExpiresAt.Sub(now) > MaxBreakGlassDuration {
		return nil, ErrExpiryTooFar
	}
	if entry.OperatorKind == "" {
		entry.OperatorKind = "user"
	}
	if entry.Role == "" {
		entry.Role = "kapp_breakglass"
	}
	if len(entry.Metadata) == 0 {
		entry.Metadata = json.RawMessage("{}")
	}

	var id int64
	var occurredAt time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO admin_audit_log
		     (operator_id, operator_kind, role, reason_code, target_tenant,
		      target_table, expires_at, approved_by, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, occurred_at`,
		entry.OperatorID, entry.OperatorKind, entry.Role, entry.ReasonCode,
		entry.TargetTenant, nullIfEmptyStr(entry.TargetTable),
		entry.ExpiresAt, entry.ApprovedBy, entry.Metadata,
	).Scan(&id, &occurredAt)
	if err != nil {
		return nil, fmt.Errorf("breakglass: insert session: %w", err)
	}
	entry.ID = id
	entry.OccurredAt = occurredAt
	return &BreakGlassSession{Entry: entry, Active: true}, nil
}

// ListSessions returns break-glass sessions ordered by most recent
// first, up to `limit` rows. When targetTenant is non-nil, only
// sessions targeting that tenant are returned. limit <= 0 defaults to
// 50.
func (s *BreakGlassStore) ListSessions(ctx context.Context, targetTenant *uuid.UUID, limit int) ([]BreakGlassSession, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, occurred_at, operator_id, operator_kind, role, reason_code,
	                 target_tenant, target_table, expires_at, approved_by, metadata
	            FROM admin_audit_log`
	args := []any{}
	if targetTenant != nil {
		query += ` WHERE target_tenant = $1`
		args = append(args, *targetTenant)
		query += fmt.Sprintf(` ORDER BY occurred_at DESC LIMIT $%d`, len(args)+1)
		args = append(args, limit)
	} else {
		query += fmt.Sprintf(` ORDER BY occurred_at DESC LIMIT $1`)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("breakglass: list sessions: %w", err)
	}
	defer rows.Close()
	var out []BreakGlassSession
	now := time.Now().UTC()
	for rows.Next() {
		var e BreakGlassEntry
		if err := rows.Scan(
			&e.ID, &e.OccurredAt, &e.OperatorID, &e.OperatorKind, &e.Role,
			&e.ReasonCode, &e.TargetTenant, &e.TargetTable, &e.ExpiresAt,
			&e.ApprovedBy, &e.Metadata,
		); err != nil {
			return nil, fmt.Errorf("breakglass: scan session: %w", err)
		}
		active := e.ExpiresAt != nil && e.ExpiresAt.After(now)
		out = append(out, BreakGlassSession{Entry: e, Active: active})
	}
	return out, rows.Err()
}

// ListActive returns sessions that have not yet expired. This is the
// query the runtime BYPASSRLS gateway calls before opening a
// kapp_breakglass connection: if no active session exists for the
// target tenant, access is refused.
func (s *BreakGlassStore) ListActive(ctx context.Context, targetTenant *uuid.UUID) ([]BreakGlassSession, error) {
	all, err := s.ListSessions(ctx, targetTenant, 200)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var active []BreakGlassSession
	for _, s := range all {
		if s.Entry.ExpiresAt != nil && s.Entry.ExpiresAt.After(now) {
			active = append(active, s)
		}
	}
	return active, nil
}

// LogAction records a single break-glass action (e.g. a specific
// query or data access) in admin_audit_log without opening a full
// session. Used for granular per-action auditing within an active
// session window.
func (s *BreakGlassStore) LogAction(ctx context.Context, entry BreakGlassEntry) error {
	if entry.ReasonCode == "" {
		return ErrReasonRequired
	}
	if entry.OperatorKind == "" {
		entry.OperatorKind = "user"
	}
	if entry.Role == "" {
		entry.Role = "kapp_breakglass"
	}
	if len(entry.Metadata) == 0 {
		entry.Metadata = json.RawMessage("{}")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO admin_audit_log
		     (operator_id, operator_kind, role, reason_code, target_tenant,
		      target_table, expires_at, approved_by, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		entry.OperatorID, entry.OperatorKind, entry.Role, entry.ReasonCode,
		entry.TargetTenant, nullIfEmptyStr(entry.TargetTable),
		entry.ExpiresAt, entry.ApprovedBy, entry.Metadata,
	)
	if err != nil {
		return fmt.Errorf("breakglass: log action: %w", err)
	}
	return nil
}

func nullIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
