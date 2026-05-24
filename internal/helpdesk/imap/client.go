// Package imap implements the helpdesk IMAP poller. The Poller
// connects to an IMAP server, SELECT's a folder (typically INBOX),
// searches for messages with UID above the last-processed
// checkpoint, fetches each one's RFC-822 body, hands it off to the
// helpdesk inbound-email handler, and advances the checkpoint.
//
// The actual IMAP wire protocol is abstracted behind a Client
// interface so:
//
//   - The Poller is unit-testable against an in-memory fake
//     without a real IMAP server.
//   - The choice of IMAP library is a wiring decision \u2014 the
//     production implementation will adapt go-imap/v2 (or any
//     other client) without touching the orchestrator.
//
// Threading + Message-ID dedup live elsewhere: the Poller passes
// each parsed email to the handler's ProcessThreaded path which
// owns ThreadingResolver + MessageStore (Surface A).
package imap

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"
)

// Folder identifies a server-side mailbox (e.g. "INBOX",
// "Support/Tickets"). IMAP folder names are case-sensitive on
// most servers; we pass them through unchanged.
type Folder string

// FetchedMessage is what the Client returns per UID. Body is the
// raw RFC-822 wire bytes (headers + body); the Poller parses it
// into helpdesk's InboundEmail shape.
type FetchedMessage struct {
	UID    uint32
	Flags  []string
	Body   []byte
	// SeenAt is the server's INTERNALDATE for the message,
	// used as a freshness signal when the headers lack a
	// Date: value or have an obviously-wrong date.
	SeenAt time.Time
}

// SelectResult captures the response to an IMAP SELECT command:
// the UIDVALIDITY value (which invalidates checkpoints on change)
// and the UIDNEXT (a hint about the next-issued UID; unused by
// the Poller today but recorded so the dashboard can render
// "0 / 12345 messages processed").
type SelectResult struct {
	UIDValidity uint32
	UIDNext     uint32
	Exists      uint32
}

// Client is the IMAP wire-protocol surface the Poller needs. Each
// poll cycle:
//
//   1. Connect()      \u2014 dial + TLS handshake.
//   2. Login()        \u2014 authenticate.
//   3. Select()       \u2014 pick the folder; returns UIDValidity.
//   4. SearchUIDsAbove() or FetchAfter() depending on impl.
//   5. Fetch() each new UID's body.
//   6. (optional) Flag() or Move() to mark processed.
//   7. Logout()       \u2014 clean shutdown.
//
// Connect / Login are split so impls can return rich errors
// distinguishing network from auth failures (which the Manager
// uses to decide retry-with-backoff vs alert-operator).
type Client interface {
	// Connect dials the server. Errors here are network-level
	// (DNS, refused, TLS handshake) \u2014 retry-with-backoff.
	Connect(ctx context.Context) error

	// Login authenticates. Errors here are credentials-level \u2014
	// surface to operator (no auto-retry).
	Login(ctx context.Context, username, password string) error

	// Select picks a folder and returns UIDValidity + UIDNext.
	Select(ctx context.Context, folder Folder) (SelectResult, error)

	// FetchAfter returns messages with UID strictly greater
	// than uidStart. The limit caps a single fetch to avoid an
	// unbounded result set on first-poll-against-old-mailbox
	// scenarios; the Poller calls FetchAfter in a loop until
	// it returns an empty batch.
	FetchAfter(ctx context.Context, uidStart uint32, limit int) ([]FetchedMessage, error)

	// Logout closes the connection cleanly. It sends the IMAP
	// LOGOUT command and waits for the server's acknowledgement
	// before returning, then closes the underlying transport.
	// Errors are reported but the connection is closed regardless.
	Logout(ctx context.Context) error

	// Close forcibly tears down the underlying transport without
	// attempting an IMAP-level handshake. It is the cleanup hook
	// for any caller that stashed a freshly-built Client but
	// could not hand it off to a Poller (e.g. Manager.Start failed
	// because the worker is shutting down, the row was disabled
	// between converge() and Start, or a transient lock raced the
	// goroutine spawn).
	//
	// Close must be idempotent — callers may call it more than
	// once and expect the second call to be a no-op. It must not
	// panic on a Client that was never Connect()'d.
	//
	// Close exists in addition to Logout because Logout assumes
	// an active IMAP session: it sends a wire-protocol command
	// and reads the response. When the supervisor's converge loop
	// builds a Client but Manager.Start short-circuits (because a
	// previous Start race won the entry, or because the manager
	// has been Stopped) the Client is in a half-open state — no
	// session yet, but a TCP / TLS connection may still be open.
	// Logout would block or error on that state; Close just
	// drops the socket.
	Close() error
}

// ErrAuth is returned by Login on permanent authentication
// failure. Tests + the Manager use errors.Is to distinguish from
// transient network errors.
var ErrAuth = errors.New("imap: authentication failed")

// ErrUIDValidityChanged signals the caller that the IMAP server's
// UIDVALIDITY differs from our checkpoint. The Poller treats this
// as "reset last_uid + re-scan from 0"; Message-ID dedup at the
// email_messages PRIMARY KEY layer ensures duplicates don't
// double-insert.
var ErrUIDValidityChanged = errors.New("imap: uid validity changed")

// ParsedEmail is the result of parsing one FetchedMessage's body
// into the headers + body the inbound handler consumes. The
// Poller does just enough parsing to extract Message-ID,
// In-Reply-To, References, From, To, Subject, and the body text
// \u2014 attachment dispatch happens in a follow-up commit (Surface D
// integration lands in the wire-up commit).
type ParsedEmail struct {
	MessageID  string
	InReplyTo  string
	References []string
	From       string
	To         string
	Subject    string
	Body       string
	RawDate    time.Time
}

// ParseRFC822 parses an RFC-822 message into the helpdesk-relevant
// fields. Exposed for tests and the IMAP integration path. The
// body is the plain text first body part; multipart/mixed
// envelopes return the first text/plain part if present,
// otherwise the raw body.
//
// We deliberately use net/mail (stdlib) for the headers and a
// minimal home-grown multipart split rather than pulling in a
// MIME library. Reasons:
//
//   - net/mail handles header folding + RFC-2047 encoded-word
//     decoding for us.
//   - The body extraction we need (find the text/plain part)
//     is shallow enough that a 30-line walker covers it without
//     adding 4MB of dependency.
//   - The threading headers (Message-ID, In-Reply-To,
//     References) live in the headers; the Poller doesn't care
//     about deeper MIME structure (attachments are handled
//     separately by Surface D).
func ParseRFC822(raw []byte) (ParsedEmail, error) {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return ParsedEmail{}, err
	}

	subject := decodeHeader(msg.Header.Get("Subject"))
	from := decodeHeader(msg.Header.Get("From"))
	to := decodeHeader(msg.Header.Get("To"))
	messageID := stripAngle(msg.Header.Get("Message-ID"))
	inReplyTo := stripAngle(msg.Header.Get("In-Reply-To"))
	references := parseReferencesHeader(msg.Header.Get("References"))

	var date time.Time
	if d, err := msg.Header.Date(); err == nil {
		date = d
	}

	body, err := extractTextBody(msg)
	if err != nil {
		return ParsedEmail{}, err
	}

	return ParsedEmail{
		MessageID:  messageID,
		InReplyTo:  inReplyTo,
		References: references,
		From:       from,
		To:         to,
		Subject:    subject,
		Body:       body,
		RawDate:    date,
	}, nil
}

// extractTextBody returns the message body as text. For
// single-part messages, this is the body verbatim. For
// multipart/* messages, we walk the first level looking for a
// text/plain part. Nested multipart (e.g. multipart/alternative
// inside multipart/mixed) is one-level-deep \u2014 the helpdesk's
// agent UI shows quoted text anyway, so a fancier walk doesn't
// improve UX.
func extractTextBody(msg *mail.Message) (string, error) {
	ct := msg.Header.Get("Content-Type")
	bodyBytes, err := readAll(msg.Body)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/") {
		return string(bodyBytes), nil
	}
	boundary := boundaryFromContentType(ct)
	if boundary == "" {
		return string(bodyBytes), nil
	}
	parts := splitOnBoundary(string(bodyBytes), boundary)
	// Prefer the first text/plain part; if none, return the
	// first part as-is.
	for _, p := range parts {
		headers, body := splitHeaderBody(p)
		hdrCT := strings.ToLower(headers["content-type"])
		if strings.HasPrefix(hdrCT, "text/plain") {
			return body, nil
		}
	}
	if len(parts) > 0 {
		_, body := splitHeaderBody(parts[0])
		return body, nil
	}
	return string(bodyBytes), nil
}
