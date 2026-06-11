//go:build integration
// +build integration

// Workstream 3 (step 1) integration coverage: multi-currency
// consolidated statements (closing/average-rate translation + CTA)
// and the on-demand FX revaluation runner posting + persistence.
// Gated behind the `integration` tag and KAPP_TEST_DB_URL /
// KAPP_TEST_ADMIN_DB_URL, the same convention as the other
// integration tests in this package.
//
//	KAPP_TEST_DB_URL=... KAPP_TEST_ADMIN_DB_URL=... \
//	  go test -tags=integration ./internal/integrationtest/ -run Workstream3
package integrationtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestWorkstream3ConsolidatedStatements posts balanced trial
// balances for a USD parent and an EUR subsidiary, then runs the
// consolidated statement pack with a closing rate (1.2) distinct
// from the period average (1.1). It asserts:
//
//   - the consolidated trial balance balances (residual 0);
//   - the translation difference lands in the CTA equity line;
//   - the derived P&L net income and balance sheet reconcile.
func TestWorkstream3ConsolidatedStatements(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("admin pool not configured; consolidation requires BYPASSRLS")
	}
	ctx := context.Background()

	rates := ledger.NewExchangeRateStore(h.pool)
	ledgerStore := ledger.NewPGStore(h.pool, h.publisher, h.auditor).WithExchangeRates(rates)
	asOf := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	// Parent: base USD. Dr asset 200 / Cr revenue 200.
	parent := newFXTenant(t, h, "w3-parent", "USD")
	seedLedgerAccounts(t, ledgerStore,
		ledger.Account{TenantID: parent.ID, Code: "1000", Name: "Cash", Type: ledger.AccountTypeAsset, Active: true},
		ledger.Account{TenantID: parent.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
	)
	postJE(t, ledgerStore, parent.ID, asOf,
		ledger.JournalLine{AccountCode: "1000", Debit: decimal.NewFromInt(200), Currency: "USD"},
		ledger.JournalLine{AccountCode: "4000", Credit: decimal.NewFromInt(200), Currency: "USD"},
	)

	// Subsidiary: base EUR. Dr asset 100 / Cr revenue 100 (in EUR).
	sub := newFXTenant(t, h, "w3-sub", "EUR")
	seedLedgerAccounts(t, ledgerStore,
		ledger.Account{TenantID: sub.ID, Code: "1000", Name: "Cash", Type: ledger.AccountTypeAsset, Active: true},
		ledger.Account{TenantID: sub.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
	)
	postJE(t, ledgerStore, sub.ID, asOf,
		ledger.JournalLine{AccountCode: "1000", Debit: decimal.NewFromInt(100), Currency: "EUR"},
		ledger.JournalLine{AccountCode: "4000", Credit: decimal.NewFromInt(100), Currency: "EUR"},
	)
	// Closing rate EUR→USD = 1.2 for the subsidiary's translation.
	if _, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: sub.ID, FromCurrency: "EUR", ToCurrency: "USD",
		RateDate: asOf, Rate: decimal.NewFromFloat(1.2), Provider: "w3",
	}); err != nil {
		t.Fatalf("upsert EUR→USD: %v", err)
	}

	store := ledger.NewConsolidationStore(h.adminPool, ledgerStore, rates)
	g, err := store.CreateGroup(ctx, ledger.ConsolidationGroup{
		Name:                 "W3 Group",
		PresentationCurrency: "USD",
		MemberTenantIDs:      []uuid.UUID{parent.ID, sub.ID},
	})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// Average rate 1.1 (< closing 1.2) so the subsidiary's revenue
	// translates lower than its asset, producing a CTA.
	pack, err := store.RunStatements(ctx, g.ID, uuid.Nil, ledger.ConsolidationOptions{
		AsOf:         asOf,
		AverageRates: map[string]decimal.Decimal{"EUR": decimal.NewFromFloat(1.1)},
	})
	if err != nil {
		t.Fatalf("RunStatements: %v", err)
	}

	tb := pack.TrialBalance
	if !tb.TotalDebit.Equal(tb.TotalCredit) || !tb.Residual.IsZero() {
		t.Fatalf("consolidated TB unbalanced: debit=%s credit=%s residual=%s", tb.TotalDebit, tb.TotalCredit, tb.Residual)
	}
	// Subsidiary asset 100×1.2=120 debit, revenue 100×1.1=110 credit
	// → 10 CTA credit plug.
	if !tb.CTA.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("CTA = %s; want 10", tb.CTA)
	}
	// Parent revenue 200 + sub revenue 110 = 310 net income.
	if !pack.IncomeStatement.NetIncome.Equal(decimal.NewFromInt(310)) {
		t.Fatalf("net income = %s; want 310", pack.IncomeStatement.NetIncome)
	}
	bs := pack.BalanceSheet
	if !bs.Balanced || !bs.Difference.IsZero() {
		t.Fatalf("balance sheet unbalanced: diff=%s", bs.Difference)
	}
	if !bs.TotalAssets.Equal(decimal.NewFromInt(320)) || !bs.TotalEquity.Equal(decimal.NewFromInt(320)) {
		t.Fatalf("BS totals: assets=%s equity=%s; want 320/320", bs.TotalAssets, bs.TotalEquity)
	}
}

// TestWorkstream3FXRevaluationRunner posts an open USD balance on an
// EUR-functional tenant, moves the USD→EUR rate, runs the revaluation
// runner, and asserts the unrealized loss is posted and the run is
// persisted to fx_revaluation_runs.
func TestWorkstream3FXRevaluationRunner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	rates := ledger.NewExchangeRateStore(h.pool)
	ledgerStore := ledger.NewPGStore(h.pool, h.publisher, h.auditor).WithExchangeRates(rates)

	tn := newFXTenant(t, h, "w3-reval", "EUR")
	seedLedgerAccounts(t, ledgerStore,
		ledger.Account{TenantID: tn.ID, Code: "1200", Name: "USD Bank", Type: ledger.AccountTypeAsset, Active: true},
		ledger.Account{TenantID: tn.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
		ledger.Account{TenantID: tn.ID, Code: ledger.AccountCodeUnrealizedFXGain, Name: "FX Gain", Type: ledger.AccountTypeRevenue, Active: true},
		ledger.Account{TenantID: tn.ID, Code: ledger.AccountCodeUnrealizedFXLoss, Name: "FX Loss", Type: ledger.AccountTypeExpense, Active: true},
	)

	day1 := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	if _, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "USD", ToCurrency: "EUR",
		RateDate: day1, Rate: decimal.NewFromFloat(0.9), Provider: "w3",
	}); err != nil {
		t.Fatalf("upsert USD→EUR day1: %v", err)
	}
	// Open USD 1000 balance on the bank account → base 900 EUR.
	postJE(t, ledgerStore, tn.ID, day1,
		ledger.JournalLine{AccountCode: "1200", Debit: decimal.NewFromInt(1000), Currency: "USD"},
		ledger.JournalLine{AccountCode: "4000", Credit: decimal.NewFromInt(1000), Currency: "USD"},
	)

	// Move the rate down: USD weakens to 0.8 → revalued base 800,
	// a 100 EUR unrealized loss against the recorded 900.
	asOf := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	if _, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "USD", ToCurrency: "EUR",
		RateDate: asOf, Rate: decimal.NewFromFloat(0.8), Provider: "w3",
	}); err != nil {
		t.Fatalf("upsert USD→EUR asOf: %v", err)
	}

	runner := ledger.NewRevaluationRunner(ledgerStore, rates, uuid.New(), ledger.RevaluationConfig{})
	res, err := runner.Run(ctx, tn.ID, asOf, ledger.RevaluationConfig{})
	if err != nil {
		t.Fatalf("revaluation run: %v", err)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("got %d revaluation lines; want 1", len(res.Lines))
	}
	line := res.Lines[0]
	if line.AccountCode != "1200" || !line.Delta.Equal(decimal.NewFromInt(-100)) {
		t.Fatalf("line = %+v; want 1200 delta -100", line)
	}
	if line.EntryID == uuid.Nil {
		t.Fatalf("revaluation line missing posted entry id")
	}
	if !res.TotalLoss.Equal(decimal.NewFromInt(100)) || !res.Net.Equal(decimal.NewFromInt(-100)) {
		t.Fatalf("totals: loss=%s net=%s; want 100/-100", res.TotalLoss, res.Net)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("got %d skipped balances; want 0 (rate present)", len(res.Skipped))
	}

	// The run must be persisted under the tenant's RLS scope.
	var persisted int
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM fx_revaluation_runs WHERE tenant_id = $1 AND net = $2`,
			tn.ID, decimal.NewFromInt(-100),
		).Scan(&persisted)
	}); err != nil {
		t.Fatalf("query persisted run: %v", err)
	}
	if persisted != 1 {
		t.Fatalf("persisted run count = %d; want 1", persisted)
	}
}

// TestWorkstream3FXRevaluationSkipsMissingRate verifies the sweep
// records — rather than silently drops — an open foreign-currency
// balance whose currency has no rate as of the run date. The balance
// is posted through a store with no exchange-rate dependency wired, so
// it lands with a NULL base_amount (the legacy / pre-conversion shape),
// and the run uses a rate store that has no USD→EUR rate at all,
// forcing the missing-rate path.
func TestWorkstream3FXRevaluationSkipsMissingRate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// rates has no USD→EUR rate; ledgerNoRates posts foreign lines
	// without converting (base_amount NULL), so posting needs no rate.
	rates := ledger.NewExchangeRateStore(h.pool)
	ledgerNoRates := ledger.NewPGStore(h.pool, h.publisher, h.auditor)

	tn := newFXTenant(t, h, "w3-reval-skip", "EUR")
	seedLedgerAccounts(t, ledgerNoRates,
		ledger.Account{TenantID: tn.ID, Code: "1200", Name: "USD Bank", Type: ledger.AccountTypeAsset, Active: true},
		ledger.Account{TenantID: tn.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
	)

	// Open USD 1000 balance posted before the run date so the sweep's
	// as-of-date filter includes it.
	postDate := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	postJE(t, ledgerNoRates, tn.ID, postDate,
		ledger.JournalLine{AccountCode: "1200", Debit: decimal.NewFromInt(1000), Currency: "USD"},
		ledger.JournalLine{AccountCode: "4000", Credit: decimal.NewFromInt(1000), Currency: "USD"},
	)

	asOf := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	runner := ledger.NewRevaluationRunner(ledgerNoRates, rates, uuid.New(), ledger.RevaluationConfig{})
	res, err := runner.Run(ctx, tn.ID, asOf, ledger.RevaluationConfig{})
	if err != nil {
		t.Fatalf("revaluation run: %v", err)
	}
	if len(res.Lines) != 0 {
		t.Fatalf("got %d revaluation lines; want 0 (no rate)", len(res.Lines))
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("got %d skipped balances; want 1", len(res.Skipped))
	}
	skip := res.Skipped[0]
	if skip.AccountCode != "1200" || skip.Currency != "USD" || skip.BaseCurrency != "EUR" {
		t.Fatalf("skip = %+v; want 1200/USD/EUR", skip)
	}
	if !skip.ForeignNet.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("skip foreign net = %s; want 1000", skip.ForeignNet)
	}
	if skip.Reason == "" {
		t.Fatalf("skip reason should explain the missing rate")
	}
}

// TestWorkstream3FXRevaluationAsOfExcludesLaterPostings verifies the
// sweep is point-in-time: a balance posted after the run date is not
// revalued, so a period-end run reflects the balance as it stood at
// the period end rather than the current all-time balance.
func TestWorkstream3FXRevaluationAsOfExcludesLaterPostings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	rates := ledger.NewExchangeRateStore(h.pool)
	ledgerStore := ledger.NewPGStore(h.pool, h.publisher, h.auditor).WithExchangeRates(rates)

	tn := newFXTenant(t, h, "w3-reval-asof", "EUR")
	seedLedgerAccounts(t, ledgerStore,
		ledger.Account{TenantID: tn.ID, Code: "1200", Name: "USD Bank", Type: ledger.AccountTypeAsset, Active: true},
		ledger.Account{TenantID: tn.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
		ledger.Account{TenantID: tn.ID, Code: ledger.AccountCodeUnrealizedFXGain, Name: "FX Gain", Type: ledger.AccountTypeRevenue, Active: true},
		ledger.Account{TenantID: tn.ID, Code: ledger.AccountCodeUnrealizedFXLoss, Name: "FX Loss", Type: ledger.AccountTypeExpense, Active: true},
	)

	rateDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "USD", ToCurrency: "EUR",
		RateDate: rateDate, Rate: decimal.NewFromFloat(0.9), Provider: "w3",
	}); err != nil {
		t.Fatalf("upsert USD→EUR: %v", err)
	}

	asOf := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	// Posted AFTER asOf: must be excluded from an asOf period-end run.
	postJE(t, ledgerStore, tn.ID, time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		ledger.JournalLine{AccountCode: "1200", Debit: decimal.NewFromInt(1000), Currency: "USD"},
		ledger.JournalLine{AccountCode: "4000", Credit: decimal.NewFromInt(1000), Currency: "USD"},
	)

	runner := ledger.NewRevaluationRunner(ledgerStore, rates, uuid.New(), ledger.RevaluationConfig{})
	res, err := runner.Run(ctx, tn.ID, asOf, ledger.RevaluationConfig{})
	if err != nil {
		t.Fatalf("revaluation run: %v", err)
	}
	if len(res.Lines) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("post-asOf balance leaked into run: lines=%d skipped=%d", len(res.Lines), len(res.Skipped))
	}
}

// newFXTenant creates a tenant with the given functional currency.
func newFXTenant(t *testing.T, h *harness, slug, currency string) *tenant.Tenant {
	t.Helper()
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug(slug), Name: slug, Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
	if err := h.tenants.SetBaseCurrency(ctx, tn.ID, currency); err != nil {
		t.Fatalf("set base currency %s: %v", currency, err)
	}
	return tn
}

func seedLedgerAccounts(t *testing.T, store *ledger.PGStore, accounts ...ledger.Account) {
	t.Helper()
	ctx := context.Background()
	for _, a := range accounts {
		if _, err := store.CreateAccount(ctx, a); err != nil {
			t.Fatalf("seed account %s: %v", a.Code, err)
		}
	}
}

func postJE(t *testing.T, store *ledger.PGStore, tenantID uuid.UUID, postedAt time.Time, lines ...ledger.JournalLine) {
	t.Helper()
	if _, err := store.PostJournalEntry(context.Background(), ledger.JournalEntry{
		TenantID:  tenantID,
		PostedAt:  postedAt,
		Memo:      "w3-seed",
		Lines:     lines,
		CreatedBy: uuid.New(),
	}); err != nil {
		t.Fatalf("post je: %v", err)
	}
}
