package outbound

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// BounceKind classifies an inbound delivery-failure notification
// (DSN — RFC 3464) so the helpdesk can react accordingly:
//
//   - BounceHard: permanent — recipient mailbox doesn't exist,
//     domain doesn't resolve, etc. Action: mark the ticket
//     contact as "undeliverable" so future replies don't pile up
//     in the outbox.
//   - BounceSoft: transient — quota full, server temporarily
//     unavailable. Action: retry via the outbox retry loop;
//     no contact-level state change.
//   - BounceComplaint: recipient marked our message as spam
//     (ARF — Abuse Reporting Format). Action: same as hard
//     bounce for the contact, plus a louder log.
//   - BounceUnknown: the body looked like a DSN but we couldn't
//     parse the status code. Action: log and continue.
type BounceKind string

// BounceKind values — see the type documentation for the action
// each kind triggers in the helpdesk layer.
const (
	BounceHard      BounceKind = "hard"
	BounceSoft      BounceKind = "soft"
	BounceComplaint BounceKind = "complaint"
	BounceUnknown   BounceKind = "unknown"
)

// Bounce is the parsed outcome of a DSN. OriginalMessageID is the
// Message-ID of the message we sent that bounced — the caller uses
// this to look up the email_messages row, find the ticket, and
// post a bounce note on its timeline.
type Bounce struct {
	Kind              BounceKind
	OriginalMessageID string
	Recipient         string
	Reason            string
	Status            string // 5.1.1 / 4.2.2 / etc.
}

// BounceHandler is the entry point for the inbound webhook /
// IMAP DSN path. It parses the DSN, looks up the original outbound
// message, and records a bounce note.
type BounceHandler struct {
	store BounceStore
}

// BounceStore is the subset of operations the bounce handler needs
// against the helpdesk's database. Exposed as an interface for the
// same reason MessageRecorder is — testable without a live pool.
type BounceStore interface {
	// FindMessage returns (ticketID, found) for an outbound
	// message-id. The handler calls this to thread the bounce
	// note onto the correct ticket.
	FindMessage(ctx context.Context, tenantID uuid.UUID, messageID string) (uuid.UUID, bool, error)

	// RecordBounce writes a bounce note to the ticket. The
	// implementation may upsert into ticket_notes /
	// email_messages depending on how the agent UI wants to
	// render the audit trail.
	RecordBounce(ctx context.Context, tenantID, ticketID uuid.UUID, bounce Bounce) error
}

// NewBounceHandler wires a BounceHandler.
func NewBounceHandler(store BounceStore) *BounceHandler {
	return &BounceHandler{store: store}
}

// ErrUnparseable is returned by Handle when the input doesn't
// look like a DSN at all (which usually means the inbound
// classifier mis-routed a human reply into the bounce path).
var ErrUnparseable = errors.New("outbound: not a DSN")

// ErrNoOriginalMessage is returned when the DSN parses cleanly but
// the original Message-ID it references doesn't exist in our
// email_messages table. Common causes:
//   - Bounce arrived after the original message row was pruned.
//   - The Message-ID in the DSN was rewritten by an upstream MTA
//     and no longer matches what we recorded.
//
// The handler logs at WARN and does NOT persist a bounce note (it
// has no ticket to attach to).
var ErrNoOriginalMessage = errors.New("outbound: bounce references unknown message")

// Handle parses a DSN body + headers and persists the bounce note.
// Returns the parsed Bounce so the caller can also log it.
// Returns ErrUnparseable if the input doesn't look like a DSN at
// all.
func (h *BounceHandler) Handle(ctx context.Context, tenantID uuid.UUID, headers map[string]string, body string) (*Bounce, error) {
	bounce := ParseDSN(headers, body)
	if bounce.Kind == BounceUnknown && bounce.OriginalMessageID == "" {
		return nil, ErrUnparseable
	}
	if bounce.OriginalMessageID == "" {
		return &bounce, ErrUnparseable
	}

	ticketID, found, err := h.store.FindMessage(ctx, tenantID, bounce.OriginalMessageID)
	if err != nil {
		return &bounce, fmt.Errorf("outbound: bounce lookup: %w", err)
	}
	if !found {
		return &bounce, ErrNoOriginalMessage
	}
	if err := h.store.RecordBounce(ctx, tenantID, ticketID, bounce); err != nil {
		return &bounce, fmt.Errorf("outbound: record bounce: %w", err)
	}
	return &bounce, nil
}

// statusCodeRe extracts the RFC-3463 enhanced status code from a
// DSN body. The status appears in the "per-recipient" section as
// "Status: X.Y.Z".
var statusCodeRe = regexp.MustCompile(`(?im)^Status:\s*([245])\.(\d+)\.(\d+)`)

// originalMsgIDRe extracts the original message id. ARF reports
// (complaints) include it under "Original-Message-ID:"; standard
// DSNs include it under "Original-Envelope-Id:" or in the
// included message/rfc822 part's Message-ID header. We accept all
// three.
var originalMsgIDRe = regexp.MustCompile(`(?im)^(?:Original-Message-ID|Original-Envelope-Id|Message-ID):\s*<?([^>\s]+)>?`)

// recipientRe extracts the failing recipient address.
var recipientRe = regexp.MustCompile(`(?im)^(?:Final-Recipient|Original-Recipient):\s*[^;]+;\s*(.+)$`)

// arfFeedbackTypeRe spots the ARF feedback-type header that
// distinguishes complaints from bounces.
var arfFeedbackTypeRe = regexp.MustCompile(`(?im)^Feedback-Type:\s*(abuse|fraud|virus|other)`)

// ParseDSN extracts a Bounce from a DSN body. Exported so the
// inbound webhook handler can call it directly when classifying
// inbound messages (some classifiers route DSNs by Content-Type
// rather than by message-disposition-notification).
func ParseDSN(headers map[string]string, body string) Bounce {
	b := Bounce{Kind: BounceUnknown}

	// Lowercase the headers for case-insensitive lookup.
	lc := make(map[string]string, len(headers))
	for k, v := range headers {
		lc[strings.ToLower(k)] = v
	}

	// ARF / feedback-loop: highest-precedence signal.
	if arfFeedbackTypeRe.MatchString(body) ||
		strings.Contains(lc["content-type"], "message/feedback-report") {
		b.Kind = BounceComplaint
	}

	// Status code from the body (RFC 3464 §2.3.3).
	if m := statusCodeRe.FindStringSubmatch(body); m != nil {
		b.Status = m[1] + "." + m[2] + "." + m[3]
		if b.Kind == BounceUnknown {
			switch m[1] {
			case "5":
				b.Kind = BounceHard
			case "4":
				b.Kind = BounceSoft
			case "2":
				// Delivered. Not a bounce — return
				// BounceUnknown with the status so the
				// caller can treat it as a positive DSN
				// (rare; some MTAs send delivery
				// confirmations).
				b.Kind = BounceUnknown
			}
		}
	}

	// Original Message-ID.
	if m := originalMsgIDRe.FindStringSubmatch(body); m != nil {
		b.OriginalMessageID = strings.TrimSpace(m[1])
	}

	// Recipient.
	if m := recipientRe.FindStringSubmatch(body); m != nil {
		b.Recipient = strings.TrimSpace(m[1])
	}

	// Reason: typically a Diagnostic-Code line or the human-
	// readable preamble. We surface the first 400 chars after
	// "Diagnostic-Code:" so the agent UI can show "550 5.1.1
	// User unknown".
	if idx := strings.Index(strings.ToLower(body), "diagnostic-code:"); idx >= 0 {
		end := idx + len("diagnostic-code:") + 400
		if end > len(body) {
			end = len(body)
		}
		line := strings.SplitN(body[idx+len("diagnostic-code:"):end], "\n", 2)[0]
		b.Reason = strings.TrimSpace(line)
	}

	return b
}
