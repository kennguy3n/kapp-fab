package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/notifications"
)

// recordingSMTPSender captures the recipient, subject, and body for
// the assertions below. It implements notifications.SMTPSender so the
// portalSMTPMailer can be tested end-to-end without dialing a real
// MTA — only the network layer is mocked, the formatting and
// envelope-construction logic in portalSMTPMailer is exercised for
// real.
type recordingSMTPSender struct {
	to      []string
	subject string
	body    string
	err     error
}

func (s *recordingSMTPSender) Send(_ context.Context, to []string, subject, body string) error {
	s.to = to
	s.subject = subject
	s.body = body
	return s.err
}

func TestPortalSMTPMailer_SendsLinkToRecipient(t *testing.T) {
	sender := &recordingSMTPSender{}
	mailer := portalSMTPMailer{sender: sender}
	tenantID := uuid.New()
	link := "https://example.invalid/portal/auth/callback?token=abc.def"
	if err := mailer.Send(context.Background(), tenantID, "customer@example.invalid", link); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sender.to) != 1 || sender.to[0] != "customer@example.invalid" {
		t.Fatalf("to = %v, want [customer@example.invalid]", sender.to)
	}
	if sender.subject == "" {
		t.Fatal("subject is empty")
	}
	// The body must contain the magic link verbatim — any
	// templating bug that mangles the URL renders the link unusable
	// and the test must catch that regression.
	if !strings.Contains(sender.body, link) {
		t.Fatalf("body does not contain the magic link verbatim.\nbody:\n%s", sender.body)
	}
}

func TestPortalSMTPMailer_RejectsEmptyRecipient(t *testing.T) {
	sender := &recordingSMTPSender{}
	mailer := portalSMTPMailer{sender: sender}
	if err := mailer.Send(context.Background(), uuid.New(), "", "https://example.invalid"); err == nil {
		t.Fatal("expected error for empty recipient")
	}
	if sender.to != nil {
		t.Fatalf("sender was called with to=%v; expected short-circuit", sender.to)
	}
}

func TestPortalSMTPMailer_PropagatesSendError(t *testing.T) {
	sentinel := errors.New("smtp: relay refused")
	sender := &recordingSMTPSender{err: sentinel}
	mailer := portalSMTPMailer{sender: sender}
	err := mailer.Send(context.Background(), uuid.New(), "x@example.invalid", "https://example.invalid")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping of %v", err, sentinel)
	}
}

func TestFailingPortalMailer_AlwaysErrors(t *testing.T) {
	sentinel := errors.New("portal: SMTP not configured")
	mailer := failingPortalMailer{err: sentinel}
	if err := mailer.Send(context.Background(), uuid.New(), "x@example.invalid", "link"); err != sentinel {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// TestPortalSMTPMailer_NoopSenderRespected exercises the contract
// between portalSMTPMailer and the real notifications.SMTPAdapter:
// an adapter built from a zero-value SMTPConfig returns ErrSMTPDisabled
// and the wrapper passes that through unchanged so the calling layer
// can log-and-continue.
func TestPortalSMTPMailer_NoopSenderRespected(t *testing.T) {
	adapter := notifications.NewSMTPAdapter(notifications.SMTPConfig{})
	mailer := portalSMTPMailer{sender: adapter}
	err := mailer.Send(context.Background(), uuid.New(), "x@example.invalid", "link")
	if !errors.Is(err, notifications.ErrSMTPDisabled) {
		t.Fatalf("err = %v, want ErrSMTPDisabled", err)
	}
}
