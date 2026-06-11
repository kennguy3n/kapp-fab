//go:build integration
// +build integration

// DB-backed tests for the Plaid modified/removed mutation+void path.
// They run as the non-superuser kapp_app role so RLS is enforced exactly
// as in production, are gated behind the `integration` build tag, and skip
// when KAPP_TEST_DB_URL is unset.
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

// ingestLine inserts a single statement line and returns its row id.
func ingestLine(t *testing.T, store *PGStore, tenantID, acctID uuid.UUID, vd time.Time, desc, amount, ref string) uuid.UUID {
	t.Helper()
	inserted, err := store.SyncBankTransactions(context.Background(), tenantID, acctID, []BankTransaction{{
		BankAccountID: acctID, ValueDate: vd, Description: desc,
		Amount: decimal.RequireFromString(amount), Currency: "USD", ExternalRef: ref,
	}})
	if err != nil || len(inserted) != 1 {
		t.Fatalf("SyncBankTransactions = %v, %v; want 1 line", inserted, err)
	}
	return inserted[0].ID
}

// reconcileViaAccept drives suggest -> accept so the line ends up matched
// against entryID, and returns the accepted suggestion id.
func reconcileViaAccept(t *testing.T, store *PGStore, tenantID, txnID, entryID uuid.UUID, vd time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	matcher := NewSmartMatcher(store).WithClock(func() time.Time { return vd })
	sugs, err := matcher.SuggestMatches(ctx, tenantID, txnID, MatchOptions{})
	if err != nil {
		t.Fatalf("SuggestMatches: %v", err)
	}
	var top *Suggestion
	for i := range sugs {
		if sugs[i].JournalEntryID == entryID {
			top = &sugs[i]
		}
	}
	if top == nil {
		t.Fatalf("no suggestion referenced candidate entry %s", entryID)
	}
	if _, err := matcher.AcceptSuggestion(ctx, tenantID, top.ID, uuid.New()); err != nil {
		t.Fatalf("AcceptSuggestion: %v", err)
	}
	return top.ID
}

func readBankTxn(t *testing.T, store *PGStore, tenantID, txnID uuid.UUID) (status string, matched *uuid.UUID, amount decimal.Decimal, desc string) {
	t.Helper()
	if err := dbutil.WithTenantTx(context.Background(), store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, matched_entry_id, amount, COALESCE(description,'') FROM bank_transactions WHERE tenant_id=$1 AND id=$2`,
			tenantID, txnID).Scan(&status, &matched, &amount, &desc)
	}); err != nil {
		t.Fatalf("read bank txn: %v", err)
	}
	return status, matched, amount, desc
}

func suggestionStatus(t *testing.T, store *PGStore, tenantID, sugID uuid.UUID) string {
	t.Helper()
	var status string
	if err := dbutil.WithTenantTx(context.Background(), store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status FROM bank_match_suggestions WHERE tenant_id=$1 AND id=$2`,
			tenantID, sugID).Scan(&status)
	}); err != nil {
		t.Fatalf("read suggestion: %v", err)
	}
	return status
}

func auditCount(t *testing.T, store *PGStore, tenantID, targetID uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := dbutil.WithTenantTx(context.Background(), store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND target_id=$2 AND action=$3`,
			tenantID, targetID, action).Scan(&n)
	}); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func TestApplyMutationsModifyUnreconciled(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)
	vd := time.Now().UTC().Truncate(24 * time.Hour)
	txnID := ingestLine(t, store, tenantID, acctID, vd, "ACME", "-100.00", "ext-1")

	res, err := store.ApplyBankTransactionMutations(ctx, tenantID, acctID,
		[]BankTxnMutation{{ExternalRef: "ext-1", ValueDate: vd, Description: "ACME CORRECTED", Amount: decimal.RequireFromString("-90.00"), Currency: "USD"}},
		nil)
	if err != nil {
		t.Fatalf("ApplyBankTransactionMutations: %v", err)
	}
	if res.Updated != 1 || res.Unwound != 0 || res.Voided != 0 {
		t.Fatalf("res = %+v; want updated=1 only", res)
	}
	if len(res.Rematch) != 1 || res.Rematch[0].ExternalRef != "ext-1" {
		t.Fatalf("rematch = %+v; want one for ext-1", res.Rematch)
	}
	status, matched, amount, desc := readBankTxn(t, store, tenantID, txnID)
	if status != BankTxnUnreconciled || matched != nil {
		t.Fatalf("status=%q matched=%v; want unreconciled/nil", status, matched)
	}
	if !amount.Equal(decimal.RequireFromString("-90.00")) || desc != "ACME CORRECTED" {
		t.Fatalf("row not updated: amount=%s desc=%q", amount, desc)
	}
	if auditCount(t, store, tenantID, txnID, "finance.bank_feed.transaction.modified") != 1 {
		t.Fatal("expected one modified audit entry")
	}
}

func TestApplyMutationsModifyMatchedUnwinds(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)
	vd := time.Now().UTC().Truncate(24 * time.Hour)
	entryID := postCandidateEntry(t, store, tenantID, vd, "100.00", "ACME CORP monthly")
	txnID := ingestLine(t, store, tenantID, acctID, vd, "ACME CORP", "-100.00", "ext-1")
	sugID := reconcileViaAccept(t, store, tenantID, txnID, entryID, vd)

	// Sanity: line is matched before the mutation.
	if s, m, _, _ := readBankTxn(t, store, tenantID, txnID); s != BankTxnMatched || m == nil || *m != entryID {
		t.Fatalf("precondition: line not matched (status=%q matched=%v)", s, m)
	}

	res, err := store.ApplyBankTransactionMutations(ctx, tenantID, acctID,
		[]BankTxnMutation{{ExternalRef: "ext-1", ValueDate: vd, Description: "ACME CORP", Amount: decimal.RequireFromString("-50.00"), Currency: "USD"}},
		nil)
	if err != nil {
		t.Fatalf("ApplyBankTransactionMutations: %v", err)
	}
	if res.Updated != 1 || res.Unwound != 1 {
		t.Fatalf("res = %+v; want updated=1 unwound=1", res)
	}
	status, matched, amount, _ := readBankTxn(t, store, tenantID, txnID)
	if status != BankTxnUnreconciled || matched != nil {
		t.Fatalf("unwind failed: status=%q matched=%v", status, matched)
	}
	if !amount.Equal(decimal.RequireFromString("-50.00")) {
		t.Fatalf("amount = %s; want -50.00", amount)
	}
	if st := suggestionStatus(t, store, tenantID, sugID); st != SuggestionRejected {
		t.Fatalf("accepted suggestion status = %q; want rejected after unwind", st)
	}
	// The journal entry itself is immutable and must survive the unwind.
	var jeCount int
	if err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE tenant_id=$1 AND id=$2`, tenantID, entryID).Scan(&jeCount)
	}); err != nil {
		t.Fatalf("read JE: %v", err)
	}
	if jeCount != 1 {
		t.Fatalf("journal entry should be untouched by unwind; count=%d", jeCount)
	}
}

func TestApplyMutationsRemoveMatchedUnwinds(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)
	vd := time.Now().UTC().Truncate(24 * time.Hour)
	entryID := postCandidateEntry(t, store, tenantID, vd, "100.00", "ACME CORP monthly")
	txnID := ingestLine(t, store, tenantID, acctID, vd, "ACME CORP", "-100.00", "ext-1")
	sugID := reconcileViaAccept(t, store, tenantID, txnID, entryID, vd)

	res, err := store.ApplyBankTransactionMutations(ctx, tenantID, acctID, nil, []string{"ext-1"})
	if err != nil {
		t.Fatalf("ApplyBankTransactionMutations: %v", err)
	}
	if res.Voided != 1 || res.Unwound != 1 {
		t.Fatalf("res = %+v; want voided=1 unwound=1", res)
	}
	status, matched, _, _ := readBankTxn(t, store, tenantID, txnID)
	if status != BankTxnVoided || matched != nil {
		t.Fatalf("void failed: status=%q matched=%v", status, matched)
	}
	if st := suggestionStatus(t, store, tenantID, sugID); st != SuggestionRejected {
		t.Fatalf("accepted suggestion status = %q; want rejected after void-unwind", st)
	}
	if auditCount(t, store, tenantID, txnID, "finance.bank_feed.transaction.voided") != 1 {
		t.Fatal("expected one voided audit entry")
	}
}

func TestApplyMutationsIdempotent(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)
	vd := time.Now().UTC().Truncate(24 * time.Hour)
	txnID := ingestLine(t, store, tenantID, acctID, vd, "ACME", "-100.00", "ext-1")

	// Re-send identical values: no-op, no audit churn.
	res, err := store.ApplyBankTransactionMutations(ctx, tenantID, acctID,
		[]BankTxnMutation{{ExternalRef: "ext-1", ValueDate: vd, Description: "ACME", Amount: decimal.RequireFromString("-100.00"), Currency: "USD"}},
		nil)
	if err != nil {
		t.Fatalf("modify (no-op): %v", err)
	}
	if res.Updated != 0 {
		t.Fatalf("unchanged modify should be a no-op; res=%+v", res)
	}
	if auditCount(t, store, tenantID, txnID, "finance.bank_feed.transaction.modified") != 0 {
		t.Fatal("unchanged modify must not write an audit entry")
	}

	// First removal voids; second removal is a no-op.
	if res, err = store.ApplyBankTransactionMutations(ctx, tenantID, acctID, nil, []string{"ext-1"}); err != nil || res.Voided != 1 {
		t.Fatalf("first remove: res=%+v err=%v; want voided=1", res, err)
	}
	if res, err = store.ApplyBankTransactionMutations(ctx, tenantID, acctID, nil, []string{"ext-1"}); err != nil || res.Voided != 0 {
		t.Fatalf("re-remove should be no-op: res=%+v err=%v", res, err)
	}
	// A modify landing on a voided line is ignored (retraction wins).
	if res, err = store.ApplyBankTransactionMutations(ctx, tenantID, acctID,
		[]BankTxnMutation{{ExternalRef: "ext-1", ValueDate: vd, Description: "ZZZ", Amount: decimal.RequireFromString("-1.00"), Currency: "USD"}},
		nil); err != nil || res.Updated != 0 {
		t.Fatalf("modify on voided line should be no-op: res=%+v err=%v", res, err)
	}
	if s, _, _, _ := readBankTxn(t, store, tenantID, txnID); s != BankTxnVoided {
		t.Fatalf("line status=%q; want voided to persist", s)
	}
}

func TestApplyMutationsModifyMissingReported(t *testing.T) {
	pool := matcherPool(t)
	ctx := context.Background()
	store, tenantID, acctID := seedMatcherFixture(t, pool)
	vd := time.Now().UTC().Truncate(24 * time.Hour)

	res, err := store.ApplyBankTransactionMutations(ctx, tenantID, acctID,
		[]BankTxnMutation{{ExternalRef: "ext-unknown", ValueDate: vd, Description: "x", Amount: decimal.RequireFromString("-1.00"), Currency: "USD"}},
		nil)
	if err != nil {
		t.Fatalf("ApplyBankTransactionMutations: %v", err)
	}
	if res.Updated != 0 || len(res.ModifiedMissing) != 1 || res.ModifiedMissing[0] != "ext-unknown" {
		t.Fatalf("res = %+v; want ModifiedMissing=[ext-unknown]", res)
	}
}
