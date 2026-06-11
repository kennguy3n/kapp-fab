package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/ledger/bankfeed"
)

// bankFeedDispatcher returns a dispatcher whose bank-feed stores are
// constructed with nil pools. Argument-validation paths return before
// any store method runs, so they never touch the pool; reaching a store
// call would panic and thereby flag a missing guard. The registry is
// built from an empty config (CSV-only, no live providers), and the
// sync handler wires the same nil-backed stores.
func bankFeedDispatcher() *CommandDispatcher {
	conns := bankfeed.NewConnectionStore(nil, nil, nil)
	rules := bankfeed.NewRuleStore(nil, nil)
	matcher := ledger.NewSmartMatcher(nil)
	registry := bankfeed.BuildRegistry(bankfeed.RegistryConfig{})
	sync := bankfeed.NewSyncHandler(conns, rules, registry, nil, matcher)
	return &CommandDispatcher{
		bankConns:   conns,
		bankMatcher: matcher,
		bankSync:    sync,
	}
}

func TestBankFeedNotConfigured(t *testing.T) {
	t.Parallel()
	d := &CommandDispatcher{} // bank-feed stores == nil
	resp, err := d.bankFeed(context.Background(), CommandRequest{TenantID: uuid.New(), UserID: uuid.New()})
	if err != nil {
		t.Fatalf("bankFeed: %v", err)
	}
	if !strings.Contains(resp.Text, "not configured") {
		t.Fatalf("want 'not configured', got %q", resp.Text)
	}
}

func TestBankFeedRequiresTenant(t *testing.T) {
	t.Parallel()
	resp, err := bankFeedDispatcher().bankFeed(context.Background(), CommandRequest{})
	if err != nil {
		t.Fatalf("bankFeed: %v", err)
	}
	if !strings.Contains(resp.Text, "tenant_id required") {
		t.Fatalf("want 'tenant_id required', got %q", resp.Text)
	}
}

func TestBankFeedArgValidation(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	user := uuid.New()
	d := bankFeedDispatcher()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown subcommand", []string{"frobnicate"}, "Usage: /bankfeed"},
		{"connections bad account id", []string{"connections", "not-a-uuid"}, "invalid bank_account_id"},
		{"suggestions missing id", []string{"suggestions"}, "Usage: /bankfeed suggestions"},
		{"suggestions bad id", []string{"suggestions", "nope"}, "invalid bank_account_id"},
		{"accept missing id", []string{"accept"}, "Usage: /bankfeed accept"},
		{"accept bad id", []string{"accept", "nope"}, "invalid suggestion_id"},
		{"reject missing id", []string{"reject"}, "Usage: /bankfeed reject"},
		{"reject bad id", []string{"reject", "nope"}, "invalid suggestion_id"},
		{"sync missing id", []string{"sync"}, "Usage: /bankfeed sync"},
		{"sync bad id", []string{"sync", "nope"}, "invalid connection_id"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			resp, err := d.bankFeed(context.Background(), CommandRequest{TenantID: tenant, UserID: user, Args: c.args})
			if err != nil {
				t.Fatalf("bankFeed: %v", err)
			}
			if !strings.Contains(resp.Text, c.want) {
				t.Fatalf("args=%v: want text containing %q, got %q", c.args, c.want, resp.Text)
			}
		})
	}
}
