package notifications

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// SMTPConfig bundles the env-vars consumed by the SMTP adapter. The
// zero value is valid and means "SMTP disabled" — every Send call
// returns ErrSMTPDisabled so callers can log-and-continue without
// wrapping every invocation in a nil-check.
type SMTPConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	From     string
}

// ErrSMTPDisabled signals the caller that SMTP was never configured.
// The worker treats it as a soft failure and falls back to logging.
var ErrSMTPDisabled = errors.New("smtp: not configured")

// SMTPSender is the minimal interface consumed by the worker. A real
// implementation talks to an MTA; a fake can short-circuit for tests.
type SMTPSender interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// SMTPAdapter is the net/smtp-backed SMTPSender. It builds an
// RFC 822 message from (From, To, Subject, Body), authenticates with
// PLAIN when User/Password are set, and dials Host:Port. Port is
// passed through untouched so operators can point it at 25, 465, 587
// or a local relay as they see fit — the adapter does NOT implement
// implicit TLS on 465; use a submission port with STARTTLS if that
// matters in your deployment. For simplicity and because this path is
// guarded by the outbox-retry loop, delivery is best-effort: any
// network or auth failure is returned to the caller unchanged.
type SMTPAdapter struct {
	cfg SMTPConfig
	// sendFn is an override seam for tests; production leaves it nil
	// and the adapter uses smtp.SendMail.
	sendFn func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// NewSMTPAdapter returns an adapter bound to cfg. When cfg is the
// zero value (no host), Send() returns ErrSMTPDisabled so the worker
// can fall through to its log-only path.
func NewSMTPAdapter(cfg SMTPConfig) *SMTPAdapter {
	return &SMTPAdapter{cfg: cfg}
}

// Send delivers a plain-text email. ctx is honored only insofar as
// the underlying net/smtp call respects dial cancellation via the
// default dialer; we do not wrap smtp.SendMail in a goroutine because
// callers already bound the outer request timeout.
func (a *SMTPAdapter) Send(ctx context.Context, to []string, subject, body string) error {
	if a == nil || a.cfg.Host == "" {
		return ErrSMTPDisabled
	}
	if len(to) == 0 {
		return fmt.Errorf("smtp: recipient list is empty")
	}
	if a.cfg.From == "" {
		return fmt.Errorf("smtp: SMTP_FROM not configured")
	}
	port := a.cfg.Port
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(a.cfg.Host, port)
	msg := buildMessage(a.cfg.From, to, subject, body)
	var auth smtp.Auth
	if a.cfg.User != "" {
		auth = smtp.PlainAuth("", a.cfg.User, a.cfg.Password, a.cfg.Host)
	}
	send := a.sendFn
	if send == nil {
		send = smtp.SendMail
	}
	if err := send(addr, auth, a.cfg.From, to, msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// buildMessage stitches together an RFC 822 message. We keep it
// plain-text only — HTML templating lives one layer up in the
// notification envelope renderer when/if it becomes necessary.
func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String())
}
