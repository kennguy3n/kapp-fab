//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestConsolidationGroupRunsAcrossTenants is the Phase M Task 7
// regression. Builds two child tenants with their own ledgers,
// posts a couple of journal entries on each, then runs the
// consolidation against an EUR group with one elimination pair
// covering an inter-company AR row.
//
// Asserts:
//
//  1. CreateGroup persists with the right shape (round-trips via
//     GetGroup).
//  2. RunConsolidation returns rows summed across tenants, with
//     per-tenant Contributions populated.
//  3. The eliminated account is removed from Rows and surfaced
//     under Eliminated.
//  4. TotalDebit ≈ TotalCredit on the consolidated trial balance
//     (post-elimination), guarding against arithmetic drift.
func TestConsolidationGroupRunsAcrossTenants(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("admin pool not configured; consolidation requires BYPASSRLS")
	}
	ctx := context.Background()

	// Two child tenants each with a tiny chart of accounts and one
	// posted JE so the trial balance has something to consolidate.
	// Tenant A debits the inter-company AR (1100) against revenue;
	// tenant B credits the same 1100 code (recording the
	// inter-company payable to A) against an expense. After summing
	// across tenants the 1100 row carries debit=100 / credit=100
	// — a true inter-company wash that nets to zero on elimination,
	// so the surviving rows still satisfy the double-entry invariant.
	tnA := newConsolidationChild(t, h, "child-a", true)
	tnB := newConsolidationChild(t, h, "child-b", false)

	rates := ledger.NewExchangeRateStore(h.pool)
	ledgerStore := ledger.NewPGStore(h.pool, h.publisher, h.auditor)
	store := ledger.NewConsolidationStore(h.adminPool, ledgerStore, rates)

	pair := ledger.EliminationPair{
		FromTenant:  tnA.ID,
		ToTenant:    tnB.ID,
		AccountCode: "1100", // shared AR code; row gets eliminated.
	}
	g, err := store.CreateGroup(ctx, ledger.ConsolidationGroup{
		Name:                 "Test Group",
		PresentationCurrency: "USD",
		MemberTenantIDs:      []uuid.UUID{tnA.ID, tnB.ID},
		EliminationPairs:     []ledger.EliminationPair{pair},
	})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	loaded, err := store.GetGroup(ctx, g.ID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if len(loaded.MemberTenantIDs) != 2 {
		t.Fatalf("got %d members; want 2", len(loaded.MemberTenantIDs))
	}
	if len(loaded.EliminationPairs) != 1 || loaded.EliminationPairs[0].AccountCode != "1100" {
		t.Fatalf("EliminationPairs round-trip = %#v", loaded.EliminationPairs)
	}

	out, err := store.RunConsolidation(ctx, g.ID, time.Now().UTC(), uuid.Nil)
	if err != nil {
		t.Fatalf("RunConsolidation: %v", err)
	}
	if out.GroupID != g.ID {
		t.Fatalf("GroupID mismatch")
	}
	if out.PresentationCurrency != "USD" {
		t.Fatalf("currency = %q; want USD", out.PresentationCurrency)
	}
	// The eliminated 1100 row must be missing from Rows but present
	// in Eliminated.
	for _, r := range out.Rows {
		if r.AccountCode == "1100" {
			t.Fatalf("eliminated row 1100 leaked into Rows")
		}
	}
	foundElim := false
	for _, r := range out.Eliminated {
		if r.AccountCode == "1100" {
			foundElim = true
		}
	}
	if !foundElim {
		t.Fatalf("Eliminated does not include AR row 1100; got %#v", out.Eliminated)
	}
	// Rows that survived must have at least one contribution per
	// row — otherwise the merge logic is empty-summing.
	for _, r := range out.Rows {
		if len(r.Contributions) == 0 {
			t.Fatalf("row %s has no contributions", r.AccountCode)
		}
	}
	if !out.TotalDebit.Equal(out.TotalCredit) {
		t.Fatalf("totals mismatched: debit=%s credit=%s", out.TotalDebit, out.TotalCredit)
	}
}

// newConsolidationChild creates a child tenant with a minimal
// chart of accounts and a single balanced JE so each member
// contributes something to the consolidation. Mirrors the seed
// pattern in phase_d_test.go but trimmed to what consolidation
// needs.
func newConsolidationChild(t *testing.T, h *harness, slugBase string, isAR bool) *tenant.Tenant {
	t.Helper()
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug(slugBase), Name: slugBase, Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant %s: %v", slugBase, err)
	}
	ledgerStore := ledger.NewPGStore(h.pool, h.publisher, h.auditor)
	// Both tenants share the 1100 code as the inter-company
	// reconciliation account. Tenant A also has a Revenue (4000);
	// tenant B has an Expense (5000) so the surviving rows after
	// elimination are non-overlapping and the invariant test below
	// can match contributions per-tenant.
	accounts := []ledger.Account{
		{TenantID: tn.ID, Code: "1100", Name: "Inter-company AR/AP", Type: ledger.AccountTypeAsset, Active: true},
	}
	if isAR {
		accounts = append(accounts, ledger.Account{
			TenantID: tn.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true,
		})
	} else {
		accounts = append(accounts, ledger.Account{
			TenantID: tn.ID, Code: "5000", Name: "Expense", Type: ledger.AccountTypeExpense, Active: true,
		})
	}
	for _, a := range accounts {
		if _, err := ledgerStore.CreateAccount(ctx, a); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	postedAt := time.Now().UTC()
	var lines []ledger.JournalLine
	if isAR {
		// A: receivable from B — debit AR, credit revenue.
		lines = []ledger.JournalLine{
			{AccountCode: "1100", Debit: decimal.NewFromInt(100), Credit: decimal.Zero, Currency: "USD"},
			{AccountCode: "4000", Debit: decimal.Zero, Credit: decimal.NewFromInt(100), Currency: "USD"},
		}
	} else {
		// B: payable to A — debit expense, credit AP (same 1100
		// code so the consolidation merge cancels it out against A).
		lines = []ledger.JournalLine{
			{AccountCode: "5000", Debit: decimal.NewFromInt(100), Credit: decimal.Zero, Currency: "USD"},
			{AccountCode: "1100", Debit: decimal.Zero, Credit: decimal.NewFromInt(100), Currency: "USD"},
		}
	}
	if _, err := ledgerStore.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID:  tn.ID,
		PostedAt:  postedAt,
		Memo:      "seed",
		Lines:     lines,
		CreatedBy: uuid.New(),
	}); err != nil {
		t.Fatalf("post je for %s: %v", slugBase, err)
	}
	return tn
}
