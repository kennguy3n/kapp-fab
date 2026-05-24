package mailboxes

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validMailbox(t *testing.T) Mailbox {
	t.Helper()
	return Mailbox{
		TenantID:        uuid.New(),
		MailboxID:       uuid.New(),
		Name:            "support",
		IMAPHost:        "imap.gmail.com",
		IMAPPort:        993,
		IMAPUsername:    "support@example.com",
		IMAPPasswordRef: "vault://kapp/helpdesk/support/password",
		IMAPUseTLS:      true,
		Folder:          "INBOX",
		Enabled:         true,
	}
}

func TestValidate_HappyPath(t *testing.T) {
	m := validMailbox(t)
	if err := m.Validate(); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidate_RejectsZeroTenant(t *testing.T) {
	m := validMailbox(t)
	m.TenantID = uuid.Nil
	if err := m.Validate(); !errors.Is(err, ErrInvalidTenantID) {
		t.Fatalf("expected ErrInvalidTenantID, got %v", err)
	}
}

func TestValidate_RejectsZeroMailbox(t *testing.T) {
	m := validMailbox(t)
	m.MailboxID = uuid.Nil
	if err := m.Validate(); !errors.Is(err, ErrInvalidMailboxID) {
		t.Fatalf("expected ErrInvalidMailboxID, got %v", err)
	}
}

// TestValidate_BoundaryMatrix pins the per-field invariants. Each
// case mutates ONE field of a valid Mailbox and asserts the
// matching error sentinel — this catches cross-field regressions
// (e.g. accidentally checking Host where we meant Folder) that a
// single-field test would miss.
func TestValidate_BoundaryMatrix(t *testing.T) {
	zero := 0
	negative := -1
	cases := []struct {
		name    string
		mutate  func(*Mailbox)
		wantErr error
	}{
		{"name_empty", func(m *Mailbox) { m.Name = "" }, ErrInvalidName},
		{"name_whitespace", func(m *Mailbox) { m.Name = "   " }, ErrInvalidName},
		{"host_empty", func(m *Mailbox) { m.IMAPHost = "" }, ErrInvalidHost},
		{"port_zero", func(m *Mailbox) { m.IMAPPort = 0 }, ErrInvalidPort},
		{"port_negative", func(m *Mailbox) { m.IMAPPort = -1 }, ErrInvalidPort},
		{"port_over_65535", func(m *Mailbox) { m.IMAPPort = 70000 }, ErrInvalidPort},
		{"username_empty", func(m *Mailbox) { m.IMAPUsername = "" }, ErrInvalidUsername},
		{"password_ref_empty", func(m *Mailbox) { m.IMAPPasswordRef = "" }, ErrInvalidPasswordRef},
		{"password_ref_plaintext", func(m *Mailbox) { m.IMAPPasswordRef = "hunter2" }, ErrPasswordRefLooksPlain},
		{"folder_empty", func(m *Mailbox) { m.Folder = "" }, ErrInvalidFolder},
		{"poll_interval_zero", func(m *Mailbox) { m.PollIntervalSeconds = &zero }, ErrInvalidPollInterval},
		{"poll_interval_negative", func(m *Mailbox) { m.PollIntervalSeconds = &negative }, ErrInvalidPollInterval},
		{"max_backoff_zero", func(m *Mailbox) { m.MaxBackoffSeconds = &zero }, ErrInvalidMaxBackoff},
		{"fetch_batch_zero", func(m *Mailbox) { m.FetchBatchSize = &zero }, ErrInvalidFetchBatchSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMailbox(t)
			tc.mutate(&m)
			err := m.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidate_PasswordRefSchemeMatrix pins the
// looksLikePlaintextPassword heuristic. Every value in the
// "should pass" list MUST validate, every value in the "should
// reject" list MUST hit ErrPasswordRefLooksPlain. Together they
// document the operator-facing contract for what an
// imap_password_ref is allowed to look like.
func TestValidate_PasswordRefSchemeMatrix(t *testing.T) {
	shouldPass := []string{
		"env:KAPP_HELPDESK_PASSWORD_ACME",
		"vault://kapp/helpdesk/acme/imap-password",
		"aws://arn:aws:secretsmanager:us-east-1:123456789012:secret:kapp/helpdesk/acme",
		"aws:arn:aws:secretsmanager:us-east-1:123456789012:secret:kapp/helpdesk/acme",
		"gcp://projects/my-project/secrets/kapp-helpdesk-acme/versions/latest",
		"gcp:projects/my-project/secrets/kapp-helpdesk-acme/versions/latest",
		"file:///var/run/secrets/kapp/helpdesk-password",
		"file:/var/run/secrets/kapp/helpdesk-password",
	}
	for _, ref := range shouldPass {
		t.Run("pass_"+ref, func(t *testing.T) {
			m := validMailbox(t)
			m.IMAPPasswordRef = ref
			if err := m.Validate(); err != nil {
				t.Fatalf("expected nil for %q, got %v", ref, err)
			}
		})
	}
	shouldReject := []string{
		"hunter2",
		"plaintext-password-paste",
		"P@ssw0rd!",
		"justastring",
	}
	for _, ref := range shouldReject {
		t.Run("reject_"+ref, func(t *testing.T) {
			m := validMailbox(t)
			m.IMAPPasswordRef = ref
			err := m.Validate()
			if !errors.Is(err, ErrPasswordRefLooksPlain) {
				t.Fatalf("expected ErrPasswordRefLooksPlain for %q, got %v", ref, err)
			}
		})
	}
}

func TestPollInterval_DefaultWhenNil(t *testing.T) {
	m := validMailbox(t)
	m.PollIntervalSeconds = nil
	if got := m.PollInterval(); got != DefaultPollInterval {
		t.Fatalf("expected DefaultPollInterval (%v), got %v", DefaultPollInterval, got)
	}
}

func TestPollInterval_HonoursExplicit(t *testing.T) {
	seconds := 120
	m := validMailbox(t)
	m.PollIntervalSeconds = &seconds
	want := 120 * time.Second
	if got := m.PollInterval(); got != want {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestMaxBackoff_DefaultAndExplicit(t *testing.T) {
	m := validMailbox(t)
	if got := m.MaxBackoff(); got != DefaultMaxBackoff {
		t.Fatalf("default: expected %v, got %v", DefaultMaxBackoff, got)
	}
	seconds := 30
	m.MaxBackoffSeconds = &seconds
	if got := m.MaxBackoff(); got != 30*time.Second {
		t.Fatalf("explicit: expected 30s, got %v", got)
	}
}

func TestFetchBatchSize_DefaultAndExplicit(t *testing.T) {
	m := validMailbox(t)
	if got := m.FetchBatchSizeOrDefault(); got != DefaultFetchBatchSize {
		t.Fatalf("default: expected %d, got %d", DefaultFetchBatchSize, got)
	}
	size := 25
	m.FetchBatchSize = &size
	if got := m.FetchBatchSizeOrDefault(); got != 25 {
		t.Fatalf("explicit: expected 25, got %d", got)
	}
}
