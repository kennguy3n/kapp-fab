//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/notifications"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// TestBudgetStoreLifecycle exercises the full Phase N5 budget surface
// end-to-end against a real Postgres:
//
//  1. Create a header + 12-month line for one account, then update
//     a couple of months → list reflects the upsert.
//  2. Post a real journal entry through the ledger so journal_lines
//     carry base_amount values.
//  3. Compute variance for the current month — asserts both the
//     monthly amount and the sign-flip rule on expense accounts
//     (debit-normal: actuals > plan → positive variance, i.e.
//     "over-budget").
//  4. Drive the variance-alert handler once and confirm a
//     `budget_variance` notification fires when the variance %
//     crosses the per-budget threshold.
func TestBudgetStoreLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, ledgerStore, _ := newTenantForFinance(t, h)
	actor := uuid.New()
	budgetStore := finance.NewBudgetStore(h.pool)

	// Pick a deterministic fixed clock in the CURRENT calendar year so
	// the variance-alert handler (which gates on b.FiscalYear == now.Year())
	// still considers our budget active. The handler walks current-month
	// rows only, so we also drive the ledger postings into the same month.
	now := time.Date(time.Now().UTC().Year(), time.July, 15, 12, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// --- (1) Header + line creation. ----------------------------------
	threshold := decimal.NewFromFloat(0.05) // 5% — tight so the test
	// triggers an alert at a small variance delta.
	hdr, err := budgetStore.CreateBudget(ctx, finance.Budget{
		TenantID:          tn.ID,
		Name:              "Marketing FY",
		FiscalYear:        now.Year(),
		Status:            finance.BudgetStatusActive,
		VarianceThreshold: &threshold,
		CreatedBy:         &actor,
	})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}

	// 12 monthly amounts of 1000 each on account 6000 (Operating
	// Expense). The current-month plan is therefore 1000.
	var months [12]decimal.Decimal
	for i := range months {
		months[i] = decimal.NewFromInt(1000)
	}
	if _, err := budgetStore.UpsertBudgetLine(ctx, finance.BudgetLine{
		TenantID:    tn.ID,
		BudgetID:    hdr.ID,
		AccountCode: "6000",
		Months:      months,
	}); err != nil {
		t.Fatalf("upsert budget line: %v", err)
	}

	// Idempotent upsert on the same (account, cost_center) replaces
	// the row in place.
	months[int(now.Month())-1] = decimal.NewFromInt(1200)
	if _, err := budgetStore.UpsertBudgetLine(ctx, finance.BudgetLine{
		TenantID:    tn.ID,
		BudgetID:    hdr.ID,
		AccountCode: "6000",
		Months:      months,
	}); err != nil {
		t.Fatalf("re-upsert budget line: %v", err)
	}
	lines, err := budgetStore.ListBudgetLines(ctx, tn.ID, hdr.ID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line after idempotent upsert, got %d", len(lines))
	}
	if got := lines[0].Months[int(now.Month())-1]; !got.Equal(decimal.NewFromInt(1200)) {
		t.Fatalf("post-upsert month %d = %s, want 1200", int(now.Month()), got.String())
	}

	// --- (2) Post a real expense journal so actuals exist. ------------
	// 1500 debit to 6000, credit to 1000 (we need a cash-like asset
	// account — the seed CoA in newTenantForFinance doesn't include
	// one, so add it here).
	if _, err := ledgerStore.CreateAccount(ctx, ledger.Account{
		TenantID: tn.ID, Code: "1000", Name: "Cash", Type: ledger.AccountTypeAsset, Active: true,
	}); err != nil {
		t.Fatalf("seed cash: %v", err)
	}
	entry := ledger.JournalEntry{
		TenantID:  tn.ID,
		Memo:      "test expense",
		PostedAt:  monthStart.Add(48 * time.Hour),
		CreatedBy: actor,
		Lines: []ledger.JournalLine{
			{AccountCode: "6000", Debit: decimal.NewFromInt(1500), Credit: decimal.Zero, Currency: "USD"},
			{AccountCode: "1000", Debit: decimal.Zero, Credit: decimal.NewFromInt(1500), Currency: "USD"},
		},
	}
	if _, err := ledgerStore.PostJournalEntry(ctx, entry); err != nil {
		t.Fatalf("post entry: %v", err)
	}

	// --- (3) Variance report. -----------------------------------------
	report, err := budgetStore.BudgetVsActual(ctx, tn.ID, finance.VarianceQuery{
		BudgetID: hdr.ID,
		From:     monthStart,
		To:       now,
	})
	if err != nil {
		t.Fatalf("budget vs actual: %v", err)
	}
	if len(report.Rows) == 0 {
		t.Fatalf("variance report has no rows")
	}
	wantMonth := monthStart.Format("2006-01")
	var found bool
	for _, row := range report.Rows {
		if row.AccountCode != "6000" || row.Period != wantMonth {
			continue
		}
		found = true
		// Expense account → debit-normal → variance = actual − budget,
		// no sign flip. actual=1500, budget=1200, expect +300.
		if !row.Actual.Equal(decimal.NewFromInt(1500)) {
			t.Fatalf("actual = %s, want 1500", row.Actual.String())
		}
		if !row.Budgeted.Equal(decimal.NewFromInt(1200)) {
			t.Fatalf("budgeted = %s, want 1200", row.Budgeted.String())
		}
		if !row.Variance.Equal(decimal.NewFromInt(300)) {
			t.Fatalf("variance = %s, want 300", row.Variance.String())
		}
		// 300 / 1200 = 0.25 — well above the 5% threshold so the
		// alerter MUST raise a notification.
		if got := row.VariancePct; !got.Equal(decimal.NewFromFloat(0.25)) {
			t.Fatalf("variance_pct = %s, want 0.25", got.String())
		}
	}
	if !found {
		t.Fatalf("no row for account 6000 / month %s", wantMonth)
	}

	// --- (4) Variance alert handler. ----------------------------------
	notifyStore := notifications.NewStore(h.pool)
	handler := finance.NewVarianceAlertHandler(budgetStore, notifyStore).
		WithClock(func() time.Time { return now })
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{
		ActionType: finance.ActionTypeBudgetVariance,
	}); err != nil {
		t.Fatalf("variance alert handle: %v", err)
	}
	notifs, err := notifyStore.List(ctx, tn.ID, notifications.ListFilter{Limit: 25})
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	var sawAlert bool
	for _, n := range notifs {
		if n.Type == "budget_variance" {
			sawAlert = true
			break
		}
	}
	if !sawAlert {
		t.Fatalf("expected a budget_variance notification but found none in %d rows", len(notifs))
	}

	// --- (5) Delete cleans up the line via CASCADE. -------------------
	if err := budgetStore.DeleteBudget(ctx, tn.ID, hdr.ID); err != nil {
		t.Fatalf("delete budget: %v", err)
	}
	remaining, err := budgetStore.ListBudgetLines(ctx, tn.ID, hdr.ID)
	if err != nil {
		t.Fatalf("list lines after delete: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 lines after CASCADE delete, got %d", len(remaining))
	}
}
