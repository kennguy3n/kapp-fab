//go:build integration
// +build integration

// These tests exercise the DB-backed smart-matcher write path against a
// live Postgres connected as the non-superuser kapp_app role, so RLS is
// enforced exactly as in production. They are gated behind the
// `integration` build tag and skip when KAPP_TEST_DB_URL is unset, the
// same convention as internal/integrationtest.
//
//	KAPP_TEST_DB_URL=... go test -tags=integration ./internal/ledger/
package ledger

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

func matcherPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("KAPP_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("KAPP_TEST_DB_URL not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := platform.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return pool
}

// seedMatcherFixture creates a tenant, a bank account (GL code 1000), the
// expense account (6000) the candidate entry posts to, and returns the
// store plus ids. Each call uses a unique tenant so tests are repeatable.
func seedMatcherFixture(t *testing.T, pool *pgxpool.Pool) (store *PGStore, tenantID, bankAccountID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenants := tenant.NewPGStore(pool)
	tn, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "mx-" + uuid.NewString()[:8], Name: "Matcher Test", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	store = NewPGStore(pool, events.NewPGPublisher(pool), audit.NewPGLogger(pool))

	for _, a := range []Account{
		{TenantID: tn.ID, Code: "1000", Name: "Operating Cash", Type: AccountTypeAsset, Active: true},
		{TenantID: tn.ID, Code: "6000", Name: "Subscriptions", Type: AccountTypeExpense, Active: true},
	} {
		if _, err := store.CreateAccount(ctx, a); err != nil {
			t.Fatalf("create account %s: %v", a.Code, err)
		}
	}

	acct, err := store.UpsertBankAccount(ctx, BankAccount{
		TenantID: tn.ID, Name: "Operating", AccountCode: "1000", Currency: "USD", Active: true,
	})
	if err != nil {
		t.Fatalf("create bank account: %v", err)
	}
	return store, tn.ID, acct.ID
}

// postCandidateEntry posts a balanced JE (debit expense / credit bank)
// that the matcher should surface as a candidate for a -amount feed line.
func postCandidateEntry(t *testing.T, store *PGStore, tenantID uuid.UUID, when time.Time, amount, memo string) uuid.UUID {
	t.Helper()
	amt := decimal.RequireFromString(amount)
	entry, err := store.PostJournalEntry(context.Background(), JournalEntry{
		TenantID:  tenantID,
		PostedAt:  when,
		Memo:      memo,
		CreatedBy: uuid.New(),
		Lines: []JournalLine{
			{AccountCode: "6000", Debit: amt, Currency: "USD"},
			{AccountCode: "1000", Credit: amt, Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("PostJournalEntry: %v", err)
	}
	return entry.ID
}

// TestSmartMatcherSuggestAcceptLearn drives the full write path:
// suggest -> persist -> list -> accept -> reconcile -> learn, then proves
// the learned counterparty boosts the next suggestion and that reject
// closes an open suggestion.
func TestSmartMatcherSuggestAcceptLearn(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	entryID := postCandidateEntry(t, store, tenantID, vd, "100.00", "ACME CORP monthly")

	// Ingest the matching statement line (money out, so negative).
	inserted, err := store.SyncBankTransactions(ctx, tenantID, acctID, []BankTransaction{{
		BankAccountID: acctID, ValueDate: vd, Description: "ACME CORP",
		Amount: decimal.RequireFromString("-100.00"), Currency: "USD", ExternalRef: "ext-1",
	}})
	if err != nil || len(inserted) != 1 {
		t.Fatalf("SyncBankTransactions = %v, %v; want 1 line", inserted, err)
	}
	txnID := inserted[0].ID

	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })

	// SuggestMatches must persist at least one suggestion (exact amount +
	// same-day) and point at the candidate entry.
	sugs, err := matcher.SuggestMatches(ctx, tenantID, txnID, MatchOptions{})
	if err != nil {
		t.Fatalf("SuggestMatches: %v", err)
	}
	if len(sugs) == 0 {
		t.Fatal("expected at least one suggestion for an exact same-day match")
	}
	var top *Suggestion
	for i := range sugs {
		if sugs[i].JournalEntryID == entryID {
			top = &sugs[i]
		}
	}
	if top == nil {
		t.Fatalf("no suggestion referenced the candidate entry %s", entryID)
	}
	if top.Confidence < DefaultMinConfidence {
		t.Fatalf("confidence %.2f below persistence threshold", top.Confidence)
	}

	// ListSuggestions surfaces the open suggestion for the account.
	listed, err := matcher.ListSuggestions(ctx, tenantID, acctID)
	if err != nil {
		t.Fatalf("ListSuggestions: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("ListSuggestions returned none")
	}

	// Accept reconciles the txn, marks it matched, and feeds the learner.
	actor := uuid.New()
	accepted, err := matcher.AcceptSuggestion(ctx, tenantID, top.ID, actor)
	if err != nil {
		t.Fatalf("AcceptSuggestion: %v", err)
	}
	if accepted.Status != SuggestionAccepted {
		t.Fatalf("status = %q; want accepted", accepted.Status)
	}

	// The transaction is now matched against the entry.
	var status string
	var matched *uuid.UUID
	if err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, matched_entry_id FROM bank_transactions WHERE tenant_id=$1 AND id=$2`,
			tenantID, txnID).Scan(&status, &matched)
	}); err != nil {
		t.Fatalf("read txn: %v", err)
	}
	if status != BankTxnMatched || matched == nil || *matched != entryID {
		t.Fatalf("txn not reconciled: status=%q matched=%v", status, matched)
	}

	// Accepting again must fail — the suggestion is no longer open.
	if _, err := matcher.AcceptSuggestion(ctx, tenantID, top.ID, actor); err == nil {
		t.Fatal("re-accepting a decided suggestion should error")
	}

	// A second identical-counterparty line should now score higher thanks
	// to the learned match (+0.20). Post a fresh candidate and feed line.
	entry2 := postCandidateEntry(t, store, tenantID, vd, "100.00", "ACME CORP monthly")
	in2, err := store.SyncBankTransactions(ctx, tenantID, acctID, []BankTransaction{{
		BankAccountID: acctID, ValueDate: vd, Description: "ACME CORP",
		Amount: decimal.RequireFromString("-100.00"), Currency: "USD", ExternalRef: "ext-2",
	}})
	if err != nil || len(in2) != 1 {
		t.Fatalf("second sync = %v, %v", in2, err)
	}
	sugs2, err := matcher.SuggestMatches(ctx, tenantID, in2[0].ID, MatchOptions{})
	if err != nil {
		t.Fatalf("SuggestMatches (2nd): %v", err)
	}
	var learnedHit bool
	var second *Suggestion
	for i := range sugs2 {
		if sugs2[i].JournalEntryID == entry2 {
			second = &sugs2[i]
		}
		if sugs2[i].Confidence >= top.Confidence {
			learnedHit = true
		}
	}
	if !learnedHit {
		t.Fatalf("learned counterparty did not raise confidence above %.2f", top.Confidence)
	}
	if second == nil {
		t.Fatal("second pass produced no suggestion for the new entry")
	}

	// Reject closes the open suggestion.
	if err := matcher.RejectSuggestion(ctx, tenantID, second.ID, actor); err != nil {
		t.Fatalf("RejectSuggestion: %v", err)
	}
	if err := matcher.RejectSuggestion(ctx, tenantID, second.ID, actor); err == nil {
		t.Fatal("re-rejecting a closed suggestion should error")
	}
}
