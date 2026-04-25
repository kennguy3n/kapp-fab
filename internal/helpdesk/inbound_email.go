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
type InboundEmailHandler struct {
	resolver TenantResolver
	records  *record.PGStore
	store    *Store
	// systemActor stamps CreatedBy on the synthetic ticket. The
	// API and worker use the same constant the recurring-invoice
	// handler does so audit attribution stays consistent across
	// machine-driven record creation.
	systemActor uuid.UUID
}

// NewInboundEmailHandler wires the dependencies. Resolver and records
// are required; passing nil panics so misconfiguration is caught at
// boot time rather than the first inbound email.
func NewInboundEmailHandler(resolver TenantResolver, records *record.PGStore, store *Store, systemActor uuid.UUID) *InboundEmailHandler {
	if resolver == nil || records == nil || store == nil {
		panic("helpdesk: inbound email handler requires non-nil deps")
	}
	if systemActor == uuid.Nil {
		panic("helpdesk: inbound email handler requires non-nil systemActor")
	}
	return &InboundEmailHandler{resolver: resolver, records: records, store: store, systemActor: systemActor}
}

// Process resolves the tenant, computes SLA targets via ResolvePolicy,
// and creates the helpdesk.ticket KRecord. Returns the persisted
// record so the API layer can echo the ticket id + SLA timestamps.
func (h *InboundEmailHandler) Process(ctx context.Context, email InboundEmail) (*record.KRecord, error) {
	if strings.TrimSpace(email.Subject) == "" {
		return nil, fmt.Errorf("%w: subject required", ErrInvalidEmail)
	}
	if strings.TrimSpace(email.BodyText) == "" {
		return nil, fmt.Errorf("%w: body required", ErrInvalidEmail)
	}
	if email.ReceivedAt.IsZero() {
		email.ReceivedAt = time.Now().UTC()
	}
	tenantID, err := h.resolver.ResolveByRecipient(ctx, email.To)
	if err != nil {
		return nil, ErrUnknownRecipient
	}

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
