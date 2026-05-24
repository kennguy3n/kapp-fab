package outbound

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeTransport records every Send call and returns a controlled
// error. Tests use it to verify ordering (transport called BEFORE
// recorder), payload content, and the transport-error
// short-circuit.
type fakeTransport struct {
	calls []transportCall
	err   error
}

type transportCall struct {
	from string
	to   []string
	wire []byte
}

func (f *fakeTransport) Send(_ context.Context, from string, to []string, wire []byte) error {
	f.calls = append(f.calls, transportCall{from: from, to: to, wire: append([]byte(nil), wire...)})
	return f.err
}

// fakeRecorder records every Put call. Tests use it to verify the
// OutboundRecord shape and the order vs. transport calls.
type fakeRecorder struct {
	puts []Record
	err  error
}

func (f *fakeRecorder) Put(_ context.Context, m Record) error {
	f.puts = append(f.puts, m)
	return f.err
}

// TestNewSender_ValidationErrors pins constructor validation.
func TestNewSender_ValidationErrors(t *testing.T) {
	tr := &fakeTransport{}
	rec := &fakeRecorder{}
	if _, err := NewSender(nil, rec, "d"); err == nil {
		t.Errorf("expected error for nil transport")
	}
	if _, err := NewSender(tr, nil, "d"); err == nil {
		t.Errorf("expected error for nil recorder")
	}
	if _, err := NewSender(tr, rec, ""); err == nil {
		t.Errorf("expected error for empty domain")
	}
	if _, err := NewSender(tr, rec, "   "); err == nil {
		t.Errorf("expected error for whitespace domain")
	}
}

// TestSender_HappyPath pins the full success path: builder →
// transport → recorder, in that order. Returned Message-ID
// matches what was sent + recorded.
func TestSender_HappyPath(t *testing.T) {
	tr := &fakeTransport{}
	rec := &fakeRecorder{}
	s, err := NewSender(tr, rec, "support.acme.com")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	s.SetClock(fixedClock(time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)))

	ticketID := uuid.New()
	tenantID := uuid.New()
	msgID, err := s.Send(context.Background(), SendArgs{
		TenantID: tenantID,
		TicketID: ticketID,
		Message: Message{
			From:       "support@acme.com",
			To:         []string{"customer@example.com"},
			Subject:    "Re: foo",
			InReplyTo:  "orig@example.com",
			References: []string{"orig@example.com"},
			BodyText:   "thanks",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if msgID == "" {
		t.Fatalf("expected non-empty message-id")
	}
	if len(tr.calls) != 1 {
		t.Fatalf("expected one transport call, got %d", len(tr.calls))
	}
	if len(rec.puts) != 1 {
		t.Fatalf("expected one recorder Put, got %d", len(rec.puts))
	}
	put := rec.puts[0]
	if put.TenantID != tenantID || put.TicketID != ticketID {
		t.Errorf("expected tenant/ticket threaded through, got %v/%v", put.TenantID, put.TicketID)
	}
	if put.MessageID != msgID {
		t.Errorf("expected recorded msgID = returned msgID; got %q vs %q", put.MessageID, msgID)
	}
	if put.ToAddr != "customer@example.com" {
		t.Errorf("expected recorded ToAddr, got %q", put.ToAddr)
	}
	if put.InReplyTo != "orig@example.com" {
		t.Errorf("expected InReplyTo preserved, got %q", put.InReplyTo)
	}
	if !strings.Contains(string(tr.calls[0].wire), "Re: foo") {
		t.Errorf("expected wire to carry subject")
	}
}

// TestSender_BuilderErrorShortCircuits pins that a builder
// validation failure does NOT call transport or recorder.
func TestSender_BuilderErrorShortCircuits(t *testing.T) {
	tr := &fakeTransport{}
	rec := &fakeRecorder{}
	s, _ := NewSender(tr, rec, "d")
	_, err := s.Send(context.Background(), SendArgs{
		TenantID: uuid.New(),
		TicketID: uuid.New(),
		Message:  Message{}, // empty — builder rejects
	})
	if err == nil {
		t.Fatalf("expected builder validation error")
	}
	if len(tr.calls) != 0 {
		t.Errorf("expected transport NOT called on builder error, got %d calls", len(tr.calls))
	}
	if len(rec.puts) != 0 {
		t.Errorf("expected recorder NOT called on builder error, got %d puts", len(rec.puts))
	}
}

// TestSender_TransportErrorShortCircuits pins the
// "no row for an undelivered message" contract. On transport
// failure, the recorder is NEVER called.
func TestSender_TransportErrorShortCircuits(t *testing.T) {
	tr := &fakeTransport{err: errors.New("simulated DNS failure")}
	rec := &fakeRecorder{}
	s, _ := NewSender(tr, rec, "d")
	_, err := s.Send(context.Background(), SendArgs{
		TenantID: uuid.New(),
		TicketID: uuid.New(),
		Message: Message{
			From: "a@x.com", To: []string{"b@x.com"}, Subject: "s", BodyText: "b",
		},
	})
	if err == nil {
		t.Fatalf("expected transport error")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("expected wrap to mention transport, got %v", err)
	}
	if len(rec.puts) != 0 {
		t.Errorf("expected recorder NOT called on transport error, got %d puts", len(rec.puts))
	}
}

// TestSender_RecorderErrorReturnsSentinel pins the recorder-
// after-send sentinel. The email has already left the building, so
// the caller distinguishes "email lost" from "threading lost" via
// errors.Is(err, ErrRecordAfterSend). The returned Message-ID is
// still set so the caller can log it for forensics.
func TestSender_RecorderErrorReturnsSentinel(t *testing.T) {
	tr := &fakeTransport{}
	rec := &fakeRecorder{err: errors.New("simulated db outage")}
	s, _ := NewSender(tr, rec, "d")
	msgID, err := s.Send(context.Background(), SendArgs{
		TenantID: uuid.New(),
		TicketID: uuid.New(),
		Message: Message{
			From: "a@x.com", To: []string{"b@x.com"}, Subject: "s", BodyText: "b",
		},
	})
	if err == nil {
		t.Fatalf("expected recorder error")
	}
	if !errors.Is(err, ErrRecordAfterSend) {
		t.Errorf("expected ErrRecordAfterSend sentinel, got %v", err)
	}
	if msgID == "" {
		t.Errorf("expected non-empty msgID on record-after-send failure (for forensics)")
	}
	// Transport WAS called (the email went out).
	if len(tr.calls) != 1 {
		t.Errorf("expected exactly one transport call, got %d", len(tr.calls))
	}
}

// TestSender_RejectsNilIDs pins the tenant + ticket id non-zero
// requirement.
func TestSender_RejectsNilIDs(t *testing.T) {
	tr := &fakeTransport{}
	rec := &fakeRecorder{}
	s, _ := NewSender(tr, rec, "d")
	cases := []struct {
		name     string
		tenantID uuid.UUID
		ticketID uuid.UUID
	}{
		{"nil_tenant", uuid.Nil, uuid.New()},
		{"nil_ticket", uuid.New(), uuid.Nil},
		{"both_nil", uuid.Nil, uuid.Nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Send(context.Background(), SendArgs{
				TenantID: tc.tenantID,
				TicketID: tc.ticketID,
				Message:  Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "s", BodyText: "b"},
			})
			if err == nil {
				t.Errorf("expected validation error")
			}
		})
	}
	if len(tr.calls) != 0 || len(rec.puts) != 0 {
		t.Errorf("expected no I/O on validation errors")
	}
}

// TestSender_RecordsAllRecipients pins that a To: list with
// multiple addresses gets joined to a single recorded ToAddr
// string (for display purposes; the actual recipient list is
// passed to transport individually).
func TestSender_RecordsAllRecipients(t *testing.T) {
	tr := &fakeTransport{}
	rec := &fakeRecorder{}
	s, _ := NewSender(tr, rec, "d")
	_, err := s.Send(context.Background(), SendArgs{
		TenantID: uuid.New(),
		TicketID: uuid.New(),
		Message: Message{
			From: "a@x.com",
			To:   []string{"b@x.com", "c@x.com"},
			Subject: "s", BodyText: "b",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.puts[0].ToAddr != "b@x.com, c@x.com" {
		t.Errorf("expected joined ToAddr, got %q", rec.puts[0].ToAddr)
	}
	if len(tr.calls[0].to) != 2 {
		t.Errorf("expected 2 transport recipients, got %d", len(tr.calls[0].to))
	}
}
