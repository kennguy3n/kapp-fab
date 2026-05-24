package outbound

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Transport delivers a built wire-form message to recipients. The
// interface is intentionally narrow so SMTP, in-memory test
// transports, and (future) HTTP relay APIs all plug in. Production
// wires an SMTP-backed implementation; tests inject a fake.
type Transport interface {
	// Send delivers the wire bytes to the recipient set. The
	// from address is also passed explicitly so the transport
	// can use it as the SMTP envelope MAIL FROM (which may
	// differ from the message's RFC-822 From: header in
	// SRS-rewriting or sender-spoofing protection deployments).
	Send(ctx context.Context, from string, to []string, wire []byte) error
}

// MessageRecorder is the subset of helpdesk.MessageStore the Sender
// uses to persist outbound rows. Exposed as an interface so unit
// tests inject a fake without a live pgxpool.
type MessageRecorder interface {
	Put(ctx context.Context, m Record) error
}

// Record is the row the Sender hands to the MessageRecorder
// after a successful send. Mirrors helpdesk.Message but with only
// the fields the outbound path populates (Direction is always
// "outbound"; From/To are the addresses we sent to).
type Record struct {
	TenantID   uuid.UUID
	MessageID  string
	TicketID   uuid.UUID
	InReplyTo  string
	References []string
	Subject    string
	FromAddr   string
	ToAddr     string
	SentAt     time.Time
}

// Sender wires a Builder + Transport + MessageRecorder. Send()
// runs all three in sequence; failure of the transport leaves no
// row in email_messages (so a retry on the same outbound ticket
// reply doesn't double-record), while a transport-success followed
// by a recorder failure logs but does NOT roll back the transport
// (the email has already been delivered to the customer's MTA —
// reversing that is not possible).
type Sender struct {
	transport       Transport
	recorder        MessageRecorder
	messageIDDomain string
	now             func() time.Time
}

// NewSender wires a Sender from its dependencies. messageIDDomain
// is the host portion of the generated Message-IDs (typically the
// helpdesk's own domain, e.g. "support.acme.com"). Empty domain
// rejects at construction so production wiring catches the
// misconfiguration at boot, not on first send.
func NewSender(transport Transport, recorder MessageRecorder, messageIDDomain string) (*Sender, error) {
	if transport == nil {
		return nil, errors.New("outbound: transport required")
	}
	if recorder == nil {
		return nil, errors.New("outbound: recorder required")
	}
	if strings.TrimSpace(messageIDDomain) == "" {
		return nil, errors.New("outbound: message-id domain required")
	}
	return &Sender{
		transport:       transport,
		recorder:        recorder,
		messageIDDomain: messageIDDomain,
		now:             func() time.Time { return time.Now().UTC() },
	}, nil
}

// SendArgs bundles the per-call parameters for Send. Keeping them
// in a struct keeps the call site readable when there are 5+
// fields (vs a 5-arg function signature).
type SendArgs struct {
	TenantID uuid.UUID
	TicketID uuid.UUID
	Message  Message
}

// ErrRecordAfterSend is returned by Send when the transport
// succeeded but the recorder failed. Callers use
// errors.Is(err, ErrRecordAfterSend) to distinguish "email lost"
// from "threading anchor lost" — the customer's reply will land
// as a new ticket in the latter case (no parent message row to
// look up against), which is the correct degraded behaviour.
var ErrRecordAfterSend = errors.New("outbound: record-after-send failed")

// Send builds the message, dispatches via the transport, and (on
// success) records the row in email_messages. The returned
// Message-ID is the one the customer's mail client will see and
// thread off of.
//
// Errors:
//
//   - Builder-side errors (validation, message-id generation):
//     returned before any I/O. Transport NOT called.
//   - Transport errors: returned wrapped; recorder NOT called
//     (no row is written for an undelivered message — a retry
//     should not produce two recorded outbound rows).
//   - Recorder errors after successful send: returned wrapped
//     in ErrRecordAfterSend so callers can distinguish the two
//     failure modes; the Message-ID is still returned so it can
//     be logged for forensics.
func (s *Sender) Send(ctx context.Context, args SendArgs) (string, error) {
	if args.TenantID == uuid.Nil {
		return "", errors.New("outbound: tenant id required")
	}
	if args.TicketID == uuid.Nil {
		return "", errors.New("outbound: ticket id required")
	}
	built, err := Build(s.now, args.Message, s.messageIDDomain)
	if err != nil {
		return "", err
	}
	if err := s.transport.Send(ctx, args.Message.From, args.Message.To, built.Wire); err != nil {
		return "", fmt.Errorf("outbound: transport: %w", err)
	}
	// Email delivered. Persist the outbound row so the
	// customer's reply threads back to args.TicketID via the
	// resolver. We deliberately do NOT roll back the transport
	// on recorder failure — the email has left the building.
	rec := Record{
		TenantID:   args.TenantID,
		MessageID:  built.MessageID,
		TicketID:   args.TicketID,
		InReplyTo:  args.Message.InReplyTo,
		References: args.Message.References,
		Subject:    args.Message.Subject,
		FromAddr:   args.Message.From,
		ToAddr:     strings.Join(args.Message.To, ", "),
		SentAt:     s.now(),
	}
	if err := s.recorder.Put(ctx, rec); err != nil {
		return built.MessageID, fmt.Errorf("%w: %w", ErrRecordAfterSend, err)
	}
	return built.MessageID, nil
}

// SetClock injects a deterministic clock for header timestamps +
// record timestamps. Exposed for tests; production leaves it at
// the real clock.
func (s *Sender) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}
