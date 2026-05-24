package goimap

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk/imap"
)

// TestNewFactory_DefaultsAndOverrides pins the defaulting logic
// applied by NewFactory. The factory must accept the zero
// FactoryOptions and still produce a usable Client.
func TestNewFactory_DefaultsAndOverrides(t *testing.T) {
	f := NewFactory(FactoryOptions{})
	c := f("imap.example.com", 993, true, nil).(*Client)
	if c.dialTimeout != 30*time.Second {
		t.Errorf("default dialTimeout: want 30s, got %s", c.dialTimeout)
	}
	if c.cmdTimeout != 30*time.Second {
		t.Errorf("default cmdTimeout: want 30s, got %s", c.cmdTimeout)
	}
	if c.logger == nil {
		t.Errorf("nil logger fallback should have produced a default")
	}

	// Custom overrides should be honoured.
	custom := NewFactory(FactoryOptions{
		DialTimeout:    5 * time.Second,
		CommandTimeout: 7 * time.Second,
		TLSConfig:      &tls.Config{ServerName: "override.example.com", MinVersion: tls.VersionTLS13},
	})
	c2 := custom("imap2.example.com", 143, false, slog.Default()).(*Client)
	if c2.dialTimeout != 5*time.Second {
		t.Errorf("custom dialTimeout: got %s", c2.dialTimeout)
	}
	if c2.cmdTimeout != 7*time.Second {
		t.Errorf("custom cmdTimeout: got %s", c2.cmdTimeout)
	}
	if c2.tlsConfig == nil || c2.tlsConfig.ServerName != "override.example.com" {
		t.Errorf("custom TLSConfig not threaded through")
	}
}

// TestNewFactory_NegativeCommandTimeout pins that explicit
// negative cmdTimeout disables the deadline (treated as "no
// per-command timeout") while explicit positive zero falls back
// to the 30s default.
func TestNewFactory_CommandTimeoutNegativeDisables(t *testing.T) {
	f := NewFactory(FactoryOptions{CommandTimeout: -1})
	c := f("h", 1, false, nil).(*Client)
	if c.cmdTimeout != 0 {
		t.Errorf("negative cmdTimeout should disable the deadline (got %s)", c.cmdTimeout)
	}
	// And withCommandDeadline should return the input ctx with
	// a no-op cancel.
	ctx, cancel := c.withCommandDeadline(context.Background())
	defer cancel()
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		t.Errorf("disabled cmdTimeout should produce a deadline-less ctx")
	}
}

// TestClient_CloseBeforeConnect pins the interface contract: Close
// on a never-Connected client is a safe no-op.
func TestClient_CloseBeforeConnect(t *testing.T) {
	f := NewFactory(FactoryOptions{})
	c := f("imap.example.com", 993, true, nil)
	if err := c.Close(); err != nil {
		t.Errorf("Close on never-Connect'd client: want nil, got %v", err)
	}
	// Idempotent — second Close also nil.
	if err := c.Close(); err != nil {
		t.Errorf("second Close on never-Connect'd client: want nil, got %v", err)
	}
}

// TestClient_LoginBeforeConnect ensures Login fails cleanly when
// the caller skipped Connect — the activeClient guard catches
// this and returns a clear error rather than a nil-pointer panic.
func TestClient_LoginBeforeConnect(t *testing.T) {
	f := NewFactory(FactoryOptions{})
	c := f("imap.example.com", 993, true, nil)
	err := c.Login(context.Background(), "u", "p")
	if err == nil {
		t.Fatalf("Login before Connect should fail")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestClient_ConnectAfterClose ensures Connect surfaces a clear
// error on a closed Client rather than reopening or panicking.
func TestClient_ConnectAfterClose(t *testing.T) {
	f := NewFactory(FactoryOptions{})
	c := f("imap.example.com", 993, true, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := c.Connect(context.Background())
	if err == nil {
		t.Fatalf("Connect after Close should fail")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestClient_ConnectContextCancelled pins context-cancellation
// behaviour. The underlying imapclient.DialTLS is synchronous,
// but our Connect wraps it in a goroutine + select so a cancelled
// ctx returns promptly. Pointing at a non-routable address makes
// the dial hang long enough to observe the cancellation.
func TestClient_ConnectContextCancelled(t *testing.T) {
	// 198.18.0.0/15 is reserved benchmark space; nothing
	// answers, so the dial blocks until our cancellation fires.
	f := NewFactory(FactoryOptions{DialTimeout: 30 * time.Second})
	c := f("198.18.0.1", 993, true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatalf("Connect should have returned an error on cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected ctx error, got %v", err)
	}
}

// TestIsAuthFailure_Matrix pins the substring matrix used to
// distinguish auth failures from transient errors. Each row in
// this table is an actual response prefix observed from one of
// the major providers (Gmail, Microsoft 365, Dovecot, Cyrus).
func TestIsAuthFailure_Matrix(t *testing.T) {
	cases := []struct {
		err  error
		auth bool
	}{
		{errors.New("NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)"), true},
		{errors.New("BAD username or password"), true},
		{errors.New("NO Authentication failed: bad password"), true},
		{errors.New("NO [AUTHORIZATIONFAILED] User does not exist"), true},
		{errors.New("NO [LOGINDISABLED] Login disabled"), true},
		{errors.New("BAD command not recognised"), false},
		{errors.New("read tcp 1.2.3.4:993: connection reset by peer"), false},
		{errors.New("dial tcp: i/o timeout"), false},
		{nil, false},
	}
	for _, tc := range cases {
		got := isAuthFailure(tc.err)
		if got != tc.auth {
			t.Errorf("isAuthFailure(%v) = %v, want %v", tc.err, got, tc.auth)
		}
	}
}

// TestConvertFetchMessage pins the FetchMessageBuffer →
// imap.FetchedMessage translation. The buffer is the
// upstream-shaped result from a successful FETCH; we copy UID,
// flags, internal-date, and the body bytes from the zero
// BODY[] section.
func TestConvertFetchMessage(t *testing.T) {
	// nil → zero-value FetchedMessage.
	if got := convertFetchMessage(nil); got.UID != 0 || got.Body != nil {
		t.Errorf("nil buffer: want zero FetchedMessage, got %+v", got)
	}

	internal := time.Date(2026, 3, 14, 9, 30, 0, 0, time.UTC)
	section := &imapv2.FetchItemBodySection{Peek: true}
	buf := &imapclient.FetchMessageBuffer{
		UID:          42,
		Flags:        []imapv2.Flag{imapv2.FlagSeen, imapv2.FlagFlagged},
		InternalDate: internal,
		BodySection: []imapclient.FetchBodySectionBuffer{
			{
				Section: section,
				Bytes:   []byte("From: a@example.com\r\n\r\nhello"),
			},
		},
	}
	got := convertFetchMessage(buf)
	if got.UID != 42 {
		t.Errorf("UID: want 42, got %d", got.UID)
	}
	if !got.SeenAt.Equal(internal) {
		t.Errorf("SeenAt: want %v, got %v", internal, got.SeenAt)
	}
	if len(got.Flags) != 2 || got.Flags[0] != string(imapv2.FlagSeen) {
		t.Errorf("Flags: want [\\Seen \\Flagged], got %v", got.Flags)
	}
	if string(got.Body) != "From: a@example.com\r\n\r\nhello" {
		t.Errorf("Body: want raw RFC-822, got %q", string(got.Body))
	}
}

// TestResolvedTLSConfig pins the per-host TLS config wiring.
// Each Connect derives a Clone so per-host ServerName fills do
// not leak between Clients built from the same factory.
func TestResolvedTLSConfig(t *testing.T) {
	shared := &tls.Config{MinVersion: tls.VersionTLS13}
	f := NewFactory(FactoryOptions{TLSConfig: shared})
	c1 := f("a.example.com", 993, true, nil).(*Client)
	c2 := f("b.example.com", 993, true, nil).(*Client)
	cfg1 := c1.resolvedTLSConfig()
	cfg2 := c2.resolvedTLSConfig()
	if cfg1.ServerName != "a.example.com" || cfg2.ServerName != "b.example.com" {
		t.Errorf("ServerName per-host fill failed: got %q / %q", cfg1.ServerName, cfg2.ServerName)
	}
	// Shared config must NOT have been mutated.
	if shared.ServerName != "" {
		t.Errorf("shared TLSConfig was mutated (ServerName=%q)", shared.ServerName)
	}
	if cfg1.MinVersion != tls.VersionTLS13 {
		t.Errorf("Clone did not preserve MinVersion")
	}
}

// TestClient_InterfaceConformance is a compile-time-equivalent
// runtime check that *Client satisfies the helpdesk imap.Client
// interface. The var declaration in the package already pins
// this at compile time; this test gives the reader a single
// place to read the conformance contract.
func TestClient_InterfaceConformance(_ *testing.T) {
	var _ imap.Client = (*Client)(nil)
}
