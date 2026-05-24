package hr

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
)

// dec is a thin helper that panics on parse failure — fixture values
// are constants so a parse failure is a programmer error, not a
// runtime condition the test should tolerate.
func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// TestSplitDeductionsByCode_NoMap covers the legacy path: a tenant
// that hasn't configured DeductionAccountMap MUST get the same
// roll-up behaviour as before Phase M2 — every deduction collapses
// into salary_payable so existing pay_runs reproduce verbatim. The
// helper returns nil for nil/empty maps so PostPayRun's loop is a
// no-op.
func TestSplitDeductionsByCode_NoMap(t *testing.T) {
	perCode := map[string]decimal.Decimal{
		"MY_EPF":   dec("500"),
		"FED_TAX":  dec("1200"),
		"MY_SOCSO": dec("25"),
	}
	got := splitDeductionsByCode(perCode, nil)
	if got != nil {
		t.Fatalf("nil accountMap: expected nil splits, got %+v", got)
	}
	got = splitDeductionsByCode(perCode, map[string]string{})
	if got != nil {
		t.Fatalf("empty accountMap: expected nil splits, got %+v", got)
	}
}

// TestSplitDeductionsByCode_PerCodeMapping covers the production
// path: each mapped code gets its own (account, amount) split with
// the account from the map. Unmapped codes are excluded — PostPayRun
// catches them in the `unmapped` rollover and credits salary_payable
// so the entry balances.
func TestSplitDeductionsByCode_PerCodeMapping(t *testing.T) {
	perCode := map[string]decimal.Decimal{
		"MY_EPF":   dec("500.00"),
		"MY_SOCSO": dec("25.00"),
		"MY_EIS":   dec("10.00"),
		"MY_PCB":   dec("1200.00"),
	}
	accountMap := map[string]string{
		"MY_EPF":   "2310", // EPF / KWSP payable
		"MY_SOCSO": "2320", // SOCSO / PERKESO payable
		"MY_EIS":   "2330", // EIS payable
		// MY_PCB intentionally absent — should roll into salary_payable.
	}
	splits := splitDeductionsByCode(perCode, accountMap)
	if len(splits) != 3 {
		t.Fatalf("expected 3 mapped splits, got %d: %+v", len(splits), splits)
	}
	// Deterministic ordering: codes must come back sorted ASC so
	// audit diffs stay stable across reposts. The expected sort is
	// MY_EIS < MY_EPF < MY_SOCSO.
	want := []struct {
		code, account string
		amount        string
	}{
		{"MY_EIS", "2330", "10.00"},
		{"MY_EPF", "2310", "500.00"},
		{"MY_SOCSO", "2320", "25.00"},
	}
	for i, w := range want {
		if splits[i].code != w.code {
			t.Errorf("splits[%d].code: got %q, want %q", i, splits[i].code, w.code)
		}
		if splits[i].account != w.account {
			t.Errorf("splits[%d].account: got %q, want %q", i, splits[i].account, w.account)
		}
		if !splits[i].amount.Equal(dec(w.amount)) {
			t.Errorf("splits[%d].amount: got %s, want %s", i, splits[i].amount, w.amount)
		}
	}
}

// TestSplitDeductionsByCode_SkipsZeroAndEmpty exercises the
// defensive filters: a zero amount is excluded (a zero credit
// line is just audit noise), and a blank account string in the
// map is treated as "no mapping" so a misconfigured tenant
// doesn't end up posting to account "".
//
// Negative amounts come through (credit-style adjustments —
// PostPayRun inverts them into Dr lines), so this test no longer
// expects negative aggregates to be filtered. The journal-entry
// balance proof in TestPostPayRun_*JournalBalance below covers
// the negative path end-to-end.
func TestSplitDeductionsByCode_SkipsZeroAndEmpty(t *testing.T) {
	perCode := map[string]decimal.Decimal{
		"NZ_PAYE":      dec("0"),     // zero — should be skipped
		"NZ_ACC":       dec("12.34"),
		"NZ_KIWISAVER": dec("-1.00"), // negative — should come through with sign
	}
	accountMap := map[string]string{
		"NZ_PAYE":      "2401",
		"NZ_ACC":       "", // empty account — should be skipped
		"NZ_KIWISAVER": "2403",
	}
	splits := splitDeductionsByCode(perCode, accountMap)
	if len(splits) != 1 {
		t.Fatalf("expected exactly NZ_KIWISAVER (negative), got %+v", splits)
	}
	if splits[0].code != "NZ_KIWISAVER" {
		t.Fatalf("first split: got %q, want NZ_KIWISAVER", splits[0].code)
	}
	if !splits[0].amount.Equal(dec("-1.00")) {
		t.Fatalf("NZ_KIWISAVER amount: got %s, want -1.00", splits[0].amount)
	}
	if splits[0].account != "2403" {
		t.Fatalf("NZ_KIWISAVER account: got %q, want 2403", splits[0].account)
	}

	// Now flip NZ_ACC to a real account: it must come through too
	// (positive, alongside the negative NZ_KIWISAVER). NZ_PAYE
	// stays at zero, still excluded.
	accountMap["NZ_ACC"] = "2402"
	splits = splitDeductionsByCode(perCode, accountMap)
	if len(splits) != 2 {
		t.Fatalf("expected 2 splits (NZ_ACC + NZ_KIWISAVER), got %+v", splits)
	}
	// Deterministic sort: NZ_ACC < NZ_KIWISAVER.
	if splits[0].code != "NZ_ACC" || !splits[0].amount.Equal(dec("12.34")) {
		t.Fatalf("splits[0]: got %+v, want NZ_ACC=12.34", splits[0])
	}
	if splits[1].code != "NZ_KIWISAVER" || !splits[1].amount.Equal(dec("-1.00")) {
		t.Fatalf("splits[1]: got %+v, want NZ_KIWISAVER=-1.00", splits[1])
	}
}

// TestSplitDeductionsByCode_PreservesNegativeAggregates pins the
// signed-arithmetic contract: when perCodeDeductions arrives with
// a net negative aggregate for a code (positive deduction + larger
// negative refund of the same code summed at the call site), the
// helper returns the negative amount intact so PostPayRun can
// invert it into a Dr line. Closing BUG-0001 (Devin Review on
// PR #93): the old positivity filter dropped the row and broke
// the journal-entry balance.
func TestSplitDeductionsByCode_PreservesNegativeAggregates(t *testing.T) {
	perCode := map[string]decimal.Decimal{
		"TAX_REFUND": dec("-20.00"),
	}
	accountMap := map[string]string{
		"TAX_REFUND": "2401",
	}
	splits := splitDeductionsByCode(perCode, accountMap)
	if len(splits) != 1 {
		t.Fatalf("expected 1 split, got %+v", splits)
	}
	if !splits[0].amount.Equal(dec("-20.00")) {
		t.Fatalf("amount: got %s, want -20.00 (negative preserved)", splits[0].amount)
	}
	if splits[0].account != "2401" {
		t.Fatalf("account: got %q, want 2401", splits[0].account)
	}
}

// TestSplitDeductionsByCode_SumPreservation is the invariant
// PostPayRun depends on for journal-entry balancing: the sum of
// every mapped split + the unmapped remainder must equal the total
// across perCode. If this invariant ever breaks, the journal entry
// will fail ledger.Post's debit-vs-credit reconciliation check.
func TestSplitDeductionsByCode_SumPreservation(t *testing.T) {
	perCode := map[string]decimal.Decimal{
		"SG_CPF_EMPLOYEE": dec("400.00"),
		"SG_NONRES_TAX":   dec("150.00"),
		"FOREIGN_OTHER":   dec("75.00"),
	}
	accountMap := map[string]string{
		"SG_CPF_EMPLOYEE": "2311",
		// SG_NONRES_TAX, FOREIGN_OTHER deliberately unmapped.
	}
	splits := splitDeductionsByCode(perCode, accountMap)
	if len(splits) != 1 {
		t.Fatalf("expected 1 mapped split, got %+v", splits)
	}
	total := decimal.Zero
	for _, d := range perCode {
		total = total.Add(d)
	}
	mapped := decimal.Zero
	for _, s := range splits {
		mapped = mapped.Add(s.amount)
	}
	unmapped := total.Sub(mapped)
	if !unmapped.Equal(dec("225.00")) {
		t.Fatalf("unmapped balance: got %s, want 225.00 (mapped=%s, total=%s)",
			unmapped, mapped, total)
	}
}

// assertJournalBalances walks a list of ledger.JournalLine entries
// and checks both ΣDebit == ΣCredit (the cardinal double-entry
// invariant) and that the totals match the expected magnitude. It
// also rejects any line that carries both a debit and a credit,
// which the ledger.PostJournalEntry path forbids.
func assertJournalBalances(t *testing.T, lines []ledger.JournalLine, wantTotal decimal.Decimal) {
	t.Helper()
	totalDr := decimal.Zero
	totalCr := decimal.Zero
	for i := range lines {
		l := &lines[i]
		if l.Debit.IsPositive() && l.Credit.IsPositive() {
			t.Fatalf("line %d: cannot carry both debit (%s) and credit (%s)", i, l.Debit, l.Credit)
		}
		totalDr = totalDr.Add(l.Debit)
		totalCr = totalCr.Add(l.Credit)
	}
	if !totalDr.Equal(totalCr) {
		t.Fatalf("journal entry unbalanced: ΣDebit=%s, ΣCredit=%s, lines=%+v", totalDr, totalCr, lines)
	}
	if !totalDr.Equal(wantTotal) {
		t.Fatalf("journal entry totals: got %s, want %s (=expected magnitude)", totalDr, wantTotal)
	}
}

// TestBuildPayrollJournalLines_NoMappingBalances pins the baseline
// (no DeductionAccountMap) — every deduction rolls into
// salary_payable. The entry must balance and the totals must
// equal gross.
func TestBuildPayrollJournalLines_NoMappingBalances(t *testing.T) {
	lines := buildPayrollJournalLines(payrollJournalInput{
		Gross:      dec("6000"),
		Net:        dec("5340"),
		Deductions: dec("660"),
		PerCodeDeductions: map[string]decimal.Decimal{
			"MY_EPF": dec("660"),
		},
		DeductionAccountMap:      nil,
		SalaryExpenseAccountCode: "5100",
		SalaryPayableAccountCode: "2100",
		Currency:                 "MYR",
	})
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (expense, net, deductions catch-all), got %d: %+v", len(lines), lines)
	}
	assertJournalBalances(t, lines, dec("6000"))
}

// TestBuildPayrollJournalLines_AllMappedBalances pins the
// per-code mapping path: every deduction code has its own
// liability account and the catch-all salary_payable rollover
// line is omitted (unmapped == 0).
func TestBuildPayrollJournalLines_AllMappedBalances(t *testing.T) {
	lines := buildPayrollJournalLines(payrollJournalInput{
		Gross:      dec("6000"),
		Net:        dec("5305"),
		Deductions: dec("695"),
		PerCodeDeductions: map[string]decimal.Decimal{
			"MY_EPF":   dec("660"),
			"MY_SOCSO": dec("25"),
			"MY_EIS":   dec("10"),
		},
		DeductionAccountMap: map[string]string{
			"MY_EPF":   "2310",
			"MY_SOCSO": "2320",
			"MY_EIS":   "2330",
		},
		SalaryExpenseAccountCode: "5100",
		SalaryPayableAccountCode: "2100",
		Currency:                 "MYR",
	})
	// Expense + net + 3 mapped splits = 5 lines, no rollover.
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines (expense, net, 3 mapped), got %d: %+v", len(lines), lines)
	}
	assertJournalBalances(t, lines, dec("6000"))
}

// TestBuildPayrollJournalLines_PartialMappingBalances pins the
// mixed path: some codes mapped, others fall through to the
// catch-all salary_payable rollover. This is the production case
// for tenants who only configured the most common statutory
// liability accounts (e.g. EPF mapped, PCB not yet mapped).
func TestBuildPayrollJournalLines_PartialMappingBalances(t *testing.T) {
	lines := buildPayrollJournalLines(payrollJournalInput{
		Gross:      dec("8000"),
		Net:        dec("6200"),
		Deductions: dec("1800"),
		PerCodeDeductions: map[string]decimal.Decimal{
			"MY_EPF":   dec("600"),
			"MY_PCB":   dec("1200"),
		},
		DeductionAccountMap: map[string]string{
			"MY_EPF": "2310",
			// MY_PCB intentionally unmapped → rolls into salary_payable
		},
		SalaryExpenseAccountCode: "5100",
		SalaryPayableAccountCode: "2100",
		Currency:                 "MYR",
	})
	// expense + net + EPF + rollover = 4 lines
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %+v", len(lines), lines)
	}
	assertJournalBalances(t, lines, dec("8000"))
}

// TestBuildPayrollJournalLines_NegativeMappedCodeBalances is the
// regression test for BUG-0001 (Devin Review on PR #93). A slip
// carries a credit-style component for a mapped code (e.g. a
// tax-refund line with negative amount), and TotalDeductions
// reflects that signed total. Before the fix, perCodeDeductions
// dropped the negative line, leaving the mapped split overstated
// relative to `deductions`, and the resulting journal entry
// failed ledger.PostJournalEntry's balance check. Post-fix, the
// negative-aggregate code emits a Dr line that absorbs the
// adjustment cleanly.
func TestBuildPayrollJournalLines_NegativeMappedCodeBalances(t *testing.T) {
	// One slip: TAX +100, TAX_REFUND -20 (different codes).
	// TAX is mapped, TAX_REFUND is not. Total deductions = 80.
	lines := buildPayrollJournalLines(payrollJournalInput{
		Gross:      dec("1000"),
		Net:        dec("920"),
		Deductions: dec("80"),
		PerCodeDeductions: map[string]decimal.Decimal{
			"TAX":        dec("100"),
			"TAX_REFUND": dec("-20"),
		},
		DeductionAccountMap: map[string]string{
			"TAX": "2401",
			// TAX_REFUND intentionally unmapped → contributes
			// -20 to the catch-all `unmapped` balance, which
			// then becomes a Dr on salary_payable.
		},
		SalaryExpenseAccountCode: "5100",
		SalaryPayableAccountCode: "2100",
		Currency:                 "USD",
	})
	// expense (Dr 1000) + net (Cr 920) + TAX (Cr 100) + rollover (Dr 20) = 4 lines
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %+v", len(lines), lines)
	}
	// ΣDr = 1000 (expense) + 20 (rollover) = 1020
	// ΣCr = 920 (net) + 100 (TAX) = 1020 ✓
	assertJournalBalances(t, lines, dec("1020"))
	// Also check the rollover line is a debit (negative unmapped → Dr).
	rollover := lines[len(lines)-1]
	if !rollover.Debit.Equal(dec("20")) {
		t.Fatalf("rollover debit: got %s, want 20", rollover.Debit)
	}
	if !rollover.Credit.IsZero() {
		t.Fatalf("rollover credit: got %s, want 0", rollover.Credit)
	}
}

// TestBuildPayrollJournalLines_NegativeMappedAggregateBalances
// exercises the corner where a mapped code's *aggregate* is
// negative — e.g. two slips, one with TAX +50 and another with
// TAX -80 (an over-withholding correction in the second slip).
// The Dr line on the mapped TAX liability account inverts the
// sign so the entry still balances.
func TestBuildPayrollJournalLines_NegativeMappedAggregateBalances(t *testing.T) {
	// TAX aggregate = +50 - 80 = -30; deductions = -30.
	// Gross 1000, net 1030 (net > gross because TAX adjustment
	// nets out to a refund). This is rare but legal.
	lines := buildPayrollJournalLines(payrollJournalInput{
		Gross:      dec("1000"),
		Net:        dec("1030"),
		Deductions: dec("-30"),
		PerCodeDeductions: map[string]decimal.Decimal{
			"TAX": dec("-30"),
		},
		DeductionAccountMap: map[string]string{
			"TAX": "2401",
		},
		SalaryExpenseAccountCode: "5100",
		SalaryPayableAccountCode: "2100",
		Currency:                 "USD",
	})
	// expense + net + TAX Dr = 3 lines, no rollover.
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %+v", len(lines), lines)
	}
	taxLine := lines[2]
	if taxLine.AccountCode != "2401" {
		t.Fatalf("TAX line account: got %q, want 2401", taxLine.AccountCode)
	}
	if !taxLine.Debit.Equal(dec("30")) {
		t.Fatalf("TAX line debit: got %s, want 30 (sign-inverted)", taxLine.Debit)
	}
	if !taxLine.Credit.IsZero() {
		t.Fatalf("TAX line credit: got %s, want 0", taxLine.Credit)
	}
	// ΣDr = 1000 (expense) + 30 (TAX inverted) = 1030
	// ΣCr = 1030 (net) = 1030 ✓
	assertJournalBalances(t, lines, dec("1030"))
}

// TestBuildPayrollJournalLines_EmptyCodedDeductionsAbsorbed pins
// the empty-code path: an ad-hoc deduction line with Code == ""
// is not tracked per-code (so it stays out of perCodeDeductions)
// but its amount is included in `deductions` (since the slip's
// TotalDeductions counts every deduction line). The unmapped
// rollover absorbs the difference cleanly.
func TestBuildPayrollJournalLines_EmptyCodedDeductionsAbsorbed(t *testing.T) {
	// Slip has [MY_EPF: 660, "": 50] where the empty-coded line
	// is an ad-hoc adjustment. perCodeDeductions only carries
	// MY_EPF; deductions = 660 + 50 = 710. The rollover line
	// picks up the 50 difference and credits salary_payable.
	lines := buildPayrollJournalLines(payrollJournalInput{
		Gross:      dec("6000"),
		Net:        dec("5290"),
		Deductions: dec("710"),
		PerCodeDeductions: map[string]decimal.Decimal{
			"MY_EPF": dec("660"),
		},
		DeductionAccountMap: map[string]string{
			"MY_EPF": "2310",
		},
		SalaryExpenseAccountCode: "5100",
		SalaryPayableAccountCode: "2100",
		Currency:                 "MYR",
	})
	// expense + net + EPF (Cr) + rollover (Cr 50) = 4 lines
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %+v", len(lines), lines)
	}
	rollover := lines[len(lines)-1]
	if !rollover.Credit.Equal(dec("50")) {
		t.Fatalf("rollover credit: got %s, want 50 (=empty-coded absorbed)", rollover.Credit)
	}
	assertJournalBalances(t, lines, dec("6000"))
}
