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
// treats clients as opaque handles so no methods need to fire.
type fakeIMAPClient struct{ imap.Client }

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

// TestResolveMailboxPassword_Env pins the env: scheme.
func TestResolveMailboxPassword_Env(t *testing.T) {
	const key = "KAPP_TEST_IMAP_PASSWORD"
	t.Setenv(key, "s3kret")
	val, err := resolveMailboxPassword(context.Background(), "env:"+key)
	if err != nil {
		t.Fatalf("env: with value: %v", err)
	}
	if val != "s3kret" {
		t.Errorf("env: want s3kret, got %q", val)
	}

	// Empty value
	t.Setenv(key, "")
	if _, err := resolveMailboxPassword(context.Background(), "env:"+key); err == nil {
		t.Errorf("env: with empty value: expected error, got nil")
	}

	// Unset key
	if _, err := resolveMailboxPassword(context.Background(), "env:KAPP_TEST_DEFINITELY_NOT_SET_"+t.Name()); err == nil {
		t.Errorf("env: with unset var: expected error, got nil")
	}
}

// TestResolveMailboxPassword_EmptyRef pins the early-return for
// blank refs.
func TestResolveMailboxPassword_EmptyRef(t *testing.T) {
	if _, err := resolveMailboxPassword(context.Background(), ""); err == nil {
		t.Errorf("expected error for empty ref")
	}
	if _, err := resolveMailboxPassword(context.Background(), "   "); err == nil {
		t.Errorf("expected error for whitespace-only ref")
	}
}

// TestResolveMailboxPassword_UnsupportedScheme pins the
// not-yet-wired schemes: each returns a clear error mentioning
// the scheme so the supervisor's converge log line reads cleanly.
func TestResolveMailboxPassword_UnsupportedScheme(t *testing.T) {
	cases := []string{
		"vault://kv/data/foo",
		"aws://arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
		"gcp://projects/p/secrets/foo/versions/latest",
		"file:///etc/secrets/foo",
		"plaintext-leak",
	}
	for _, ref := range cases {
		_, err := resolveMailboxPassword(context.Background(), ref)
		if err == nil {
			t.Errorf("ref %q: expected error, got nil", ref)
		}
	}
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
