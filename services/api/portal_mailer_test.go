package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

// TestMailerConfiguredReports pins the Configured() contract on both
// implementations. requestMagicLink uses Configured() to short-circuit
// with 503 when SMTP is not wired — if either implementation lies
// about its state, deployments would silently 204 instead of
// surfacing the misconfiguration.
func TestMailerConfiguredReports(t *testing.T) {
	if !(portalSMTPMailer{}).Configured() {
		t.Fatal("portalSMTPMailer{}.Configured() = false; want true (only built when SMTP wired)")
	}
	if (failingPortalMailer{}).Configured() {
		t.Fatal("failingPortalMailer{}.Configured() = true; want false (only built when SMTP empty)")
	}
}

// stubMailer lets the handler test toggle Configured() without
// touching the SMTP stack. Send is a no-op because the unconfigured
// path returns 503 before Send would ever be reached.
type stubMailer struct {
	configured bool
}

func (m stubMailer) Configured() bool                                      { return m.configured }
func (m stubMailer) Send(context.Context, uuid.UUID, string, string) error { return nil }

// TestRequestMagicLink_Unconfigured503 is the regression guard for
// the Devin Review finding (FLAG_…0006) that the old code returned
// 204 even when SMTP was not wired. With Configured()=false the
// handler must short-circuit with 503 BEFORE the tenant lookup so a
// missing SMTP transport surfaces as a hard failure instead of being
// indistinguishable from a successful drop.
func TestRequestMagicLink_Unconfigured503(t *testing.T) {
	h := &portalHandlers{mailer: stubMailer{configured: false}}
	body, _ := json.Marshal(portalAuthRequest{TenantSlug: "acme", Email: "x@example.invalid"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/auth/request-link", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.requestMagicLink(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (mailer unconfigured)", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("body = %q, want message naming the misconfiguration", rec.Body.String())
	}
}

// TestRequestMagicLink_NilMailer503 covers the defensive nil check
// that protects against a wiring bug (h.mailer not assigned). It
// must also surface 503 rather than panic on Send.
func TestRequestMagicLink_NilMailer503(t *testing.T) {
	h := &portalHandlers{mailer: nil}
	body, _ := json.Marshal(portalAuthRequest{TenantSlug: "acme", Email: "x@example.invalid"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/auth/request-link", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.requestMagicLink(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (mailer nil)", rec.Code, http.StatusServiceUnavailable)
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
