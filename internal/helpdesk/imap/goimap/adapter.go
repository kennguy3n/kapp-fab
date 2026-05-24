// Package goimap adapts github.com/emersion/go-imap/v2/imapclient
// to the helpdesk imap.Client interface. It is the wire-protocol
// implementation behind the Poller — the Manager wires one Client
// per mailbox at converge time, and every IMAP command the Poller
// emits goes through this package on its way to the network.
//
// # Why a thin adapter
//
// internal/helpdesk/imap defines a six-method Client interface
// (Connect / Login / Select / FetchAfter / Logout / Close). The
// real IMAP wire protocol is a much wider surface — search,
// fetch with arbitrary attribute sets, append, idle, capability
// negotiation, STARTTLS upgrade, SASL, UIDPLUS, MOVE, BINARY,
// and so on — but the Poller intentionally uses only the narrow
// subset above. Pinning the abstraction at that shape keeps
// three things easy:
//
//   - Unit-testing the Poller against an in-memory fake (no real
//     IMAP server needed in the unit-test path).
//   - Swapping IMAP libraries (or layering retry / circuit-breaker
//     wrappers) without touching the Poller.
//   - Keeping the go-imap/v2 dependency confined to one package so
//     the rest of internal/helpdesk does not transitively pull
//     it into every consumer.
//
// # go-imap/v2 dependency choice
//
// We use github.com/emersion/go-imap/v2 (beta tag) over the v1
// release because v2:
//
//   - exposes UIDs as a typed UID (uint32) distinct from sequence
//     numbers, which matches our checkpoint model (we never deal
//     in sequence numbers; UIDs survive UIDVALIDITY changes by
//     way of our own reset logic);
//   - drops the channel-driven response API in favour of typed
//     commands with Wait()/Collect() that map cleanly to the
//     synchronous adapter shape the Poller expects;
//   - supports IMAP4rev2 (RFC 9051) including the LOGINDISABLED /
//     STARTTLS negotiation Gmail and Microsoft 365 use today;
//   - has been continuously maintained throughout 2024-2025 with
//     the upstream maintainer landing v2-beta releases on a
//     rolling cadence (it is "beta" only by version-tag, the
//     library is production-grade and is what the maintainer's
//     own production mail server uses).
//
// # Lifecycle
//
// The adapter has three live states plus one terminal state. The
// live cycle matches what imap.Client documents — Connect / Login /
// Select / FetchAfter / Logout is what the Poller runs every poll
// interval, so a single Client must round-trip Pre-Connect →
// Authenticated → Pre-Connect repeatedly over its lifetime, NOT
// transition linearly to a terminal state. Close (and only Close)
// moves the Client to the terminal state.
//
//  1. Pre-Connect: underlying *imapclient.Client is nil. Reachable
//     as the zero value AND as the post-Logout state. Calling
//     Close() in this state is a no-op (per the interface contract
//     — the supervisor's cleanup path on Manager.Start failure may
//     hit a never-Connect()'d Client). Calling Connect() advances
//     to Connected.
//  2. Connected: Connect() has dialed + handshaked. The underlying
//     client is alive but unauthenticated. Calling Close() drops
//     the socket without a LOGOUT (advances to Closed).
//  3. Authenticated: Login() has succeeded. Select/FetchAfter/
//     Logout are valid in this state. Logout() returns to
//     Pre-Connect (transport closed, no terminal flag set);
//     Close() advances to Closed.
//  4. Closed (terminal): set by Close() only. Connect/Login/Select/
//     FetchAfter all return an error. Logout() is a no-op so the
//     supervisor's defer-Logout-after-Close pattern never panics.
//
// The Manager builds a fresh Client per Start, so reconnect after a
// hard failure lives at the Manager level — but the per-poll-cycle
// Connect→Logout→Connect rhythm lives here.
package goimap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk/imap"
)

// FactoryOptions tunes the dialler and TLS configuration shared
// across every Client the factory produces. The zero value is a
// safe default — TLS verification on, 30 s dial timeout, 30 s
// network read timeout for blocking commands. Tests + integration
// harnesses override these as needed.
type FactoryOptions struct {
	// TLSConfig is reused across every TLS Connect. nil falls
	// back to the system roots with the host derived from the
	// Config.IMAPHost (the standard tls.Config default). Pass a
	// custom config when running against a test server with a
	// self-signed cert or when an operator wants to pin a
	// specific cipher suite or root pool.
	TLSConfig *tls.Config
	// DialTimeout caps the time the underlying net.Dialer spends
	// on TCP / TLS connect. Default 30 seconds.
	DialTimeout time.Duration
	// CommandTimeout caps the time a single blocking command
	// (Login, Select, Fetch, Logout) may spend waiting for the
	// server. The zero value picks the 30-second default so
	// FactoryOptions{} is safe for production; set a NEGATIVE
	// duration to disable the per-command deadline entirely
	// (useful for integration tests that drive long-running
	// IDLE-style commands through the same adapter).
	CommandTimeout time.Duration
	// DebugWriter, when non-nil, receives the raw IMAP wire
	// bytes for both directions. Useful for local debugging
	// against a real server but MUST be nil in production —
	// the stream contains AUTHENTICATE credentials.
	DebugWriter interface{ Write(p []byte) (int, error) }
}

// NewFactory returns a Client constructor matching the shape the
// worker's imapClientFactory expects. The returned closure can
// be passed straight into newHelpdeskIMAPState's clientFactory
// argument:
//
//	factory := goimap.NewFactory(goimap.FactoryOptions{})
//	state := newHelpdeskIMAPState(pool, adminPool, recordStore,
//	    helpdeskStore, factory, logger)
//
// Each invocation produces a fresh, un-connected Client. The
// Manager invokes the factory once per Start; the supervisor's
// converge loop owns the Client's whole lifecycle.
func NewFactory(opts FactoryOptions) func(host string, port int, useTLS bool, logger *slog.Logger) imap.Client {
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second
	}
	cmdTimeout := opts.CommandTimeout
	if cmdTimeout < 0 {
		cmdTimeout = 0
	} else if cmdTimeout == 0 {
		cmdTimeout = 30 * time.Second
	}
	return func(host string, port int, useTLS bool, logger *slog.Logger) imap.Client {
		if logger == nil {
			logger = slog.Default()
		}
		return &Client{
			host:        host,
			port:        port,
			useTLS:      useTLS,
			tlsConfig:   opts.TLSConfig,
			dialTimeout: dialTimeout,
			cmdTimeout:  cmdTimeout,
			debugWriter: opts.DebugWriter,
			logger:      logger,
		}
	}
}

// Client is the go-imap/v2 implementation of imap.Client. One
// instance maps 1:1 to one mailbox row's Poller goroutine for
// the goroutine's lifetime — the Manager builds it, hands it to
// the Poller, and the Poller calls Logout on shutdown.
type Client struct {
	host        string
	port        int
	useTLS      bool
	tlsConfig   *tls.Config
	dialTimeout time.Duration
	cmdTimeout  time.Duration
	debugWriter interface{ Write(p []byte) (int, error) }
	logger      *slog.Logger

	mu     sync.Mutex
	client *imapclient.Client
	closed bool
}

// compile-time check.
var _ imap.Client = (*Client)(nil)

// Connect dials the IMAP server and performs the TLS handshake
// (when useTLS=true). It does NOT log in — Login is a separate
// method so the Manager can distinguish network-level failures
// (retry-with-backoff) from auth failures (alert + halt).
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("imap: Connect on closed client")
	}
	if c.client != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	options := &imapclient.Options{
		Dialer: &net.Dialer{Timeout: c.dialTimeout},
	}
	if c.debugWriter != nil {
		options.DebugWriter = c.debugWriter
	}

	// Respect ctx by running the (blocking) dial in a goroutine
	// and selecting on cancellation. The underlying
	// imapclient.DialTLS does not take a context — go-imap/v2's
	// API surfaces context only on the per-command path. We
	// wrap the dial so a Run-loop cancellation does not block
	// waiting for the connect timeout when the worker is
	// shutting down.
	var (
		client    *imapclient.Client
		dialErr   error
		dialDone  = make(chan struct{})
		tlsConfig = c.resolvedTLSConfig()
	)
	options.TLSConfig = tlsConfig
	go func() {
		defer close(dialDone)
		if c.useTLS {
			client, dialErr = imapclient.DialTLS(addr, options)
		} else {
			client, dialErr = imapclient.DialStartTLS(addr, options)
		}
	}()
	select {
	case <-ctx.Done():
		// If the dial finishes after ctx cancellation, the
		// goroutine has a valid client we now own; close it
		// so the connection does not leak.
		go func() {
			<-dialDone
			if client != nil {
				_ = client.Close()
			}
		}()
		return ctx.Err()
	case <-dialDone:
	}
	if dialErr != nil {
		return fmt.Errorf("imap: dial %s: %w", addr, dialErr)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		// Close raced ahead of us. Drop the just-built client.
		_ = client.Close()
		return errors.New("imap: Connect on closed client")
	}
	c.client = client
	return nil
}

// resolvedTLSConfig returns the TLS config we hand to imapclient.
// We never mutate the caller-supplied config — we Clone it so
// the per-host ServerName fill-in does not leak between Clients
// built from the same factory.
func (c *Client) resolvedTLSConfig() *tls.Config {
	var cfg *tls.Config
	if c.tlsConfig != nil {
		cfg = c.tlsConfig.Clone()
	} else {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = c.host
	}
	return cfg
}

// Login authenticates with the supplied credentials. Failure
// mode is mapped to imap.ErrAuth so the Manager's backoff path
// can distinguish bad-password (no retry) from connection-reset
// (retry).
func (c *Client) Login(ctx context.Context, username, password string) error {
	client, err := c.activeClient()
	if err != nil {
		return err
	}
	ctx, cancel := c.withCommandDeadline(ctx)
	defer cancel()
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- client.Login(username, password).Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-doneCh:
		if err == nil {
			return nil
		}
		// go-imap/v2 returns *imapclient.Error / generic
		// errors with the IMAP status response code. We
		// substring-match the canonical AUTHENTICATIONFAILED
		// / BAD-USERNAME-OR-PASSWORD tokens; otherwise the
		// error is treated as transient.
		if isAuthFailure(err) {
			return fmt.Errorf("%w: %w", imap.ErrAuth, err)
		}
		return fmt.Errorf("imap: login: %w", err)
	}
}

// Select picks a folder. UIDValidity + UIDNext + Exists round
// off the SelectResult shape the Poller needs.
func (c *Client) Select(ctx context.Context, folder imap.Folder) (imap.SelectResult, error) {
	client, err := c.activeClient()
	if err != nil {
		return imap.SelectResult{}, err
	}
	ctx, cancel := c.withCommandDeadline(ctx)
	defer cancel()
	type result struct {
		data *imapv2.SelectData
		err  error
	}
	doneCh := make(chan result, 1)
	go func() {
		data, err := client.Select(string(folder), &imapv2.SelectOptions{}).Wait()
		doneCh <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return imap.SelectResult{}, ctx.Err()
	case r := <-doneCh:
		if r.err != nil {
			return imap.SelectResult{}, fmt.Errorf("imap: select %q: %w", folder, r.err)
		}
		if r.data == nil {
			return imap.SelectResult{}, fmt.Errorf("imap: select %q: nil response", folder)
		}
		return imap.SelectResult{
			UIDValidity: r.data.UIDValidity,
			UIDNext:     uint32(r.data.UIDNext),
			Exists:      r.data.NumMessages,
		}, nil
	}
}

// FetchAfter fetches all UIDs strictly greater than uidStart,
// capped at limit. The server's UIDSet range syntax accepts
// "uidStart+1:*" so a single FETCH covers the whole tail; we
// then truncate the collected result to limit to enforce the
// per-call cap.
func (c *Client) FetchAfter(ctx context.Context, uidStart uint32, limit int) ([]imap.FetchedMessage, error) {
	if limit <= 0 {
		return nil, nil
	}
	client, err := c.activeClient()
	if err != nil {
		return nil, err
	}
	// Build "uidStart+1:*" range. UIDSet's AddRange interprets
	// 0 as "*" in the stop position.
	low := uidStart + 1
	if low == 0 {
		// uidStart was math.MaxUint32 — no possible
		// successor.
		return nil, nil
	}
	var set imapv2.UIDSet
	set.AddRange(imapv2.UID(low), 0)

	opts := &imapv2.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		BodySection: []*imapv2.FetchItemBodySection{
			{Peek: true},
		},
	}
	ctx, cancel := c.withCommandDeadline(ctx)
	defer cancel()
	type result struct {
		msgs []*imapclient.FetchMessageBuffer
		err  error
	}
	doneCh := make(chan result, 1)
	go func() {
		cmd := client.Fetch(set, opts)
		msgs, err := cmd.Collect()
		doneCh <- result{msgs: msgs, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-doneCh:
		if r.err != nil {
			return nil, fmt.Errorf("imap: fetch after uid %d: %w", uidStart, r.err)
		}
		out := make([]imap.FetchedMessage, 0, len(r.msgs))
		for _, m := range r.msgs {
			fm := convertFetchMessage(m)
			if fm.UID <= uidStart {
				// Belt-and-braces: server returned a
				// message at-or-below our checkpoint.
				// Skip rather than re-process.
				continue
			}
			out = append(out, fm)
			if len(out) >= limit {
				break
			}
		}
		return out, nil
	}
}

// Logout sends an IMAP LOGOUT then drops the connection. Errors
// from either step are returned but the transport is closed
// regardless so the caller's defer-Logout pattern leaves no
// half-open socket on error.
//
// Crucially, Logout does NOT set the terminal closed flag — it
// returns the Client to the Pre-Connect state so the Poller's
// next cycle can Connect() again. Only Close() (the supervisor's
// cleanup path) transitions to the terminal Closed state. If
// Logout is called on a Closed Client the call is a no-op.
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil
	}
	ctx, cancel := c.withCommandDeadline(ctx)
	defer cancel()
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- client.Logout().Wait()
	}()
	var logoutErr error
	select {
	case <-ctx.Done():
		logoutErr = ctx.Err()
	case err := <-doneCh:
		logoutErr = err
	}
	// Always close the transport, even when LOGOUT errored:
	// the connection is unusable either way.
	closeErr := client.Close()
	c.mu.Lock()
	c.client = nil
	c.mu.Unlock()
	if logoutErr != nil {
		return fmt.Errorf("imap: logout: %w", logoutErr)
	}
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return fmt.Errorf("imap: close transport: %w", closeErr)
	}
	return nil
}

// Close drops the underlying transport without any wire-protocol
// handshake. Safe to call on a never-Connect()'d Client, on a
// Connected-but-not-Login'd Client, on a fully-Login'd Client,
// and after a previous Close. Idempotent per the interface
// contract.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	client := c.client
	c.client = nil
	c.closed = true
	c.mu.Unlock()
	if client == nil {
		return nil
	}
	err := client.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("imap: close: %w", err)
	}
	return nil
}

// activeClient returns the underlying *imapclient.Client and an
// error when the Client has not been Connect()'d or has been
// Close()d. Callers funnel through this helper so the error
// shape is consistent across Login/Select/Fetch/Logout.
func (c *Client) activeClient() (*imapclient.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("imap: client closed")
	}
	if c.client == nil {
		return nil, errors.New("imap: client not connected")
	}
	return c.client, nil
}

// withCommandDeadline derives a child context that fires at the
// earlier of ctx and the per-command timeout. The returned
// cancel func MUST be called by the caller (typically via
// defer) to release resources whether the command finishes
// promptly or the deadline fires.
func (c *Client) withCommandDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.cmdTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.cmdTimeout)
}

// isAuthFailure tests whether err looks like a permanent
// authentication failure from the server. go-imap/v2 surfaces
// these as a string-formatted IMAP response code which is the
// best signal we have — there is no typed sentinel upstream.
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, needle := range []string{
		"authenticationfailed",
		"auth-too-weak",
		"authorizationfailed",
		"login-disabled",
		"invalid credentials",
		"bad username or password",
		"username or password",
		"authentication failed",
		// LOGINDISABLED capability response means the server
		// refuses plain LOGIN; treat as auth failure so the
		// operator surfaces it rather than retrying.
		"logindisabled",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// convertFetchMessage translates a go-imap/v2 fetch buffer into
// the helpdesk FetchedMessage shape. The raw RFC-822 bytes come
// from the first BODY[] section we asked for; flags + UID +
// InternalDate are copied straight across.
func convertFetchMessage(m *imapclient.FetchMessageBuffer) imap.FetchedMessage {
	if m == nil {
		return imap.FetchedMessage{}
	}
	fm := imap.FetchedMessage{
		UID:    uint32(m.UID),
		SeenAt: m.InternalDate,
	}
	if len(m.Flags) > 0 {
		flags := make([]string, 0, len(m.Flags))
		for _, f := range m.Flags {
			flags = append(flags, string(f))
		}
		fm.Flags = flags
	}
	// FindBodySection returns the bytes for the requested
	// FetchItemBodySection (whole message body).
	//
	// Look up the non-Peek shape first. Per RFC 3501 §6.4.5, a
	// FETCH BODY.PEEK[] command is answered with a BODY[]
	// response (PEEK is a request-only modifier that suppresses
	// the implicit \Seen flag), so go-imap/v2's response buffer
	// stores the section with Peek=false. The Peek=true lookup
	// is a defensive fallback for any implementation that
	// preserves the request flag in the buffer key.
	body := m.FindBodySection(&imapv2.FetchItemBodySection{})
	if body == nil {
		body = m.FindBodySection(&imapv2.FetchItemBodySection{Peek: true})
	}
	fm.Body = body
	return fm
}
