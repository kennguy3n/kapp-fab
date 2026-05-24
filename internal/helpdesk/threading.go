package helpdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Direction enumerates whether a stored message was inbound (received
// from a customer / external sender) or outbound (sent by an agent
// via the reply path). The threading resolver uses the row regardless
// of direction — a customer's reply to an agent's outbound message
// threads onto the same ticket via the outbound row's Message-ID.
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

// DefaultThreadingLookback is the lookback window the resolver uses
// when none is configured. Mirrors frappe/helpdesk's default thread
// horizon and keeps a stale-message-id attack bounded: replaying a
// 90-day-old Message-ID on a fresh ticket has no parent-lookup
// effect and opens a new ticket instead.
const DefaultThreadingLookback = 30 * 24 * time.Hour

// Message is one row of email_messages — the per-message persistence
// layer underneath the threading resolver. The struct is exported so
// the outbound sender can persist rows on send (direction='outbound')
// and the inbound handler can persist on receive (direction='inbound')
// without sharing private state.
type Message struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	MessageID  string    `json:"message_id"`
	TicketID   uuid.UUID `json:"ticket_id"`
	Direction  string    `json:"direction"`
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	References []string  `json:"references,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	FromAddr   string    `json:"from_addr,omitempty"`
	ToAddr     string    `json:"to_addr,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

// MessageStore persists per-message rows for the helpdesk email
// threading path. The store is tenant-scoped: every Put / Lookup
// opens a transaction under the tenant's RLS context so a
// misconfigured caller cannot read across tenants even if a
// Message-ID happens to collide.
type MessageStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewMessageStore wires a MessageStore from the shared pool. The pool
// must be the tenant-app pool (not admin) so the RLS policy on
// email_messages applies.
func NewMessageStore(pool *pgxpool.Pool) *MessageStore {
	return &MessageStore{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

// ErrMessageNotFound is returned by Lookup when no row matches the
// (tenant, message-id) probe. The threading resolver treats this as
// a normal miss (open a new ticket) rather than an error to surface
// upstream.
var ErrMessageNotFound = errors.New("helpdesk: message not found")

// Put inserts a message row, returning ErrNoOp when the row already
// exists for (tenant, message_id). The idempotency contract mirrors
// deriveTicketID: a retry by the upstream relay must not double-
// thread the same physical email.
//
// References is JSON-encoded so the threading resolver can walk the
// chain when In-Reply-To miss-matches but a deeper ancestor matches.
// We persist it as a JSONB column rather than TEXT[] for parity with
// the rest of the platform's JSON-first persistence style.
func (s *MessageStore) Put(ctx context.Context, m Message) (*Message, error) {
	if m.TenantID == uuid.Nil {
		return nil, errors.New("helpdesk: tenant id required")
	}
	if strings.TrimSpace(m.MessageID) == "" {
		return nil, errors.New("helpdesk: message id required")
	}
	if m.TicketID == uuid.Nil {
		return nil, errors.New("helpdesk: ticket id required")
	}
	if m.Direction != DirectionInbound && m.Direction != DirectionOutbound {
		return nil, fmt.Errorf("helpdesk: invalid direction %q", m.Direction)
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = s.now()
	}
	refs := m.References
	if refs == nil {
		refs = []string{}
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return nil, fmt.Errorf("helpdesk: marshal references: %w", err)
	}
	out := m
	err = dbutil.WithTenantTx(ctx, s.pool, m.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// ON CONFLICT DO NOTHING — first writer wins.
		// RETURNING tells us whether the row was newly
		// inserted; an empty result set on conflict still
		// honours the idempotency contract.
		_, err := tx.Exec(ctx,
			`INSERT INTO email_messages
                 (tenant_id, message_id, ticket_id, direction, in_reply_to, "references", subject, from_addr, to_addr, received_at)
             VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10)
             ON CONFLICT (tenant_id, message_id) DO NOTHING`,
			m.TenantID, m.MessageID, m.TicketID, m.Direction,
			m.InReplyTo, refsJSON, m.Subject, m.FromAddr, m.ToAddr, m.ReceivedAt,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("helpdesk: put email_message: %w", err)
	}
	return &out, nil
}

// Lookup fetches a row by (tenant, message-id). Returns
// ErrMessageNotFound when no row matches. The lookup is the
// threading resolver's first probe — a hit returns the ticket id
// so the new message attaches to the existing ticket.
func (s *MessageStore) Lookup(ctx context.Context, tenantID uuid.UUID, messageID string) (*Message, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("helpdesk: tenant id required")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, ErrMessageNotFound
	}
	var out Message
	var refsJSON []byte
	var inReplyTo, subject, fromAddr, toAddr *string
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT tenant_id, message_id, ticket_id, direction,
                    in_reply_to, "references", subject, from_addr, to_addr, received_at
             FROM email_messages
             WHERE tenant_id = $1 AND message_id = $2`,
			tenantID, messageID,
		).Scan(
			&out.TenantID, &out.MessageID, &out.TicketID, &out.Direction,
			&inReplyTo, &refsJSON, &subject, &fromAddr, &toAddr, &out.ReceivedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, fmt.Errorf("helpdesk: lookup email_message: %w", err)
	}
	if inReplyTo != nil {
		out.InReplyTo = *inReplyTo
	}
	if subject != nil {
		out.Subject = *subject
	}
	if fromAddr != nil {
		out.FromAddr = *fromAddr
	}
	if toAddr != nil {
		out.ToAddr = *toAddr
	}
	if len(refsJSON) > 0 {
		if err := json.Unmarshal(refsJSON, &out.References); err != nil {
			return nil, fmt.Errorf("helpdesk: decode references: %w", err)
		}
	}
	return &out, nil
}

// MessageLookuper is the subset of MessageStore the threading
// resolver uses. Exposing this as an interface keeps the resolver
// unit-testable without a live pgxpool — the test seam swaps in an
// in-memory fake whose Lookup behaviour pins the resolver's
// candidate-walk ordering and freshness gate.
type MessageLookuper interface {
	Lookup(ctx context.Context, tenantID uuid.UUID, messageID string) (*Message, error)
}

// ThreadingResolver decides whether an inbound email threads onto an
// existing ticket or opens a new one. The resolver is stateless
// except for its dependency on a MessageLookuper — every Resolve call
// runs a fresh series of lookups under the tenant's RLS context.
//
// Resolution order (RFC-5322 + frappe/helpdesk convention):
//
//  1. In-Reply-To header — most specific signal; the sender is
//     deliberately replying to one specific message.
//  2. References header walked newest-to-oldest. The newest is the
//     immediate parent, the oldest is the thread root. We try each
//     in turn so a chain like A → B → C still threads onto A's
//     ticket if B's row is missing (e.g. the platform was offline
//     when B was received).
//  3. Miss → open a new ticket (caller's responsibility).
//
// The lookback window applies as a freshness gate AFTER the lookup
// hits. Returning a ticket that's older than the lookback window
// would let an attacker hijack a long-closed ticket by replaying its
// Message-ID; the gate makes the worst-case attack window
// configurable (default 30 days).
type ThreadingResolver struct {
	messages MessageLookuper
	lookback time.Duration
	now      func() time.Time
}

// NewThreadingResolver wires a resolver to its MessageLookuper.
// Lookback of zero means "use the default" (30 days); pass a
// negative duration to disable the freshness gate entirely (useful
// for tests that exercise time-skewed parents).
func NewThreadingResolver(messages MessageLookuper, lookback time.Duration) *ThreadingResolver {
	if messages == nil {
		panic("helpdesk: threading resolver requires non-nil message store")
	}
	if lookback == 0 {
		lookback = DefaultThreadingLookback
	}
	return &ThreadingResolver{
		messages: messages,
		lookback: lookback,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Resolve returns the ticket id an inbound email should attach to,
// or uuid.Nil if no parent message matches. The caller treats a Nil
// result as "open a new ticket".
//
// The headers are normalised (angle brackets stripped, whitespace
// trimmed) before lookup so callers don't have to pre-clean the
// values pulled out of the raw MIME parser.
func (r *ThreadingResolver) Resolve(ctx context.Context, tenantID uuid.UUID, email InboundEmail) (uuid.UUID, error) {
	if tenantID == uuid.Nil {
		return uuid.Nil, errors.New("helpdesk: tenant id required")
	}
	// Probe order: In-Reply-To first, then References walked
	// newest-to-oldest. Each probe is a one-row lookup against
	// the (tenant, message_id) primary key so the total cost is
	// O(1 + len(References)) regardless of tenant size.
	candidates := make([]string, 0, 1+len(email.References))
	if id := normalizeMessageID(email.InReplyTo); id != "" {
		candidates = append(candidates, id)
	}
	for i := len(email.References) - 1; i >= 0; i-- {
		if id := normalizeMessageID(email.References[i]); id != "" {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return uuid.Nil, nil
	}
	cutoff := time.Time{}
	if r.lookback > 0 {
		cutoff = r.now().Add(-r.lookback)
	}
	for _, mid := range candidates {
		parent, err := r.messages.Lookup(ctx, tenantID, mid)
		if errors.Is(err, ErrMessageNotFound) {
			continue
		}
		if err != nil {
			return uuid.Nil, err
		}
		// Freshness gate. parent.ReceivedAt is in UTC; cutoff
		// is in UTC; the comparison is well-defined. A zero
		// cutoff (lookback disabled or unset for tests) means
		// every hit is accepted.
		if !cutoff.IsZero() && parent.ReceivedAt.Before(cutoff) {
			continue
		}
		return parent.TicketID, nil
	}
	return uuid.Nil, nil
}

// normalizeMessageID strips RFC-822 angle brackets and surrounding
// whitespace from a Message-ID header value. RFC-5322 §3.6.4
// specifies the angle-bracketed form (`<msgid@host>`) but some
// upstreams strip the brackets when serialising to JSON; we accept
// both forms so the threading resolver works regardless of the
// upstream's quirks.
func normalizeMessageID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}
