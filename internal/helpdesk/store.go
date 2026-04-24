package helpdesk

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

// Priority enums match the ticket schema and the CHECK constraint on
// sla_policies.priority in migrations/000018_helpdesk.sql.
const (
	PriorityLow    = "low"
	PriorityMedium = "medium"
	PriorityHigh   = "high"
	PriorityUrgent = "urgent"
)

// SLAPolicy is a per-tenant (priority → targets) record. Response and
// resolution targets are stored as minute counts so the evaluator can
// add them to the ticket's created_at without ambiguity.
type SLAPolicy struct {
	TenantID          uuid.UUID  `json:"tenant_id"`
	ID                uuid.UUID  `json:"id"`
	Name              string     `json:"name"`
	Priority          string     `json:"priority"`
	ResponseMinutes   int        `json:"response_minutes"`
	ResolutionMinutes int        `json:"resolution_minutes"`
	Active            bool       `json:"active"`
	CreatedBy         *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// SLAEventKind enumerates the breach-log `event_kind` values.
const (
	EventResponseWarning   = "response_warning"
	EventResponseBreach    = "response_breach"
	EventResolutionWarning = "resolution_warning"
	EventResolutionBreach  = "resolution_breach"
)

// ActionTypeSLABreach is the scheduled_actions.action_type the worker
// registers its SLA breach handler under. Lives in this package so
// the tenant wizard and the worker reference the same literal.
const ActionTypeSLABreach = "sla_breach_check"

// DefaultSLABreachIntervalSeconds is the cadence the tenant wizard
// seeds for sla_breach_check — once every five minutes, matching the
// granularity ERPNext uses for its auto-escalation scheduler.
const DefaultSLABreachIntervalSeconds = 300

// SLALogEntry is one row of the append-only ticket_sla_log.
type SLALogEntry struct {
	ID         int64           `json:"id"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	TicketID   uuid.UUID       `json:"ticket_id"`
	EventKind  string          `json:"event_kind"`
	OccurredAt time.Time       `json:"occurred_at"`
	Details    json.RawMessage `json:"details"`
}

// Store owns the typed helpdesk tables (sla_policies + ticket_sla_log).
// Ticket records themselves live in the generic KRecord store behind
// the helpdesk.ticket KType.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewStore wires a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

// Sentinel errors surfaced to API callers as 404 / 409.
var (
	ErrPolicyNotFound = errors.New("helpdesk: sla policy not found")
)

func validPriority(p string) bool {
	switch p {
	case PriorityLow, PriorityMedium, PriorityHigh, PriorityUrgent:
		return true
	}
	return false
}

// UpsertPolicy inserts or replaces a policy. When `active` flips from
// true to false, the per-(tenant, priority, active) unique index drops
// it from the lookup set so another active policy can take its place.
func (s *Store) UpsertPolicy(ctx context.Context, policy SLAPolicy) (*SLAPolicy, error) {
	if policy.TenantID == uuid.Nil {
		return nil, errors.New("helpdesk: tenant id required")
	}
	if policy.Name == "" {
		return nil, errors.New("helpdesk: policy name required")
	}
	if !validPriority(policy.Priority) {
		return nil, fmt.Errorf("helpdesk: invalid priority %q", policy.Priority)
	}
	if policy.ResponseMinutes <= 0 || policy.ResolutionMinutes <= 0 {
		return nil, errors.New("helpdesk: response and resolution minutes must be positive")
	}
	if policy.ID == uuid.Nil {
		policy.ID = uuid.New()
	}
	out := policy
	err := dbutil.WithTenantTx(ctx, s.pool, policy.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if policy.CreatedBy != nil {
			createdBy = *policy.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO sla_policies
			     (tenant_id, id, name, priority, response_minutes, resolution_minutes, active, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET
			     name = EXCLUDED.name,
			     priority = EXCLUDED.priority,
			     response_minutes = EXCLUDED.response_minutes,
			     resolution_minutes = EXCLUDED.resolution_minutes,
			     active = EXCLUDED.active,
			     updated_at = now()
			 RETURNING created_at, updated_at`,
			policy.TenantID, policy.ID, policy.Name, policy.Priority,
			policy.ResponseMinutes, policy.ResolutionMinutes, policy.Active, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("helpdesk: upsert policy: %w", err)
	}
	return &out, nil
}

// ListPolicies returns every policy for a tenant ordered by priority
// (urgent first) so the UI's table matches standard helpdesk layouts.
func (s *Store) ListPolicies(ctx context.Context, tenantID uuid.UUID) ([]SLAPolicy, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("helpdesk: tenant id required")
	}
	out := make([]SLAPolicy, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, name, priority, response_minutes, resolution_minutes,
			        active, created_by, created_at, updated_at
			 FROM sla_policies WHERE tenant_id = $1
			 ORDER BY CASE priority
			              WHEN 'urgent' THEN 0
			              WHEN 'high'   THEN 1
			              WHEN 'medium' THEN 2
			              ELSE 3 END,
			          name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p SLAPolicy
			var createdBy *uuid.UUID
			if err := rows.Scan(
				&p.TenantID, &p.ID, &p.Name, &p.Priority,
				&p.ResponseMinutes, &p.ResolutionMinutes, &p.Active,
				&createdBy, &p.CreatedAt, &p.UpdatedAt,
			); err != nil {
				return err
			}
			p.CreatedBy = createdBy
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("helpdesk: list policies: %w", err)
	}
	return out, nil
}

// ResolvePolicy returns the active policy for a (tenant, priority)
// pair or ErrPolicyNotFound if none exists. Used by the SLA evaluator
// at ticket-create time to compute response_by / resolution_by.
func (s *Store) ResolvePolicy(ctx context.Context, tenantID uuid.UUID, priority string) (*SLAPolicy, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("helpdesk: tenant id required")
	}
	if !validPriority(priority) {
		return nil, fmt.Errorf("helpdesk: invalid priority %q", priority)
	}
	var out SLAPolicy
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, name, priority, response_minutes, resolution_minutes,
			        active, created_by, created_at, updated_at
			 FROM sla_policies
			 WHERE tenant_id = $1 AND priority = $2 AND active
			 ORDER BY updated_at DESC
			 LIMIT 1`,
			tenantID, priority,
		).Scan(
			&out.TenantID, &out.ID, &out.Name, &out.Priority,
			&out.ResponseMinutes, &out.ResolutionMinutes, &out.Active,
			&createdBy, &out.CreatedAt, &out.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPolicyNotFound
			}
			return err
		}
		out.CreatedBy = createdBy
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ComputeDueTimes applies the policy's minute targets to a base time
// (usually the ticket's created_at) and returns (response_by,
// resolution_by). Minutes + time arithmetic in one place so the KType
// handler, agent tool, and SLA worker all derive the same targets.
func ComputeDueTimes(policy SLAPolicy, base time.Time) (responseBy, resolutionBy time.Time) {
	base = base.UTC()
	responseBy = base.Add(time.Duration(policy.ResponseMinutes) * time.Minute)
	resolutionBy = base.Add(time.Duration(policy.ResolutionMinutes) * time.Minute)
	return
}

// LogSLAEvent appends a row to ticket_sla_log. Used by the worker
// that watches due times tick past — the caller decides whether the
// event is a warning (approaching) or a breach (past due).
func (s *Store) LogSLAEvent(ctx context.Context, entry SLALogEntry) (*SLALogEntry, error) {
	if entry.TenantID == uuid.Nil || entry.TicketID == uuid.Nil {
		return nil, errors.New("helpdesk: tenant id and ticket id required")
	}
	if entry.EventKind == "" {
		return nil, errors.New("helpdesk: event_kind required")
	}
	if len(entry.Details) == 0 {
		entry.Details = json.RawMessage(`{}`)
	}
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = s.now()
	}
	out := entry
	err := dbutil.WithTenantTx(ctx, s.pool, entry.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO ticket_sla_log (tenant_id, ticket_id, event_kind, occurred_at, details)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id`,
			entry.TenantID, entry.TicketID, entry.EventKind, entry.OccurredAt, entry.Details,
		).Scan(&out.ID)
	})
	if err != nil {
		return nil, fmt.Errorf("helpdesk: log sla event: %w", err)
	}
	return &out, nil
}

// ListTicketLog returns every SLA log row for a ticket ordered newest
// first. The HelpdeskPage renders this on the ticket detail pane.
func (s *Store) ListTicketLog(ctx context.Context, tenantID, ticketID uuid.UUID) ([]SLALogEntry, error) {
	if tenantID == uuid.Nil || ticketID == uuid.Nil {
		return nil, errors.New("helpdesk: tenant id and ticket id required")
	}
	out := make([]SLALogEntry, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, ticket_id, event_kind, occurred_at, details
			 FROM ticket_sla_log
			 WHERE tenant_id = $1 AND ticket_id = $2
			 ORDER BY occurred_at DESC`,
			tenantID, ticketID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e SLALogEntry
			if err := rows.Scan(&e.ID, &e.TenantID, &e.TicketID, &e.EventKind, &e.OccurredAt, &e.Details); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("helpdesk: list ticket log: %w", err)
	}
	return out, nil
}
