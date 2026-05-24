// Surface G (PR-7) — unit tests for the Helpdesk IMAP wire-up
// helpers in helpdesk_imap.go. The supervisor's full converge loop
// + processor's Process method require a real
// *helpdesk.InboundEmailHandler + DB-backed mailboxes.Store and
// are exercised by the worker integration tests (gated by the
// IMAP client adapter PR). The helpers below are stand-alone and
// unit-testable in process.
package main

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk/imap"
)

// TestRecipientTable_SetGetDelete pins the (tenantID, mailboxID) →
// recipient lookup used by helpdeskIMAPProcessor.
func TestRecipientTable_SetGetDelete(t *testing.T) {
	tbl := newRecipientTable()
	tenant1 := uuid.New()
	tenant2 := uuid.New()
	mb1 := uuid.New()
	mb2 := uuid.New()

	tbl.set(tenant1, mb1, "a@x.com")
	tbl.set(tenant1, mb2, "b@x.com")
	tbl.set(tenant2, mb1, "c@y.com")

	if got := tbl.get(tenant1, mb1); got != "a@x.com" {
		t.Errorf("want a@x.com, got %q", got)
	}
	if got := tbl.get(tenant1, mb2); got != "b@x.com" {
		t.Errorf("want b@x.com, got %q", got)
	}
	if got := tbl.get(tenant2, mb1); got != "c@y.com" {
		t.Errorf("want c@y.com, got %q", got)
	}
	if got := tbl.get(uuid.New(), mb1); got != "" {
		t.Errorf("want empty for unknown tenant, got %q", got)
	}

	// deleteMailbox removes the mailboxID from every tenant
	// because the supervisor doesn't carry tenant context when
	// it observes a stopped poller — Manager.Stop is keyed by
	// mailbox UUID alone (mailbox UUIDs are globally unique).
	tbl.deleteMailbox(mb1)
	if got := tbl.get(tenant1, mb1); got != "" {
		t.Errorf("expected empty after delete (tenant1), got %q", got)
	}
	if got := tbl.get(tenant2, mb1); got != "" {
		t.Errorf("expected empty after delete (tenant2), got %q", got)
	}
	// Untouched rows must survive.
	if got := tbl.get(tenant1, mb2); got != "b@x.com" {
		t.Errorf("want b@x.com after unrelated delete, got %q", got)
	}
}

// TestRecipientTable_ConcurrentAccess pins thread-safety: many
// concurrent set+get+delete calls don't race. Run with -race in
// CI to actually catch races.
func TestRecipientTable_ConcurrentAccess(t *testing.T) {
	t.Helper()
	tbl := newRecipientTable()
	tenant := uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mb := uuid.New()
			tbl.set(tenant, mb, "x@y.com")
			_ = tbl.get(tenant, mb)
			tbl.deleteMailbox(mb)
		}()
	}
	wg.Wait()
}

// fakeIMAPClient is an opaque stand-in imap.Client; the registry
// treats clients as opaque handles so most methods are inherited
// (and would panic if invoked) — only Close is explicitly
// implemented so the supervisor's Start-failure cleanup path can
// verify it fires.
type fakeIMAPClient struct {
	imap.Client
	closeCalls int
	closeErr   error
}

func (f *fakeIMAPClient) Close() error {
	f.closeCalls++
	return f.closeErr
}

// TestClientRegistry_PutTake pins take-is-pop semantics. The
// manager hook calls take exactly once per Start so a leak is at
// most one client per failed Start.
func TestClientRegistry_PutTake(t *testing.T) {
	reg := newClientRegistry()
	mb := uuid.New()
	if _, ok := reg.take(mb); ok {
		t.Fatalf("take on empty registry returned ok=true")
	}
	want := &fakeIMAPClient{}
	reg.put(mb, want)
	got, ok := reg.take(mb)
	if !ok {
		t.Fatalf("take after put returned ok=false")
	}
	if got != want {
		t.Errorf("take returned wrong client")
	}
	// Second take must miss — put is consumed.
	if _, ok := reg.take(mb); ok {
		t.Errorf("second take returned ok=true (registry should be empty)")
	}
}

// TestStaticPasswordResolver_Env pins the legacy env:NAME path
// used by tests that don't wire the production secrets package.
// The static resolver is the fallback the supervisor never
// reaches in production; this test exists to prevent regressions
// in the test-double itself.
func TestStaticPasswordResolver_Env(t *testing.T) {
	const key = "KAPP_TEST_IMAP_PASSWORD"
	t.Setenv(key, "s3kret")
	r := staticPasswordResolver{}
	val, err := r.Resolve(context.Background(), "mailbox-A", "env:"+key)
	if err != nil {
		t.Fatalf("env: with value: %v", err)
	}
	if string(val) != "s3kret" {
		t.Errorf("env: want s3kret, got %q", string(val))
	}

	// Empty value
	t.Setenv(key, "")
	if _, err := r.Resolve(context.Background(), "mailbox-A", "env:"+key); err == nil {
		t.Errorf("env: with empty value: expected error, got nil")
	}

	// Unset key
	if _, err := r.Resolve(context.Background(), "mailbox-A", "env:KAPP_TEST_DEFINITELY_NOT_SET_"+t.Name()); err == nil {
		t.Errorf("env: with unset var: expected error, got nil")
	}
}

// TestStaticPasswordResolver_EmptyRef pins the early-return for
// blank refs in the test fallback.
func TestStaticPasswordResolver_EmptyRef(t *testing.T) {
	r := staticPasswordResolver{}
	if _, err := r.Resolve(context.Background(), "mailbox-A", ""); err == nil {
		t.Errorf("expected error for empty ref")
	}
	if _, err := r.Resolve(context.Background(), "mailbox-A", "   "); err == nil {
		t.Errorf("expected error for whitespace-only ref")
	}
}

// TestStaticPasswordResolver_UnsupportedScheme pins the static
// resolver's narrow contract — env: only. The production
// resolver dispatches every supported scheme (see
// secrets.RefResolver tests); the static resolver intentionally
// rejects anything else so accidentally-disabled secrets wiring
// in tests fails loudly rather than masking config bugs.
func TestStaticPasswordResolver_UnsupportedScheme(t *testing.T) {
	r := staticPasswordResolver{}
	cases := []string{
		"vault://kv/data/foo",
		"aws://arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
		"gcp://projects/p/secrets/foo/versions/latest",
		"file:///etc/secrets/foo",
		"plaintext-leak",
	}
	for _, ref := range cases {
		_, err := r.Resolve(context.Background(), "mailbox-A", ref)
		if err == nil {
			t.Errorf("ref %q: expected error, got nil", ref)
		}
	}
}

// TestStaticPasswordResolver_InvalidateScope pins the no-op
// contract so the supervisor's deleteMailbox path can call it
// uniformly without branching on resolver type.
func TestStaticPasswordResolver_InvalidateScope(_ *testing.T) {
	r := staticPasswordResolver{}
	r.InvalidateScope("mailbox-A")
	r.InvalidateScope("")
}

// TestSchemeOf pins the diagnostic-logging helper.
func TestSchemeOf(t *testing.T) {
	cases := map[string]string{
		"env:FOO":                      "env",
		"vault://kv/data/foo":          "vault",
		"aws://arn:secret:foo":         "aws",
		"gcp://projects/p/secrets/foo": "gcp",
		"file:///etc/secrets/foo":      "file",
		"no-scheme":                    "no-scheme",
		"":                             "",
	}
	for in, want := range cases {
		if got := schemeOf(in); got != want {
			t.Errorf("schemeOf(%q): want %q, got %q", in, want, got)
		}
	}
}
