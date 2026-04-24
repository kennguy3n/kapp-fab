package helpdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// slaWarningThreshold is the fraction of the SLA target at which a
// *_warning log is emitted. 80% matches ERPNext's default. Expressed
// as a ratio so integer-minute math stays exact: if the target is
// 60 minutes and the threshold is 0.8, warnings fire at the 48-minute
// mark (60 * 8 / 10 == 48).
const (
	slaWarningNumerator   = 8
	slaWarningDenominator = 10
)

// ticketSLARow is the per-ticket projection the sweep pulls from the
// helpdesk.ticket KRecord. Nullable time pointers are used so tickets
// without an SLA policy attached (policy lookup failed, or the
// priority had no active policy) are naturally skipped — the sweep
// filters them out in SQL but double-checks in Go.
type ticketSLARow struct {
	TicketID       uuid.UUID
	CreatedAt      time.Time
	Subject        string
	Priority       string
	Status         string
	ResponseBy     *time.Time
	ResolutionBy   *time.Time
	FirstResponded *time.Time
	ResolvedAt     *time.Time
	SLAPolicyID    string
}

// slaBreachHandler implements scheduler.ActionHandler for
// action_type=sla_breach_check. On each dispatch it:
//
//  1. Loads every open helpdesk.ticket KRecord for the action's
//     tenant whose sla_response_by or sla_resolution_by is set.
//  2. For each ticket, compares the current wall clock against the
//     response and resolution targets.
//  3. Emits response_warning / response_breach / resolution_warning
//     / resolution_breach log rows via helpdesk.Store.LogSLAEvent,
//     guarding against duplicates by consulting ListTicketLog first.
//  4. Fans a `helpdesk.sla_breach` notification event through the
//     outbox so the existing router delivers it to KChat / email.
//
// Every read + write for a given tenant runs inside a single
// dbutil.WithTenantTx so RLS stays enforced even though the parent
// scheduler loop polls via the admin pool.
type SLABreachHandler struct {
	pool      *pgxpool.Pool
	helpdesk  *Store
	publisher events.Publisher
	setTenant func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error
	now       func() time.Time
}

// NewSLABreachHandler wires a handler against the shared pool and the
// helpdesk store. publisher may be nil in tests — the handler then
// skips the outbox emit and only writes the SLA log row.
func NewSLABreachHandler(
	pool *pgxpool.Pool,
	hd *Store,
	publisher events.Publisher,
	setTenant func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error,
) *SLABreachHandler {
	return &SLABreachHandler{
		pool:      pool,
		helpdesk:  hd,
		publisher: publisher,
		setTenant: setTenant,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// WithClock substitutes the handler's time source for deterministic
// tests. Calling code that leaves it unset falls back to time.Now UTC.
func (h *SLABreachHandler) WithClock(now func() time.Time) *SLABreachHandler {
	if now != nil {
		h.now = now
	}
	return h
}

// Handle is invoked by scheduler.RunLoop. The action.Payload is
// ignored in the current contract — every fire scans the full open
// ticket set for the tenant. Future payload fields could narrow the
// scan (e.g. per-priority, per-assignee) but a full sweep stays
// cheap at SME scale because sla_response_by is indexable and open
// ticket counts stay bounded.
func (h *SLABreachHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if h == nil || h.pool == nil {
		return errors.New("worker: sla breach handler not wired")
	}
	tickets, err := h.loadOpenTickets(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("worker: sla breach load tickets: %w", err)
	}
	now := h.now()
	for _, tkt := range tickets {
		if err := h.evaluateTicket(ctx, tenantID, tkt, now); err != nil {
			// A single misbehaving ticket must not abort the sweep.
			// Log-and-continue matches the notification router's
			// contract and keeps the sweeper resilient.
			log.Printf("worker: sla breach tenant=%s ticket=%s: %v",
				tenantID, tkt.TicketID, err)
			continue
		}
	}
	return nil
}

// loadOpenTickets pulls every open helpdesk.ticket with at least one
// SLA target set. Closed / resolved tickets drop out so once a ticket
// is settled it stops producing breach events.
func (h *SLABreachHandler) loadOpenTickets(ctx context.Context, tenantID uuid.UUID) ([]ticketSLARow, error) {
	var out []ticketSLARow
	err := dbutil.WithTenantTx(ctx, h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id,
			        created_at,
			        COALESCE(data->>'subject',''),
			        COALESCE(data->>'priority',''),
			        COALESCE(data->>'status',''),
			        NULLIF(data->>'sla_response_by','')::timestamptz,
			        NULLIF(data->>'sla_resolution_by','')::timestamptz,
			        NULLIF(data->>'first_responded_at','')::timestamptz,
			        NULLIF(data->>'resolved_at','')::timestamptz,
			        COALESCE(data->>'sla_policy_id','')
			   FROM krecords
			  WHERE tenant_id = $1 AND ktype = 'helpdesk.ticket'
			    AND COALESCE(data->>'status','') IN ('open','in_progress','waiting')
			    AND (NULLIF(data->>'sla_response_by','') IS NOT NULL
			         OR NULLIF(data->>'sla_resolution_by','') IS NOT NULL)
			    AND deleted_at IS NULL`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ticketSLARow
			if err := rows.Scan(
				&r.TicketID, &r.CreatedAt,
				&r.Subject, &r.Priority, &r.Status,
				&r.ResponseBy, &r.ResolutionBy,
				&r.FirstResponded, &r.ResolvedAt,
				&r.SLAPolicyID,
			); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// evaluateTicket is the per-ticket decision. It consults the ticket's
// existing SLA log (via helpdesk.Store.ListTicketLog) to stay
// idempotent across sweeps — each of the four event kinds is emitted
// at most once per ticket. The warning threshold uses integer math
// against CreatedAt + (target - CreatedAt) * 0.8 so warnings fire at
// 48 minutes when the target is 60.
func (h *SLABreachHandler) evaluateTicket(ctx context.Context, tenantID uuid.UUID, t ticketSLARow, now time.Time) error {
	seen, err := h.seenEvents(ctx, tenantID, t.TicketID)
	if err != nil {
		return err
	}
	// Response SLA — only evaluate while the first response has not
	// landed. Once first_responded_at is set, the response window is
	// closed for warning / breach purposes.
	if t.ResponseBy != nil && t.FirstResponded == nil {
		warnAt := warningAt(t.CreatedAt, *t.ResponseBy)
		if !seen[EventResponseBreach] && !now.Before(*t.ResponseBy) {
			if err := h.emitEvent(ctx, tenantID, t, EventResponseBreach, *t.ResponseBy, now); err != nil {
				return err
			}
		} else if !seen[EventResponseWarning] && !now.Before(warnAt) && now.Before(*t.ResponseBy) {
			if err := h.emitEvent(ctx, tenantID, t, EventResponseWarning, warnAt, now); err != nil {
				return err
			}
		}
	}
	// Resolution SLA — only evaluate while the ticket has not been
	// resolved. Resolved tickets drop out of loadOpenTickets already
	// via the status filter, but the resolved_at guard is kept here
	// so a stale "open" status with resolved_at set still short-circuits.
	if t.ResolutionBy != nil && t.ResolvedAt == nil {
		warnAt := warningAt(t.CreatedAt, *t.ResolutionBy)
		if !seen[EventResolutionBreach] && !now.Before(*t.ResolutionBy) {
			if err := h.emitEvent(ctx, tenantID, t, EventResolutionBreach, *t.ResolutionBy, now); err != nil {
				return err
			}
		} else if !seen[EventResolutionWarning] && !now.Before(warnAt) && now.Before(*t.ResolutionBy) {
			if err := h.emitEvent(ctx, tenantID, t, EventResolutionWarning, warnAt, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// warningAt computes the 80%-of-window mark between createdAt and
// dueAt. Using the created_at reference (rather than simply dueAt
// minus a fraction of the original minute target) keeps the math
// correct when a due time gets adjusted after the fact.
func warningAt(createdAt, dueAt time.Time) time.Time {
	window := dueAt.Sub(createdAt)
	if window <= 0 {
		return dueAt
	}
	scaled := time.Duration(int64(window) * slaWarningNumerator / slaWarningDenominator)
	return createdAt.Add(scaled)
}

// seenEvents projects the ticket's SLA log into a set keyed by the
// four event kinds. Duplicate-prevention is idempotent: a ticket that
// has already fired a warning keeps firing warnings off the log only
// once per sweep, because the second call sees the prior row.
func (h *SLABreachHandler) seenEvents(ctx context.Context, tenantID, ticketID uuid.UUID) (map[string]bool, error) {
	log, err := h.helpdesk.ListTicketLog(ctx, tenantID, ticketID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, 4)
	for _, e := range log {
		out[e.EventKind] = true
	}
	return out, nil
}

// emitEvent logs the SLA event and publishes a notification-shaped
// outbox event in a single tenant-scoped transaction. The router
// downstream decodes the `notification` envelope and fans out to
// KChat / email according to the tenant's notification prefs.
func (h *SLABreachHandler) emitEvent(
	ctx context.Context,
	tenantID uuid.UUID,
	t ticketSLARow,
	kind string,
	occurredAt time.Time,
	now time.Time,
) error {
	detail := map[string]any{
		"fired_at":      now.Format(time.RFC3339Nano),
		"due_at":        occurredAt.Format(time.RFC3339Nano),
		"subject":       t.Subject,
		"priority":      t.Priority,
		"sla_policy_id": t.SLAPolicyID,
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshal sla detail: %w", err)
	}
	if _, err := h.helpdesk.LogSLAEvent(ctx, SLALogEntry{
		TenantID:   tenantID,
		TicketID:   t.TicketID,
		EventKind:  kind,
		OccurredAt: now,
		Details:    detailJSON,
	}); err != nil {
		return fmt.Errorf("log sla event: %w", err)
	}
	if h.publisher == nil {
		return nil
	}
	title, body := slaNotificationCopy(kind, t)
	eventPayload, err := json.Marshal(map[string]any{
		"tenant_id":  tenantID,
		"ticket_id":  t.TicketID,
		"event_kind": kind,
		"due_at":     occurredAt.Format(time.RFC3339Nano),
		"subject":    t.Subject,
		"priority":   t.Priority,
		"notification": map[string]any{
			"channel": "kchat",
			"title":   title,
			"body":    body,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal sla event: %w", err)
	}
	return h.publishEvent(ctx, tenantID, eventPayload)
}

// publishEvent opens a short-lived tx, sets the tenant GUC, and emits
// a `helpdesk.sla_breach` outbox row. The outbox row is durable so the
// notification router picks it up on its next drain.
func (h *SLABreachHandler) publishEvent(ctx context.Context, tenantID uuid.UUID, payload []byte) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin event tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if h.setTenant != nil {
		if err := h.setTenant(ctx, tx, tenantID); err != nil {
			return fmt.Errorf("set tenant for event: %w", err)
		}
	}
	if err := h.publisher.EmitTx(ctx, tx, events.Event{
		TenantID: tenantID,
		Type:     "helpdesk.sla_breach",
		Payload:  payload,
	}); err != nil {
		return fmt.Errorf("emit sla event: %w", err)
	}
	return tx.Commit(ctx)
}

// slaNotificationCopy generates the KChat card title + body for each
// event kind. Kept close to the emit path so the wording stays aligned
// with what gets written to the log.
func slaNotificationCopy(kind string, t ticketSLARow) (title, body string) {
	subject := t.Subject
	if subject == "" {
		subject = t.TicketID.String()
	}
	switch kind {
	case EventResponseWarning:
		return fmt.Sprintf("SLA warning: response on %q", subject),
			fmt.Sprintf("Ticket %q (%s) is at 80%% of its response SLA. Respond soon to avoid a breach.", subject, t.Priority)
	case EventResponseBreach:
		return fmt.Sprintf("SLA breach: response on %q", subject),
			fmt.Sprintf("Ticket %q (%s) has missed its response SLA.", subject, t.Priority)
	case EventResolutionWarning:
		return fmt.Sprintf("SLA warning: resolution on %q", subject),
			fmt.Sprintf("Ticket %q (%s) is at 80%% of its resolution SLA. Resolve soon to avoid a breach.", subject, t.Priority)
	case EventResolutionBreach:
		return fmt.Sprintf("SLA breach: resolution on %q", subject),
			fmt.Sprintf("Ticket %q (%s) has missed its resolution SLA.", subject, t.Priority)
	}
	return fmt.Sprintf("SLA event: %s", subject), kind
}
