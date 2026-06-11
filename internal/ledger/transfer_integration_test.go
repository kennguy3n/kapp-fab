//go:build integration
// +build integration

// These tests exercise the DB-backed inter-account transfer pairing and
// duplicate-flagging paths added for bank-feed reconciliation, against a
// live Postgres connected as the non-superuser kapp_app role so RLS is
// enforced exactly as in production. Gated behind the `integration` build
// tag and skipped when KAPP_TEST_DB_URL is unset, matching the rest of the
// ledger integration suite:
//
//	KAPP_TEST_DB_URL=... go test -tags=integration ./internal/ledger/
package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// secondBankAccount adds a second asset GL account (1010) and a bank
// account bound to it, so transfer pairing across two accounts of the same
// tenant can be exercised.
func secondBankAccount(t *testing.T, store *PGStore, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateAccount(ctx, Account{
		TenantID: tenantID, Code: "1010", Name: "Savings Cash", Type: AccountTypeAsset, Active: true,
	}); err != nil {
		t.Fatalf("create account 1010: %v", err)
	}
	acct, err := store.UpsertBankAccount(ctx, BankAccount{
		TenantID: tenantID, Name: "Savings", AccountCode: "1010", Currency: "USD", Active: true,
	})
	if err != nil {
		t.Fatalf("create second bank account: %v", err)
	}
	return acct.ID
}

// readTxn returns the status and duplicate_of of a bank line under RLS.
func readTxn(t *testing.T, store *PGStore, tenantID, id uuid.UUID) (status string, dupOf *uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := dbutil.WithTenantTx(ctx, store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, duplicate_of FROM bank_transactions WHERE tenant_id=$1 AND id=$2`,
			tenantID, id).Scan(&status, &dupOf)
	}); err != nil {
		t.Fatalf("read txn %s: %v", id, err)
	}
	return status, dupOf
}

func TestDetectTransferPairsOppositeLegs(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctA := seedMatcherFixture(t, pool)
	acctB := secondBankAccount(t, store, tenantID)

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	// Money out of A, money into B, same magnitude/currency, same day.
	outLeg, err := store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd, Description: "TRANSFER TO SAVINGS",
		Amount: decimal.RequireFromString("-500.00"), Currency: "USD", ExternalRef: "a-out",
	}})
	if err != nil || len(outLeg) != 1 {
		t.Fatalf("ingest out leg = %v, %v", outLeg, err)
	}
	inLeg, err := store.SyncBankTransactions(ctx, tenantID, acctB, []BankTransaction{{
		BankAccountID: acctB, ValueDate: vd, Description: "TRANSFER FROM CURRENT",
		Amount: decimal.RequireFromString("500.00"), Currency: "USD", ExternalRef: "b-in",
	}})
	if err != nil || len(inLeg) != 1 {
		t.Fatalf("ingest in leg = %v, %v", inLeg, err)
	}

	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })

	pair, err := matcher.DetectTransfer(ctx, tenantID, inLeg[0].ID)
	if err != nil {
		t.Fatalf("DetectTransfer: %v", err)
	}
	if pair == nil {
		t.Fatal("expected a transfer pair")
	}
	if pair.DebitTxnID != outLeg[0].ID || pair.CreditTxnID != inLeg[0].ID {
		t.Fatalf("pair legs = debit %s credit %s; want debit %s credit %s",
			pair.DebitTxnID, pair.CreditTxnID, outLeg[0].ID, inLeg[0].ID)
	}
	if !pair.Amount.Equal(decimal.RequireFromString("500.00")) {
		t.Fatalf("pair amount = %s; want 500.00", pair.Amount)
	}
	if pair.Confidence < 0.9 {
		t.Fatalf("confidence %.2f; want >= 0.90 for same-day cue", pair.Confidence)
	}

	// Both legs are now status='transfer'.
	for _, id := range []uuid.UUID{outLeg[0].ID, inLeg[0].ID} {
		if s, _ := readTxn(t, store, tenantID, id); s != BankTxnTransfer {
			t.Fatalf("leg %s status = %q; want transfer", id, s)
		}
	}

	// Idempotent: re-running on either leg creates no new pair.
	if p, err := matcher.DetectTransfer(ctx, tenantID, outLeg[0].ID); err != nil || p != nil {
		t.Fatalf("re-detect on debit leg = %v, %v; want nil, nil", p, err)
	}
	if p, err := matcher.DetectTransfer(ctx, tenantID, inLeg[0].ID); err != nil || p != nil {
		t.Fatalf("re-detect on credit leg = %v, %v; want nil, nil", p, err)
	}
}

func TestDetectTransferNoCounterLeg(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctA := seedMatcherFixture(t, pool)
	secondBankAccount(t, store, tenantID)

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	line, err := store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd, Description: "ACME CORP",
		Amount: decimal.RequireFromString("-75.00"), Currency: "USD", ExternalRef: "solo",
	}})
	if err != nil || len(line) != 1 {
		t.Fatalf("ingest = %v, %v", line, err)
	}
	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	if p, err := matcher.DetectTransfer(ctx, tenantID, line[0].ID); err != nil || p != nil {
		t.Fatalf("DetectTransfer = %v, %v; want nil, nil", p, err)
	}
	if s, _ := readTxn(t, store, tenantID, line[0].ID); s != BankTxnUnreconciled {
		t.Fatalf("status = %q; want unreconciled (untouched)", s)
	}
}

// Same magnitude but same sign (two debits) must NOT pair: a transfer is
// an opposite-signed pair.
func TestDetectTransferIgnoresSameSign(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctA := seedMatcherFixture(t, pool)
	acctB := secondBankAccount(t, store, tenantID)

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	a, _ := store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd, Description: "fee", Amount: decimal.RequireFromString("-30.00"),
		Currency: "USD", ExternalRef: "a1",
	}})
	store.SyncBankTransactions(ctx, tenantID, acctB, []BankTransaction{{
		BankAccountID: acctB, ValueDate: vd, Description: "fee", Amount: decimal.RequireFromString("-30.00"),
		Currency: "USD", ExternalRef: "b1",
	}})
	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	if p, err := matcher.DetectTransfer(ctx, tenantID, a[0].ID); err != nil || p != nil {
		t.Fatalf("DetectTransfer = %v, %v; want nil, nil (same sign)", p, err)
	}
}

func TestDetectDuplicateFlagsLaterLine(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctA := seedMatcherFixture(t, pool)

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	first, err := store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd, Description: "TFL TRAVEL CH",
		Amount: decimal.RequireFromString("-12.50"), Currency: "USD", ExternalRef: "feed-a-1",
	}})
	if err != nil || len(first) != 1 {
		t.Fatalf("ingest first = %v, %v", first, err)
	}
	// Same amount + similar description a day later via a different feed.
	second, err := store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd.Add(24 * time.Hour), Description: "TFL TRAVEL CHARGE",
		Amount: decimal.RequireFromString("-12.50"), Currency: "USD", ExternalRef: "feed-b-1",
	}})
	if err != nil || len(second) != 1 {
		t.Fatalf("ingest second = %v, %v", second, err)
	}

	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	canonical, err := matcher.DetectDuplicate(ctx, tenantID, second[0].ID)
	if err != nil {
		t.Fatalf("DetectDuplicate: %v", err)
	}
	if canonical == nil || *canonical != first[0].ID {
		t.Fatalf("canonical = %v; want %s", canonical, first[0].ID)
	}
	// The later line is flagged; the earlier line is untouched.
	if s, dup := readTxn(t, store, tenantID, second[0].ID); dup == nil || *dup != first[0].ID {
		t.Fatalf("second.duplicate_of = %v (status %q); want %s", dup, s, first[0].ID)
	}
	if _, dup := readTxn(t, store, tenantID, first[0].ID); dup != nil {
		t.Fatalf("first.duplicate_of = %v; want nil (canonical line)", dup)
	}

	// Idempotent: a second call returns the same canonical pointer.
	again, err := matcher.DetectDuplicate(ctx, tenantID, second[0].ID)
	if err != nil || again == nil || *again != first[0].ID {
		t.Fatalf("re-detect = %v, %v; want %s", again, err, first[0].ID)
	}
}

// A same-amount line OUTSIDE the duplicate window is not flagged.
func TestDetectDuplicateRespectsWindow(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctA := seedMatcherFixture(t, pool)

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd, Description: "GYM MEMBERSHIP",
		Amount: decimal.RequireFromString("-40.00"), Currency: "USD", ExternalRef: "m-jan",
	}})
	later, err := store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd.Add(30 * 24 * time.Hour), Description: "GYM MEMBERSHIP",
		Amount: decimal.RequireFromString("-40.00"), Currency: "USD", ExternalRef: "m-feb",
	}})
	if err != nil || len(later) != 1 {
		t.Fatalf("ingest later = %v, %v", later, err)
	}
	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	dup, err := matcher.DetectDuplicate(ctx, tenantID, later[0].ID)
	if err != nil || dup != nil {
		t.Fatalf("DetectDuplicate = %v, %v; want nil, nil (recurring charge, not dup)", dup, err)
	}
}

// A same-amount line with a dissimilar description is not flagged: two
// distinct charges that happen to share an amount must stay separate.
func TestDetectDuplicateRequiresSimilarDescription(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctA := seedMatcherFixture(t, pool)

	vd := time.Now().UTC().Truncate(24 * time.Hour)
	store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd, Description: "AMAZON MARKETPLACE",
		Amount: decimal.RequireFromString("-25.00"), Currency: "USD", ExternalRef: "x1",
	}})
	other, err := store.SyncBankTransactions(ctx, tenantID, acctA, []BankTransaction{{
		BankAccountID: acctA, ValueDate: vd, Description: "CORNER SHOP GROCERIES",
		Amount: decimal.RequireFromString("-25.00"), Currency: "USD", ExternalRef: "x2",
	}})
	if err != nil || len(other) != 1 {
		t.Fatalf("ingest = %v, %v", other, err)
	}
	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	dup, err := matcher.DetectDuplicate(ctx, tenantID, other[0].ID)
	if err != nil || dup != nil {
		t.Fatalf("DetectDuplicate = %v, %v; want nil, nil (different payee)", dup, err)
	}
}
