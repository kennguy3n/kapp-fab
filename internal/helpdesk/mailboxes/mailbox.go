// Package mailboxes is the per-tenant IMAP-mailbox configuration
// layer for the helpdesk inbound-email pipeline.
//
// A Mailbox row holds everything the worker's IMAP supervisor needs
// to bring up one polling goroutine: connection target (host + port +
// TLS), credentials (resolved via SecretProvider — the plaintext
// password never lands in this table), folder, cadence + backoff
// tuning. The Store interface is the read/write seam used by the
// admin API (CRUD per tenant) and the worker's supervisor (read-only
// enumerate-enabled scan across all tenants).
//
// One tenant may attach multiple mailboxes; the typical layout is one
// row for `support@<tenant>.kapp.io` and additional rows for
// `billing@…`, `sales@…`, etc., each with its own credentials and
// poll cadence. The PRIMARY KEY (tenant_id, mailbox_id) keeps the
// rows tenant-scoped under RLS.
package mailboxes

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Default poller tuning. These match the defaults baked into
// internal/helpdesk/imap.Config so a row that leaves the optional
// fields NULL produces the same behaviour as the bare worker default.
const (
	DefaultPollInterval   = 60 * time.Second
	DefaultMaxBackoff     = 15 * time.Minute
	DefaultFetchBatchSize = 100
	DefaultIMAPPort       = 993
	DefaultFolder         = "INBOX"
)

// Mailbox is one row of helpdesk_mailboxes. The optional poller-tuning
// fields use a *T pointer rather than a sentinel zero value so a
// caller can distinguish "operator set this to 0" from "operator
// didn't set it; use the worker default". The Store layer encodes
// nil pointers as NULL.
type Mailbox struct {
	TenantID            uuid.UUID
	MailboxID           uuid.UUID
	Name                string
	IMAPHost            string
	IMAPPort            int
	IMAPUsername        string
	IMAPPasswordRef     string
	IMAPUseTLS          bool
	Folder              string
	PollIntervalSeconds *int
	MaxBackoffSeconds   *int
	FetchBatchSize      *int
	Enabled             bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Validation sentinels. Each carries the field name in its message so
// the admin-API layer can surface a structured 400 to the caller
// without parsing the error string.
var (
	ErrInvalidName            = errors.New("mailboxes: name must be non-empty")
	ErrInvalidHost            = errors.New("mailboxes: imap_host must be non-empty")
	ErrInvalidPort            = errors.New("mailboxes: imap_port must be 1..65535")
	ErrInvalidUsername        = errors.New("mailboxes: imap_username must be non-empty")
	ErrInvalidPasswordRef     = errors.New("mailboxes: imap_password_ref must be non-empty (a SecretProvider key, NEVER a plaintext password)")
	ErrInvalidFolder          = errors.New("mailboxes: folder must be non-empty")
	ErrInvalidPollInterval    = errors.New("mailboxes: poll_interval_seconds must be > 0 when set")
	ErrInvalidMaxBackoff      = errors.New("mailboxes: max_backoff_seconds must be > 0 when set")
	ErrInvalidFetchBatchSize  = errors.New("mailboxes: fetch_batch_size must be > 0 when set")
	ErrInvalidTenantID        = errors.New("mailboxes: tenant_id is required")
	ErrInvalidMailboxID       = errors.New("mailboxes: mailbox_id is required")
	ErrPasswordRefLooksPlain  = errors.New("mailboxes: imap_password_ref looks like a plaintext password (must be a SecretProvider key such as 'env:NAME', 'vault://...', 'aws://arn:...', 'gcp://projects/...', or 'file://...')")
	ErrNotFound               = errors.New("mailboxes: not found")
	ErrDuplicateNameForTenant = errors.New("mailboxes: name already in use for this tenant")
)

// Validate runs the per-field invariants the Store + admin-API both
// enforce. Called on Create and Update; the worker's supervisor does
// NOT call this on read (the DB-side CHECK / NOT NULL constraints
// are the source of truth for already-persisted rows).
func (m *Mailbox) Validate() error {
	if m.TenantID == uuid.Nil {
		return ErrInvalidTenantID
	}
	if m.MailboxID == uuid.Nil {
		return ErrInvalidMailboxID
	}
	if strings.TrimSpace(m.Name) == "" {
		return ErrInvalidName
	}
	if strings.TrimSpace(m.IMAPHost) == "" {
		return ErrInvalidHost
	}
	if m.IMAPPort <= 0 || m.IMAPPort > 65535 {
		return ErrInvalidPort
	}
	if strings.TrimSpace(m.IMAPUsername) == "" {
		return ErrInvalidUsername
	}
	if strings.TrimSpace(m.IMAPPasswordRef) == "" {
		return ErrInvalidPasswordRef
	}
	if looksLikePlaintextPassword(m.IMAPPasswordRef) {
		return ErrPasswordRefLooksPlain
	}
	if strings.TrimSpace(m.Folder) == "" {
		return ErrInvalidFolder
	}
	if m.PollIntervalSeconds != nil && *m.PollIntervalSeconds <= 0 {
		return ErrInvalidPollInterval
	}
	if m.MaxBackoffSeconds != nil && *m.MaxBackoffSeconds <= 0 {
		return ErrInvalidMaxBackoff
	}
	if m.FetchBatchSize != nil && *m.FetchBatchSize <= 0 {
		return ErrInvalidFetchBatchSize
	}
	return nil
}

// PollInterval resolves the row's tuning to a concrete duration,
// substituting DefaultPollInterval when the row leaves the field
// unset (nil). The worker uses this when constructing the imap.Config.
func (m *Mailbox) PollInterval() time.Duration {
	if m.PollIntervalSeconds == nil {
		return DefaultPollInterval
	}
	return time.Duration(*m.PollIntervalSeconds) * time.Second
}

// MaxBackoff mirrors PollInterval for the exponential-backoff cap.
func (m *Mailbox) MaxBackoff() time.Duration {
	if m.MaxBackoffSeconds == nil {
		return DefaultMaxBackoff
	}
	return time.Duration(*m.MaxBackoffSeconds) * time.Second
}

// FetchBatchSizeOrDefault mirrors PollInterval for the FETCH cap.
func (m *Mailbox) FetchBatchSizeOrDefault() int {
	if m.FetchBatchSize == nil {
		return DefaultFetchBatchSize
	}
	return *m.FetchBatchSize
}

// looksLikePlaintextPassword is a heuristic guard against operators
// who paste a raw password into imap_password_ref by accident. A
// real ref always carries one of the known scheme prefixes (env:,
// vault://, aws://, gcp://, file://). Anything without a recognised
// scheme is rejected with a clear error so the operator notices at
// Create time rather than at first-poll time.
//
// The heuristic is conservative — we only reject when we can be
// confident the value is NOT a ref. A custom-scheme installation
// (e.g. a new provider added downstream) can still pass an arbitrary
// scheme; only schemeless inputs fail.
func looksLikePlaintextPassword(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false // handled by ErrInvalidPasswordRef above
	}
	knownSchemes := []string{"env:", "vault://", "aws://", "aws:arn:", "gcp://", "gcp:projects/", "file://", "file:/"}
	for _, p := range knownSchemes {
		if strings.HasPrefix(ref, p) {
			return false
		}
	}
	return true
}
