package outbound

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestParseDSN_HardBounce pins the most common DSN shape: a 5.x.x
// status code → BounceHard, original Message-ID extracted, failing
// recipient extracted, diagnostic-code surfaced as Reason.
func TestParseDSN_HardBounce(t *testing.T) {
	body := `This is the mail delivery agent.

The following message could not be delivered:

Final-Recipient: rfc822; bad@example.com
Original-Message-ID: <abcd1234@support.acme.com>
Action: failed
Status: 5.1.1
Diagnostic-Code: smtp; 550 5.1.1 User unknown
`
	b := ParseDSN(map[string]string{"Content-Type": "multipart/report"}, body)
	if b.Kind != BounceHard {
		t.Errorf("expected BounceHard, got %q", b.Kind)
	}
	if b.OriginalMessageID != "abcd1234@support.acme.com" {
		t.Errorf("expected original Message-ID extracted, got %q", b.OriginalMessageID)
	}
	if b.Recipient != "bad@example.com" {
		t.Errorf("expected recipient extracted, got %q", b.Recipient)
	}
	if b.Status != "5.1.1" {
		t.Errorf("expected status 5.1.1, got %q", b.Status)
	}
	if b.Reason != "smtp; 550 5.1.1 User unknown" {
		t.Errorf("unexpected Reason: %q", b.Reason)
	}
}

// TestParseDSN_SoftBounce pins 4.x.x → BounceSoft (transient,
// outbox retries).
func TestParseDSN_SoftBounce(t *testing.T) {
	body := `Final-Recipient: rfc822; full@example.com
Original-Message-ID: <xx@x>
Status: 4.2.2
Diagnostic-Code: smtp; 452 Mailbox quota exceeded
`
	b := ParseDSN(nil, body)
	if b.Kind != BounceSoft {
		t.Errorf("expected BounceSoft, got %q", b.Kind)
	}
	if b.Status != "4.2.2" {
		t.Errorf("expected status 4.2.2, got %q", b.Status)
	}
}

// TestParseDSN_ARFComplaint pins the ARF/feedback-loop shape: the
// Feedback-Type header bumps the verdict to BounceComplaint
// regardless of (or in absence of) a status code.
func TestParseDSN_ARFComplaint(t *testing.T) {
	body := `Feedback-Type: abuse
User-Agent: Yahoo-Mail-FBL/2.0
Original-Message-ID: <complaint-source@support.acme.com>
`
	b := ParseDSN(nil, body)
	if b.Kind != BounceComplaint {
		t.Errorf("expected BounceComplaint, got %q", b.Kind)
	}
	if b.OriginalMessageID != "complaint-source@support.acme.com" {
		t.Errorf("expected original Message-ID, got %q", b.OriginalMessageID)
	}
}

// TestParseDSN_ContentTypeBasedComplaint pins ARF detection via
// Content-Type when the body lacks a Feedback-Type line.
func TestParseDSN_ContentTypeBasedComplaint(t *testing.T) {
	body := `Original-Message-ID: <complaint@x>`
	b := ParseDSN(map[string]string{"Content-Type": "message/feedback-report"}, body)
	if b.Kind != BounceComplaint {
		t.Errorf("expected BounceComplaint via Content-Type, got %q", b.Kind)
	}
}

// TestParseDSN_FallbackToMessageIDHeader pins that when the body
// has a Message-ID header (in the included rfc822 part) but no
// Original-Message-ID or Original-Envelope-Id, we still extract it.
func TestParseDSN_FallbackToMessageIDHeader(t *testing.T) {
	body := `Final-Recipient: rfc822; x@y
Status: 5.5.0
Message-ID: <fallback@support.x.com>
`
	b := ParseDSN(nil, body)
	if b.OriginalMessageID != "fallback@support.x.com" {
		t.Errorf("expected fallback Message-ID extraction, got %q", b.OriginalMessageID)
	}
}

// TestParseDSN_UnparseableBody pins the "not a DSN" outcome: a
// body with no recognised DSN fields parses to BounceUnknown with
// empty Status + empty OriginalMessageID.
func TestParseDSN_UnparseableBody(t *testing.T) {
	body := `Hi! Just a regular human reply, not a bounce.`
	b := ParseDSN(nil, body)
	if b.Kind != BounceUnknown {
		t.Errorf("expected BounceUnknown for non-DSN body, got %q", b.Kind)
	}
	if b.OriginalMessageID != "" {
		t.Errorf("expected empty OriginalMessageID, got %q", b.OriginalMessageID)
	}
}

// TestParseDSN_OriginalEnvelopeId pins extraction via
// Original-Envelope-Id (alternative DSN format).
func TestParseDSN_OriginalEnvelopeId(t *testing.T) {
	body := `Status: 5.0.0
Original-Envelope-Id: env123@support.x.com
`
	b := ParseDSN(nil, body)
	if b.OriginalMessageID != "env123@support.x.com" {
		t.Errorf("expected Original-Envelope-Id extraction, got %q", b.OriginalMessageID)
	}
}

// TestParseDSN_2xxNotABounce pins that a delivery-success DSN
// (2.x.x — rare but legal) parses as BounceUnknown so the caller
// doesn't flag the original ticket as undeliverable.
func TestParseDSN_2xxNotABounce(t *testing.T) {
	body := `Status: 2.0.0
Original-Message-ID: <success@x>
`
	b := ParseDSN(nil, body)
	if b.Kind == BounceHard || b.Kind == BounceSoft {
		t.Errorf("expected non-bounce verdict for 2.x.x, got %q", b.Kind)
	}
	if b.Status != "2.0.0" {
		t.Errorf("expected status preserved, got %q", b.Status)
	}
}

// fakeBounceStore captures Handle's lookups and writes.
type fakeBounceStore struct {
	findResult uuid.UUID
	findFound  bool
	findErr    error
	recordErr  error
	finds      []string
	records    []Bounce
}

func (s *fakeBounceStore) FindMessage(_ context.Context, _ uuid.UUID, msgID string) (uuid.UUID, bool, error) {
	s.finds = append(s.finds, msgID)
	return s.findResult, s.findFound, s.findErr
}

func (s *fakeBounceStore) RecordBounce(_ context.Context, _, _ uuid.UUID, b Bounce) error {
	s.records = append(s.records, b)
	return s.recordErr
}

// TestBounceHandler_HardBouncePersistsNote pins the end-to-end
// happy path: a hard bounce with a known Message-ID is looked up
// to a ticket and persisted via RecordBounce.
func TestBounceHandler_HardBouncePersistsNote(t *testing.T) {
	store := &fakeBounceStore{findResult: uuid.New(), findFound: true}
	h := NewBounceHandler(store)
	body := `Final-Recipient: rfc822; x@y
Original-Message-ID: <abcd@x>
Status: 5.1.1
`
	b, err := h.Handle(context.Background(), uuid.New(), nil, body)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if b.Kind != BounceHard {
		t.Errorf("expected hard, got %q", b.Kind)
	}
	if len(store.finds) != 1 || store.finds[0] != "abcd@x" {
		t.Errorf("expected FindMessage called with abcd@x, got %v", store.finds)
	}
	if len(store.records) != 1 {
		t.Errorf("expected one RecordBounce, got %d", len(store.records))
	}
}

// TestBounceHandler_UnknownOriginalMessage pins the
// ErrNoOriginalMessage path: a parseable DSN whose Original-
// Message-ID isn't in our table → ErrNoOriginalMessage, no
// RecordBounce call.
func TestBounceHandler_UnknownOriginalMessage(t *testing.T) {
	store := &fakeBounceStore{findFound: false}
	h := NewBounceHandler(store)
	body := `Original-Message-ID: <unknown@x>
Status: 5.0.0
`
	_, err := h.Handle(context.Background(), uuid.New(), nil, body)
	if !errors.Is(err, ErrNoOriginalMessage) {
		t.Errorf("expected ErrNoOriginalMessage, got %v", err)
	}
	if len(store.records) != 0 {
		t.Errorf("expected no RecordBounce on unknown message, got %d", len(store.records))
	}
}

// TestBounceHandler_UnparseableBody pins ErrUnparseable.
func TestBounceHandler_UnparseableBody(t *testing.T) {
	store := &fakeBounceStore{}
	h := NewBounceHandler(store)
	_, err := h.Handle(context.Background(), uuid.New(), nil, "definitely not a dsn")
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("expected ErrUnparseable, got %v", err)
	}
	if len(store.finds) != 0 {
		t.Errorf("expected no DB calls on unparseable body")
	}
}

// TestBounceHandler_FindErrorPropagates pins that a DB error
// during FindMessage propagates to the caller (so the relay
// retries the webhook delivery).
func TestBounceHandler_FindErrorPropagates(t *testing.T) {
	store := &fakeBounceStore{findErr: errors.New("simulated db outage")}
	h := NewBounceHandler(store)
	body := `Original-Message-ID: <x@x>
Status: 5.0.0
`
	_, err := h.Handle(context.Background(), uuid.New(), nil, body)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, store.findErr) {
		t.Errorf("expected find error wrapped, got %v", err)
	}
}
