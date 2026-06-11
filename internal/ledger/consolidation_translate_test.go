package ledger

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// These tests exercise the pure consolidation math (translation,
// elimination, CTA balancing) and the derived statement builders
// without a database, so the accounting invariants are pinned down
// cheaply and deterministically.

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// rowByCode finds a consolidated row by account code.
func rowByCode(rows []ConsolidatedRow, code string) (ConsolidatedRow, bool) {
	for _, r := range rows {
		if r.AccountCode == code {
			return r, true
		}
	}
	return ConsolidatedRow{}, false
}

// assertBalanced asserts the consolidated trial balance is internally
// consistent: total debit == total credit and residual == 0.
func assertBalanced(t *testing.T, tb *ConsolidatedTrialBalance) {
	t.Helper()
	if !tb.TotalDebit.Equal(tb.TotalCredit) {
		t.Fatalf("unbalanced: debit %s != credit %s", tb.TotalDebit, tb.TotalCredit)
	}
	if !tb.Residual.IsZero() {
		t.Fatalf("residual = %s; want 0", tb.Residual)
	}
}

// TestConsolidateSingleEntityIdentity: one entity already in the
// presentation currency consolidates to itself with no CTA.
func TestConsolidateSingleEntityIdentity(t *testing.T) {
	tn := uuid.New()
	one := decimal.NewFromInt(1)
	entities := []entityTrialBalance{{
		tenantID:     tn,
		baseCurrency: "USD",
		closingRate:  one,
		averageRate:  one,
		rows: []TrialBalanceRow{
			{AccountCode: "1000", Type: AccountTypeAsset, Debit: dec("100")},
			{AccountCode: "3000", Type: AccountTypeEquity, Credit: dec("100")},
		},
	}}
	tb := consolidate(entities, nil, AccountCodeCTA)
	assertBalanced(t, tb)
	if !tb.CTA.IsZero() {
		t.Fatalf("CTA = %s; want 0 for same-currency entity", tb.CTA)
	}
	if _, ok := rowByCode(tb.Rows, AccountCodeCTA); ok {
		t.Fatalf("unexpected CTA row for same-currency consolidation")
	}
	asset, _ := rowByCode(tb.Rows, "1000")
	if !asset.Debit.Equal(dec("100")) {
		t.Fatalf("asset debit = %s; want 100", asset.Debit)
	}
}

// TestConsolidateTranslationBooksPerEntityCTA: a foreign entity whose
// balance-sheet and income-statement accounts translate at different
// rates books a per-entity CTA so the translated entity balances.
func TestConsolidateTranslationBooksPerEntityCTA(t *testing.T) {
	tn := uuid.New()
	entities := []entityTrialBalance{{
		tenantID:     tn,
		baseCurrency: "EUR",
		closingRate:  dec("1.1"),  // balance-sheet accounts
		averageRate:  dec("1.05"), // income-statement accounts
		rows: []TrialBalanceRow{
			{AccountCode: "1000", Type: AccountTypeAsset, Debit: dec("100")},
			{AccountCode: "4000", Type: AccountTypeRevenue, Credit: dec("100")},
		},
	}}
	tb := consolidate(entities, nil, AccountCodeCTA)
	assertBalanced(t, tb)

	asset, _ := rowByCode(tb.Rows, "1000")
	if !asset.Debit.Equal(dec("110")) { // 100 * 1.1 closing
		t.Fatalf("asset debit = %s; want 110", asset.Debit)
	}
	rev, _ := rowByCode(tb.Rows, "4000")
	if !rev.Credit.Equal(dec("105")) { // 100 * 1.05 average
		t.Fatalf("revenue credit = %s; want 105", rev.Credit)
	}
	cta, ok := rowByCode(tb.Rows, AccountCodeCTA)
	if !ok {
		t.Fatalf("expected a CTA row")
	}
	if !cta.Credit.Equal(dec("5")) { // 110 debit vs 105 credit -> 5 credit plug
		t.Fatalf("CTA credit = %s; want 5", cta.Credit)
	}
}

// TestConsolidateEliminationKeepsThirdParty: eliminating an
// intercompany pair removes only the two named tenants' contributions
// to the named accounts; a third entity's balance on the same account
// code survives.
func TestConsolidateEliminationKeepsThirdParty(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	one := decimal.NewFromInt(1)
	mk := func(tn uuid.UUID, rows ...TrialBalanceRow) entityTrialBalance {
		return entityTrialBalance{tenantID: tn, baseCurrency: "USD", closingRate: one, averageRate: one, rows: rows}
	}
	entities := []entityTrialBalance{
		mk(a,
			TrialBalanceRow{AccountCode: "1200", Type: AccountTypeAsset, Debit: dec("50")}, // due from B
			TrialBalanceRow{AccountCode: "3000", Type: AccountTypeEquity, Credit: dec("50")},
		),
		mk(b,
			TrialBalanceRow{AccountCode: "2200", Type: AccountTypeLiability, Credit: dec("50")}, // due to A
			TrialBalanceRow{AccountCode: "3000", Type: AccountTypeEquity, Debit: dec("50")},
		),
		mk(c,
			TrialBalanceRow{AccountCode: "1200", Type: AccountTypeAsset, Debit: dec("30")}, // third-party receivable
			TrialBalanceRow{AccountCode: "3000", Type: AccountTypeEquity, Credit: dec("30")},
		),
	}
	pairs := []EliminationPair{{FromTenant: a, ToTenant: b, FromAccount: "1200", ToAccount: "2200"}}
	tb := consolidate(entities, pairs, AccountCodeCTA)
	assertBalanced(t, tb)

	rec, ok := rowByCode(tb.Rows, "1200")
	if !ok {
		t.Fatalf("third-party 1200 balance should survive elimination")
	}
	if !rec.Debit.Equal(dec("30")) {
		t.Fatalf("1200 debit = %s; want 30 (only third party C)", rec.Debit)
	}
	if _, ok := rowByCode(tb.Rows, "2200"); ok {
		t.Fatalf("2200 should be fully eliminated")
	}
	// Eliminated report should record both removed legs.
	if elimRow, ok := rowByCode(tb.Eliminated, "1200"); !ok || !elimRow.Debit.Equal(dec("50")) {
		t.Fatalf("eliminated 1200 = %+v; want debit 50", elimRow)
	}
	if elimRow, ok := rowByCode(tb.Eliminated, "2200"); !ok || !elimRow.Credit.Equal(dec("50")) {
		t.Fatalf("eliminated 2200 = %+v; want credit 50", elimRow)
	}
}

// TestConsolidateEliminationFXMismatchFoldsToCTA: when the two
// eliminated legs translate at different rates the leftover residual
// is folded into the group CTA so the combined TB still balances.
func TestConsolidateEliminationFXMismatchFoldsToCTA(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	one := decimal.NewFromInt(1)
	entities := []entityTrialBalance{
		{
			tenantID: a, baseCurrency: "USD", closingRate: one, averageRate: one,
			rows: []TrialBalanceRow{
				{AccountCode: "1200", Type: AccountTypeAsset, Debit: dec("100")}, // due from B (USD)
				{AccountCode: "3000", Type: AccountTypeEquity, Credit: dec("100")},
			},
		},
		{
			tenantID: b, baseCurrency: "EUR", closingRate: dec("1.2"), averageRate: dec("1.2"),
			rows: []TrialBalanceRow{
				{AccountCode: "2200", Type: AccountTypeLiability, Credit: dec("100")}, // due to A (EUR)
				{AccountCode: "3000", Type: AccountTypeEquity, Debit: dec("100")},
			},
		},
	}
	pairs := []EliminationPair{{FromTenant: a, ToTenant: b, FromAccount: "1200", ToAccount: "2200"}}
	tb := consolidate(entities, pairs, AccountCodeCTA)
	assertBalanced(t, tb)

	// B's equity translated to 120 debit; A's equity 100 credit; the
	// 20 mismatch lands in the group CTA as a credit plug.
	if !tb.CTA.Equal(dec("20")) {
		t.Fatalf("group CTA = %s; want 20", tb.CTA)
	}
}

// TestConsolidateEliminationCannotTargetCTA: a misconfigured pair that
// names the CTA account as an elimination leg must be ignored, so the
// per-entity translation adjustments survive and the sheet stays
// balanced. Without the guard, eliminating the CTA contributions would
// silently corrupt the CTA and unbalance the consolidated TB.
func TestConsolidateEliminationCannotTargetCTA(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	one := decimal.NewFromInt(1)
	entities := []entityTrialBalance{
		{
			tenantID: a, baseCurrency: "USD", closingRate: one, averageRate: one,
			rows: []TrialBalanceRow{
				{AccountCode: "1000", Type: AccountTypeAsset, Debit: dec("100")},
				{AccountCode: "4000", Type: AccountTypeRevenue, Credit: dec("100")},
			},
		},
		{
			// EUR sub: asset at closing 1.2 (120 debit), revenue at
			// average 1.1 (110 credit) → a 10 per-entity CTA credit plug.
			tenantID: b, baseCurrency: "EUR", closingRate: dec("1.2"), averageRate: dec("1.1"),
			rows: []TrialBalanceRow{
				{AccountCode: "1000", Type: AccountTypeAsset, Debit: dec("100")},
				{AccountCode: "4000", Type: AccountTypeRevenue, Credit: dec("100")},
			},
		},
	}
	// Maliciously/mistakenly try to eliminate the CTA account itself.
	pairs := []EliminationPair{{FromTenant: a, ToTenant: b, FromAccount: AccountCodeCTA, ToAccount: AccountCodeCTA}}
	tb := consolidate(entities, pairs, AccountCodeCTA)

	// The guard must keep the TB balanced and the CTA intact.
	assertBalanced(t, tb)
	if cta, ok := rowByCode(tb.Rows, AccountCodeCTA); !ok || cta.Credit.Sub(cta.Debit).IsZero() {
		t.Fatalf("CTA row should survive elimination with a non-zero plug, got %+v (ok=%v)", cta, ok)
	}
	if _, ok := rowByCode(tb.Eliminated, AccountCodeCTA); ok {
		t.Fatalf("CTA must never appear in the eliminated report")
	}
}

// TestBuildConsolidatedStatements: the derived P&L and balance sheet
// reconcile against a balanced consolidated trial balance.
func TestBuildConsolidatedStatements(t *testing.T) {
	tb := &ConsolidatedTrialBalance{
		Rows: []ConsolidatedRow{
			{AccountCode: "1000", Type: AccountTypeAsset, Debit: dec("200")},
			{AccountCode: "2000", Type: AccountTypeLiability, Credit: dec("50")},
			{AccountCode: "3000", Type: AccountTypeEquity, Credit: dec("100")},
			{AccountCode: "4000", Type: AccountTypeRevenue, Credit: dec("120")},
			{AccountCode: "5000", Type: AccountTypeExpense, Debit: dec("70")},
		},
		TotalDebit:  dec("270"),
		TotalCredit: dec("270"),
	}

	is := buildConsolidatedIncomeStatement(tb)
	if !is.TotalRevenue.Equal(dec("120")) || !is.TotalExpense.Equal(dec("70")) {
		t.Fatalf("P&L totals = rev %s / exp %s; want 120 / 70", is.TotalRevenue, is.TotalExpense)
	}
	if !is.NetIncome.Equal(dec("50")) {
		t.Fatalf("net income = %s; want 50", is.NetIncome)
	}

	bs := buildConsolidatedBalanceSheet(tb, is.NetIncome)
	if !bs.TotalAssets.Equal(dec("200")) {
		t.Fatalf("assets = %s; want 200", bs.TotalAssets)
	}
	if !bs.TotalLiabilities.Equal(dec("50")) {
		t.Fatalf("liabilities = %s; want 50", bs.TotalLiabilities)
	}
	if !bs.TotalEquity.Equal(dec("150")) { // 100 booked + 50 current earnings
		t.Fatalf("equity = %s; want 150", bs.TotalEquity)
	}
	if !bs.Balanced || !bs.Difference.IsZero() {
		t.Fatalf("balance sheet not balanced: diff %s", bs.Difference)
	}
	if _, ok := func() (ConsolidatedStatementRow, bool) {
		for _, r := range bs.Equity {
			if r.AccountCode == CurrentPeriodEarningsCode {
				return r, true
			}
		}
		return ConsolidatedStatementRow{}, false
	}(); !ok {
		t.Fatalf("expected a current-period-earnings equity line")
	}
}

// TestBuildConsolidatedBalanceSheetCurrentEarningsCollision covers the
// edge case where an entity's chart of accounts already carries a real
// equity account on CurrentPeriodEarningsCode. Net income must fold
// into that existing row rather than emit a duplicate code, and the
// totals must still reconcile.
func TestBuildConsolidatedBalanceSheetCurrentEarningsCollision(t *testing.T) {
	tb := &ConsolidatedTrialBalance{
		Rows: []ConsolidatedRow{
			{AccountCode: "1000", Type: AccountTypeAsset, Debit: dec("200")},
			// A real equity account that happens to use the synthetic code.
			{AccountCode: CurrentPeriodEarningsCode, Type: AccountTypeEquity, Credit: dec("80")},
			{AccountCode: "4000", Type: AccountTypeRevenue, Credit: dec("120")},
			{AccountCode: "5000", Type: AccountTypeExpense, Debit: dec("0")},
		},
	}
	bs := buildConsolidatedBalanceSheet(tb, dec("120")) // net income 120

	count := 0
	var earnings ConsolidatedStatementRow
	for _, r := range bs.Equity {
		if r.AccountCode == CurrentPeriodEarningsCode {
			count++
			earnings = r
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one %s equity row, got %d", CurrentPeriodEarningsCode, count)
	}
	if !earnings.Amount.Equal(dec("200")) { // 80 booked + 120 current earnings
		t.Fatalf("merged earnings amount = %s; want 200", earnings.Amount)
	}
	if !bs.TotalEquity.Equal(dec("200")) {
		t.Fatalf("total equity = %s; want 200", bs.TotalEquity)
	}
	if !bs.TotalAssets.Equal(dec("200")) || !bs.Balanced {
		t.Fatalf("balance sheet not balanced: assets %s equity %s diff %s", bs.TotalAssets, bs.TotalEquity, bs.Difference)
	}
}

// TestRevaluationConfigDefaults: empty config falls back to the
// package gain/loss accounts; explicit values are preserved.
func TestRevaluationConfigDefaults(t *testing.T) {
	got := RevaluationConfig{}.withDefaults()
	if got.GainAccount != AccountCodeUnrealizedFXGain || got.LossAccount != AccountCodeUnrealizedFXLoss {
		t.Fatalf("defaults = %+v; want gain %s / loss %s", got, AccountCodeUnrealizedFXGain, AccountCodeUnrealizedFXLoss)
	}
	custom := RevaluationConfig{GainAccount: "4920", LossAccount: "5920"}.withDefaults()
	if custom.GainAccount != "4920" || custom.LossAccount != "5920" {
		t.Fatalf("custom config not preserved: %+v", custom)
	}
}

// TestAllowListParam: an empty allow-list encodes as a nil text[] so
// the sweep covers all accounts; a populated list passes through.
func TestAllowListParam(t *testing.T) {
	if allowListParam(nil) != nil {
		t.Fatalf("nil allow-list should encode as nil param")
	}
	if allowListParam([]string{}) != nil {
		t.Fatalf("empty allow-list should encode as nil param")
	}
	v := allowListParam([]string{"1200"})
	if v == nil {
		t.Fatalf("non-empty allow-list should pass through")
	}
}
