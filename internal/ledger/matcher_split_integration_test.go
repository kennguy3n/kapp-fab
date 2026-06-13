//go:build integration
// +build integration

// These tests exercise the DB-backed split-reconciliation write path
// (SmartMatcher.AcceptSplit) against a live Postgres connected as the
// non-superuser kapp_app role, so RLS is enforced exactly as in
// production. Gated behind the `integration` build tag; they skip when
// KAPP_TEST_DB_URL is unset — the same convention as the sibling
// matcher_integration_test.go.
//
//	KAPP_TEST_DB_URL=... go test -tags=integration ./internal/ledger/
package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// seedSuggestion inserts an open (status='suggested') bank_match_suggestions
// row directly so a split test can assert the chosen leg is accepted and any
// other open suggestion for the line is collapsed to rejected.
func seedSuggestion(t *testing.T, pool *pgxpool.Pool, tenantID, txnID, entryID uuid.UUID, now time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := dbutil.WithTenantTx(context.Background(), pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bank_match_suggestions
			    (tenant_id, id, transaction_id, journal_entry_id, confidence, match_reason, status, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)`,
			tenantID, id, txnID, entryID, decimal.RequireFromString("0.900"), "seed", SuggestionSuggested, now)
		return err
	}); err != nil {
		t.Fatalf("seed suggestion: %v", err)
	}
	return id
}

// splitEntry posts a balanced JE to the given expense account for amount,
// returning the entry id. Mirrors postCandidateEntry but lets each leg use
// a distinct category account so a split spans genuinely different entries.
func splitEntry(t *testing.T, store *PGStore, tenantID uuid.UUID, when time.Time, accountCode, amount, memo string) uuid.UUID {
	t.Helper()
	amt := decimal.RequireFromString(amount)
	entry, err := store.PostJournalEntry(context.Background(), JournalEntry{
		TenantID:  tenantID,
		PostedAt:  when,
		Memo:      memo,
		CreatedBy: uuid.New(),
		Lines: []JournalLine{
			{AccountCode: accountCode, Debit: amt, Currency: "USD"},
			{AccountCode: "1000", Credit: amt, Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("PostJournalEntry: %v", err)
	}
	return entry.ID
}

// allocationsFor reads back the persisted split legs for a transaction.
func allocationsFor(t *testing.T, pool *pgxpool.Pool, tenantID, txnID uuid.UUID) map[uuid.UUID]decimal.Decimal {
	t.Helper()
	out := map[uuid.UUID]decimal.Decimal{}
	if err := dbutil.WithTenantTx(context.Background(), pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT journal_entry_id, amount FROM bank_transaction_allocations
			  WHERE tenant_id=$1 AND transaction_id=$2`, tenantID, txnID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var amt decimal.Decimal
			if err := rows.Scan(&id, &amt); err != nil {
				return err
			}
			out[id] = amt
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read allocations: %v", err)
	}
	return out
}

func TestAcceptSplitReconcilesAcrossEntries(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)

	// A second expense account so the two legs post to distinct entries.
	if _, err := store.CreateAccount(ctx, Account{
		TenantID: tenantID, Code: "6100", Name: "Office", Type: AccountTypeExpense, Active: true,
	}); err != nil {
		t.Fatalf("create account 6100: %v", err)
	}

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	entryA := splitEntry(t, store, tenantID, vd, "6000", "60.00", "ACME part A")
	entryB := splitEntry(t, store, tenantID, vd, "6100", "40.00", "ACME part B")

	inserted, err := store.SyncBankTransactions(ctx, tenantID, acctID, []BankTransaction{{
		BankAccountID: acctID, ValueDate: vd, Description: "ACME CORP",
		Amount: decimal.RequireFromString("-100.00"), Currency: "USD", ExternalRef: "split-1",
	}})
	if err != nil || len(inserted) != 1 {
		t.Fatalf("SyncBankTransactions = %v, %v; want 1 line", inserted, err)
	}
	txnID := inserted[0].ID

	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })

	// Seed an open suggestion for entryA and a third, unrelated open
	// suggestion so we can prove the chosen leg is accepted and the
	// remaining open suggestion is collapsed to rejected.
	otherEntry := splitEntry(t, store, tenantID, vd, "6000", "100.00", "stale candidate")
	sugA := seedSuggestion(t, pool, tenantID, txnID, entryA, vd)
	sugOther := seedSuggestion(t, pool, tenantID, txnID, otherEntry, vd)

	actor := uuid.New()
	out, err := matcher.AcceptSplit(ctx, tenantID, txnID, []SplitLeg{
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-60.00"), SuggestionID: sugA},
		{JournalEntryID: entryB, Amount: decimal.RequireFromString("-40.00")},
	}, actor)
	if err != nil {
		t.Fatalf("AcceptSplit: %v", err)
	}
	if out.Status != BankTxnMatched || out.MatchedEntryID != nil {
		t.Fatalf("split line state: status=%q matched=%v; want matched + nil entry", out.Status, out.MatchedEntryID)
	}

	// Allocations persisted with exact partial amounts.
	allocs := allocationsFor(t, pool, tenantID, txnID)
	if len(allocs) != 2 {
		t.Fatalf("want 2 allocation rows, got %d", len(allocs))
	}
	if got := allocs[entryA]; !got.Equal(decimal.RequireFromString("-60.00")) {
		t.Fatalf("entryA allocation = %s; want -60.00", got)
	}
	if got := allocs[entryB]; !got.Equal(decimal.RequireFromString("-40.00")) {
		t.Fatalf("entryB allocation = %s; want -40.00", got)
	}

	// The line is matched with NULL matched_entry_id (allocations are the
	// source of truth for a split).
	var status string
	var matched *uuid.UUID
	if err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, matched_entry_id FROM bank_transactions WHERE tenant_id=$1 AND id=$2`,
			tenantID, txnID).Scan(&status, &matched)
	}); err != nil {
		t.Fatalf("read txn: %v", err)
	}
	if status != BankTxnMatched || matched != nil {
		t.Fatalf("txn not split-reconciled: status=%q matched=%v", status, matched)
	}

	// Chosen suggestion accepted; the remaining open one collapsed to rejected.
	if st := suggestionStatus(t, store, tenantID, sugA); st != SuggestionAccepted {
		t.Fatalf("sugA status = %q; want accepted", st)
	}
	if st := suggestionStatus(t, store, tenantID, sugOther); st != SuggestionRejected {
		t.Fatalf("sugOther status = %q; want rejected", st)
	}

	// Re-splitting a reconciled line is a conflict.
	if _, err := matcher.AcceptSplit(ctx, tenantID, txnID, []SplitLeg{
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-60.00")},
		{JournalEntryID: entryB, Amount: decimal.RequireFromString("-40.00")},
	}, actor); err == nil {
		t.Fatal("re-splitting a matched line should error")
	}
}

func TestAcceptSplitRejectsInvalid(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)
	if _, err := store.CreateAccount(ctx, Account{
		TenantID: tenantID, Code: "6100", Name: "Office", Type: AccountTypeExpense, Active: true,
	}); err != nil {
		t.Fatalf("create account 6100: %v", err)
	}
	vd := time.Now().UTC().Truncate(24 * time.Hour)
	entryA := splitEntry(t, store, tenantID, vd, "6000", "60.00", "A")
	entryB := splitEntry(t, store, tenantID, vd, "6100", "40.00", "B")
	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	actor := uuid.New()

	mk := func(ref string) uuid.UUID {
		in, err := store.SyncBankTransactions(ctx, tenantID, acctID, []BankTransaction{{
			BankAccountID: acctID, ValueDate: vd, Description: "ACME",
			Amount: decimal.RequireFromString("-100.00"), Currency: "USD", ExternalRef: ref,
		}})
		if err != nil || len(in) != 1 {
			t.Fatalf("sync %s: %v %v", ref, in, err)
		}
		return in[0].ID
	}

	// Unbalanced: legs sum to -90, line is -100.
	if _, err := matcher.AcceptSplit(ctx, tenantID, mk("inv-1"), []SplitLeg{
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-60.00")},
		{JournalEntryID: entryB, Amount: decimal.RequireFromString("-30.00")},
	}, actor); err == nil {
		t.Fatal("unbalanced split should be rejected")
	}

	// Duplicate entry.
	if _, err := matcher.AcceptSplit(ctx, tenantID, mk("inv-2"), []SplitLeg{
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-60.00")},
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-40.00")},
	}, actor); err == nil {
		t.Fatal("duplicate entry split should be rejected")
	}

	// Empty legs.
	if _, err := matcher.AcceptSplit(ctx, tenantID, mk("inv-3"), nil, actor); err == nil {
		t.Fatal("empty split should be rejected")
	}

	// Single leg: a split must span >=2 entries (one entry is a 1:1 accept).
	if _, err := matcher.AcceptSplit(ctx, tenantID, mk("inv-5"), []SplitLeg{
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-100.00")},
	}, actor); err == nil {
		t.Fatal("single-leg split should be rejected")
	}

	// Non-existent / cross-tenant entry: a random uuid is not visible
	// under this tenant's RLS scope, so it reads as not-found.
	if _, err := matcher.AcceptSplit(ctx, tenantID, mk("inv-4"), []SplitLeg{
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-60.00")},
		{JournalEntryID: uuid.New(), Amount: decimal.RequireFromString("-40.00")},
	}, actor); err == nil {
		t.Fatal("split citing an unknown entry should be rejected")
	}
}

// TestAcceptSplitDoesNotTouchForeignSuggestion proves that citing a
// suggestion_id belonging to a *different* bank line cannot flip that
// foreign suggestion to accepted. Without the transaction_id scope on the
// per-leg accept, splitting line 1 while citing line 2's suggestion would
// strand line 2 unreconciled with an accepted suggestion that has vanished
// from its review queue — a cross-line corruption.
func TestAcceptSplitDoesNotTouchForeignSuggestion(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)
	if _, err := store.CreateAccount(ctx, Account{
		TenantID: tenantID, Code: "6100", Name: "Office", Type: AccountTypeExpense, Active: true,
	}); err != nil {
		t.Fatalf("create account 6100: %v", err)
	}
	vd := time.Now().UTC().Truncate(24 * time.Hour)
	entryA := splitEntry(t, store, tenantID, vd, "6000", "60.00", "A")
	entryB := splitEntry(t, store, tenantID, vd, "6100", "40.00", "B")
	foreignEntry := splitEntry(t, store, tenantID, vd, "6000", "100.00", "foreign")
	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	actor := uuid.New()

	mk := func(ref string) uuid.UUID {
		in, err := store.SyncBankTransactions(ctx, tenantID, acctID, []BankTransaction{{
			BankAccountID: acctID, ValueDate: vd, Description: "ACME",
			Amount: decimal.RequireFromString("-100.00"), Currency: "USD", ExternalRef: ref,
		}})
		if err != nil || len(in) != 1 {
			t.Fatalf("sync %s: %v %v", ref, in, err)
		}
		return in[0].ID
	}

	txn1 := mk("foreign-1")
	txn2 := mk("foreign-2")
	// An open suggestion belonging to txn2 (the line we are NOT splitting).
	foreignSug := seedSuggestion(t, pool, tenantID, txn2, foreignEntry, vd)

	// Split txn1, maliciously/accidentally citing txn2's suggestion on a leg.
	if _, err := matcher.AcceptSplit(ctx, tenantID, txn1, []SplitLeg{
		{JournalEntryID: entryA, Amount: decimal.RequireFromString("-60.00"), SuggestionID: foreignSug},
		{JournalEntryID: entryB, Amount: decimal.RequireFromString("-40.00")},
	}, actor); err != nil {
		t.Fatalf("AcceptSplit txn1: %v", err)
	}

	// txn2's suggestion must be untouched (still open), and txn2 itself
	// must remain unreconciled.
	if st := suggestionStatus(t, store, tenantID, foreignSug); st != SuggestionSuggested {
		t.Fatalf("foreign suggestion status = %q; want still suggested (split must not touch another line's suggestion)", st)
	}
	var txn2Status string
	if err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status FROM bank_transactions WHERE tenant_id=$1 AND id=$2`,
			tenantID, txn2).Scan(&txn2Status)
	}); err != nil {
		t.Fatalf("read txn2: %v", err)
	}
	if txn2Status != BankTxnUnreconciled {
		t.Fatalf("txn2 status = %q; want unreconciled", txn2Status)
	}
}
