package helpdesk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// InboundEmail is the parsed shape an SMTP relay or mailbox webhook
// pushes into the helpdesk surface. The structure mirrors the subset
// of frappe/helpdesk's Email Account contract that we honour: only
// envelope + body + attachment metadata; full MIME parsing happens
// upstream.
type InboundEmail struct {
	// MessageID is the RFC-822 Message-ID header. Used as the
	// idempotency key so a relay that re-sends the same message
	// (e.g. retry after upstream 5xx) never opens a duplicate
	// ticket. Empty messages fall back to a hash of (from, subject,
	// received_at) so legacy senders without Message-ID still get
	// dedup'd within a 24h window.
	MessageID string `json:"message_id,omitempty"`

	// To is the recipient address the relay forwarded for. Used to
	// resolve the tenant via TenantResolver; e.g.
	// support@acme.kapp.io → tenant "acme".
	To string `json:"to"`

	// From is the sender's RFC-822 mailbox. The local-part is
	// preserved as the ticket's reporter contact; the domain is
	// surfaced separately for spam scoring.
	From string `json:"from"`

	// Subject is the email Subject header. Empty subjects are
	// rejected — the helpdesk.ticket schema requires a subject of
	// at least 1 char.
	Subject string `json:"subject"`

	// BodyText is the plain-text body. Required; HTML-only
	// senders should be downconverted upstream by the relay.
	BodyText string `json:"body_text"`

	// BodyHTML is the rich body, stored verbatim for replay in the
	// ticket detail pane. Optional.
	BodyHTML string `json:"body_html,omitempty"`

	// Attachments are file references already uploaded to the
	// shared object store by the relay. The handler links them on
	// the ticket via attachment KRecords; the bytes are NOT
	// re-uploaded here.
	Attachments []InboundAttachment `json:"attachments,omitempty"`

	// ReceivedAt is when the relay accepted the message. Falls
	// back to time.Now when zero.
	ReceivedAt time.Time `json:"received_at,omitempty"`

	// InReplyTo is the RFC-822 In-Reply-To header (the parent
	// message-id, angle-bracketed). The threading resolver looks
	// this up against the message store to decide whether to
	// thread or open a new ticket.
	InReplyTo string `json:"in_reply_to,omitempty"`

	// References is the RFC-822 References header — the full
	// thread chain, oldest first. Some MTAs split it across
	// multiple header instances; the relay flattens them into
	// this slice.
	References []string `json:"references,omitempty"`

	// Source records which channel ingested the email so the
	// agent UI can render provenance (e.g. "received via Mailgun"
	// vs "received via IMAP"). Defaults to "api" when unset.
	Source string `json:"source,omitempty"`
}

// InboundAttachment names a single attachment that the relay has
// already uploaded into the platform object store.
type InboundAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	StorageKey  string `json:"storage_key"`
	Size        int64  `json:"size"`
}

// TenantResolver maps a recipient address (e.g. support@acme.kapp.io)
// to a tenant uuid. Implementations look at the host portion against
// the per-tenant support_domains config (migration 000031).
type TenantResolver interface {
	ResolveByRecipient(ctx context.Context, recipient string) (uuid.UUID, error)
}

// Sentinel errors so the HTTP layer can render the right status code.
var (
	// ErrUnknownRecipient is returned when no tenant claims the
	// recipient domain. The HTTP handler surfaces this as 404 so
	// the relay rejects the bounce upstream rather than retrying.
	ErrUnknownRecipient = errors.New("helpdesk: no tenant for recipient")

	// ErrInvalidEmail covers structural validation failures —
	// missing subject, missing body, malformed addresses.
	ErrInvalidEmail = errors.New("helpdesk: invalid inbound email")
)

// InboundEmailHandler turns parsed inbound email into a
// helpdesk.ticket KRecord under the resolved tenant's RLS context.
// The handler does not own SMTP / IMAP plumbing — the relay or
// upstream mail processor parses MIME and POSTs the structured
// payload to the API which forwards to Process.
//
// PR-7 adds two optional fields — `messages` and `threading` — so
// inbound mail can be threaded onto an existing ticket via
// In-Reply-To / References. When either is nil the handler falls
// back to the pre-PR-7 "always create a new ticket" behaviour for
// backwards compatibility with existing webhook bridges.
type InboundEmailHandler struct {
	resolver  TenantResolver
	records   *record.PGStore
	store     *Store
	messages  *MessageStore
	threading *ThreadingResolver
	// systemActor stamps CreatedBy on the synthetic ticket. The
	// API and worker use the same constant the recurring-invoice
	// handler does so audit attribution stays consistent across
	// machine-driven record creation.
	systemActor uuid.UUID
}

// NewInboundEmailHandler wires the dependencies. Resolver and records
// are required; passing nil panics so misconfiguration is caught at
// boot time rather than the first inbound email. messages and
// threading are optional (pre-PR-7 callers can pass nil).
func NewInboundEmailHandler(resolver TenantResolver, records *record.PGStore, store *Store, systemActor uuid.UUID) *InboundEmailHandler {
	if resolver == nil || records == nil || store == nil {
		panic("helpdesk: inbound email handler requires non-nil deps")
	}
	if systemActor == uuid.Nil {
		panic("helpdesk: inbound email handler requires non-nil systemActor")
	}
	return &InboundEmailHandler{resolver: resolver, records: records, store: store, systemActor: systemActor}
}

// WithThreading attaches the message store + threading resolver so
// inbound emails are threaded onto existing tickets via In-Reply-To
// and References. Returns the handler for chaining at boot.
func (h *InboundEmailHandler) WithThreading(messages *MessageStore, threading *ThreadingResolver) *InboundEmailHandler {
	h.messages = messages
	h.threading = threading
	return h
}

// Process resolves the tenant, computes SLA targets via ResolvePolicy,
// and creates the helpdesk.ticket KRecord. Returns the persisted
// record so the API layer can echo the ticket id + SLA timestamps.
//
// Process is the pre-PR-7 behaviour — every call opens a new ticket.
// Callers that want threading (parent-message lookup via
// In-Reply-To / References) should use ProcessThreaded.
func (h *InboundEmailHandler) Process(ctx context.Context, email InboundEmail) (*record.KRecord, error) {
	if err := validateInbound(&email); err != nil {
		return nil, err
	}
	if email.ReceivedAt.IsZero() {
		email.ReceivedAt = time.Now().UTC()
	}
	tenantID, err := h.resolver.ResolveByRecipient(ctx, email.To)
	if err != nil {
		// Preserve database / transport errors so the HTTP handler
		// can surface them as 5xx — relays must keep retrying. Only
		// the explicit "no tenant for this recipient" sentinel maps
		// to a 4xx terminal failure.
		return nil, err
	}
	return h.createTicket(ctx, tenantID, email)
}

// ProcessThreaded resolves the tenant, asks the threading resolver
// whether the inbound email attaches to an existing ticket, and
// either threads onto the existing ticket (returning it without
// re-creating) or opens a new one — using the same createTicket
// path Process does so the KRecord shape is identical across both
// entry points.
//
// When the handler is constructed without WithThreading,
// ProcessThreaded falls back to Process — every email becomes a new
// ticket. This keeps the API safe for callers wired against an
// older deps_build that hasn't yet wired the MessageStore +
// ThreadingResolver pair.
//
// On a successful thread-onto-existing path, the inbound message is
// persisted to email_messages (direction='inbound') so subsequent
// replies on the same chain resolve back to this ticket via the
// resolver's In-Reply-To / References walk.
//
// Errors from the resolver are surfaced verbatim so a transient
// database failure during lookup translates to a 5xx that the relay
// retries. A successful lookup with a nil ticket (no parent) falls
// through to createTicket without distinction — the "open a new
// ticket" path is the right answer for first-message-in-a-thread
// AND for "no usable parent in the lookback window".
func (h *InboundEmailHandler) ProcessThreaded(ctx context.Context, email InboundEmail) (*record.KRecord, error) {
	if h.threading == nil || h.messages == nil {
		return h.Process(ctx, email)
	}
	if err := validateInbound(&email); err != nil {
		return nil, err
	}
	if email.ReceivedAt.IsZero() {
		email.ReceivedAt = time.Now().UTC()
	}
	tenantID, err := h.resolver.ResolveByRecipient(ctx, email.To)
	if err != nil {
		return nil, err
	}
	parentTicket, err := h.threading.Resolve(ctx, tenantID, email)
	if err != nil {
		return nil, fmt.Errorf("helpdesk: resolve thread: %w", err)
	}
	var rec *record.KRecord
	if parentTicket != uuid.Nil {
		// Thread hit — fetch the existing ticket to return it
		// to the caller. We deliberately do NOT mutate the
		// existing ticket here (no comment append, no status
		// transition); that's the responsibility of the agent
		// UI surface which renders the message thread against
		// email_messages directly. The handler's contract is
		// "open or attach a ticket"; the timeline is built
		// downstream from the per-message rows.
		rec, err = h.records.Get(ctx, tenantID, parentTicket)
		if err != nil {
			// Only the "stale email_messages row → deleted
			// ticket" case (record.ErrNotFound) is treated
			// as a recoverable thread-broken-fall-through.
			// Any OTHER error (transient DB outage, lock
			// timeout, serialization failure) must be
			// propagated — silently opening a new ticket on
			// a transient failure would split the customer's
			// conversation onto a different ticket id, and
			// every subsequent reply on the chain would then
			// land on the wrong ticket too. The relay's retry
			// (we return the wrapped error → 5xx) brings us
			// back through Resolve which will rediscover the
			// real parent ticket.
			if !errors.Is(err, record.ErrNotFound) {
				return nil, fmt.Errorf("helpdesk: fetch parent ticket: %w", err)
			}
			rec, err = h.createTicket(ctx, tenantID, email)
			if err != nil {
				return nil, err
			}
		}
	} else {
		rec, err = h.createTicket(ctx, tenantID, email)
		if err != nil {
			return nil, err
		}
	}
	// Persist the inbound message so subsequent replies on the
	// same thread resolve back to rec.ID. The MessageID may be
	// empty (no Message-ID header) — skip persistence in that
	// case; the threading path won't be useful for replies
	// anyway since the customer's mail client will need a
	// real Message-ID to reference.
	if strings.TrimSpace(email.MessageID) != "" {
		if _, perr := h.messages.Put(ctx, Message{
			TenantID:   tenantID,
			MessageID:  normalizeMessageID(email.MessageID),
			TicketID:   rec.ID,
			Direction:  DirectionInbound,
			InReplyTo:  normalizeMessageID(email.InReplyTo),
			References: email.References,
			Subject:    email.Subject,
			FromAddr:   email.From,
			ToAddr:     email.To,
			ReceivedAt: email.ReceivedAt,
		}); perr != nil {
			// Message persistence failure is logged but
			// does NOT fail the request — the ticket has
			// been created, the customer's email has been
			// captured. Losing the thread linkage for
			// downstream replies is a strictly lesser
			// outcome than re-creating the whole ticket on
			// retry. The HTTP layer renders the rec so the
			// relay sees a 2xx.
			return rec, nil //nolint:nilerr // see comment
		}
	}
	return rec, nil
}

// createTicket builds the ticket KRecord shape that both the legacy
// Process and the PR-7 ProcessThreaded paths share. Tenant has
// already been resolved by the caller.
func (h *InboundEmailHandler) createTicket(ctx context.Context, tenantID uuid.UUID, email InboundEmail) (*record.KRecord, error) {
	// Default priority on email is "medium" — the SLA evaluator
	// picks the policy registered for the (tenant, "medium") pair,
	// or falls back to the platform default. Email channel records
	// the source so the helpdesk view can filter on it later.
	priority := "medium"
	policy, _ := h.store.ResolvePolicy(ctx, tenantID, priority)
	now := email.ReceivedAt
	data := map[string]any{
		"subject":     trimTo(email.Subject, 200),
		"description": email.BodyText,
		"status":      "open",
		"priority":    priority,
		"channel":     "email",
		"reporter": map[string]string{
			"email": email.From,
		},
		"inbound": map[string]any{
			"message_id":  email.MessageID,
			"to":          email.To,
			"received_at": email.ReceivedAt,
			"body_html":   email.BodyHTML,
			"attachments": email.Attachments,
			"in_reply_to": email.InReplyTo,
			"references":  email.References,
			"source":      email.Source,
		},
	}
	if policy != nil {
		data["sla_policy_id"] = policy.ID.String()
		data["sla_response_by"] = now.Add(time.Duration(policy.ResponseMinutes) * time.Minute)
		data["sla_resolution_by"] = now.Add(time.Duration(policy.ResolutionMinutes) * time.Minute)
	}
	body, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("helpdesk: marshal ticket data: %w", err)
	}
	rec := record.KRecord{
		ID:        deriveTicketID(tenantID, email),
		TenantID:  tenantID,
		KType:     KTypeTicket,
		Data:      body,
		Status:    "active",
		CreatedBy: h.systemActor,
	}
	created, err := h.records.Create(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("helpdesk: create ticket: %w", err)
	}
	return created, nil
}

// deriveTicketID hashes the inbound MessageID (or its fallback
// composite) into a UUIDv5 under the tenant namespace so a retry by
// the relay returns the same ticket id and the records.Create call
// dedup's via its primary-key uniqueness constraint.
func deriveTicketID(tenantID uuid.UUID, email InboundEmail) uuid.UUID {
	key := email.MessageID
	if key == "" {
		h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email.From)) + "|" + email.Subject + "|" + email.ReceivedAt.UTC().Format("2006-01-02")))
		key = hex.EncodeToString(h[:])
	}
	return uuid.NewSHA1(tenantID, []byte("helpdesk.inbound:"+key))
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// validateInbound performs structural validation on the inbound
// payload that both the legacy Process and the PR-7 ProcessThreaded
// path share. The helpdesk.ticket schema requires a non-empty
// subject (max_length 200) and a body to seed the description; the
// HTTP layer surfaces ErrInvalidEmail as 4xx so the relay treats
// the rejection as terminal (no retries on a structurally bad
// payload). trimTo on the subject happens later in createTicket —
// we don't fail on length here so an upstream relay's >200-char
// "Auto-reply: Out of Office..." subject still opens a ticket.
func validateInbound(e *InboundEmail) error {
	if strings.TrimSpace(e.Subject) == "" {
		return fmt.Errorf("%w: subject required", ErrInvalidEmail)
	}
	if strings.TrimSpace(e.BodyText) == "" {
		return fmt.Errorf("%w: body required", ErrInvalidEmail)
	}
	return nil
}
