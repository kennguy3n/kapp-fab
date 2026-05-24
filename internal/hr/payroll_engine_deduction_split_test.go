package hr

import (
	"testing"

	"github.com/shopspring/decimal"
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
// defensive filters: a zero / negative amount is excluded (the
// journal entry would still balance because the perCode total
// would also be zero, but a zero credit line is just noise), and a
// blank account string in the map is treated as "no mapping" so a
// misconfigured tenant doesn't end up posting to account "".
func TestSplitDeductionsByCode_SkipsZeroAndEmpty(t *testing.T) {
	perCode := map[string]decimal.Decimal{
		"NZ_PAYE":      dec("0"),
		"NZ_ACC":       dec("12.34"),
		"NZ_KIWISAVER": dec("-1"), // negative — defensive
	}
	accountMap := map[string]string{
		"NZ_PAYE":      "2401",
		"NZ_ACC":       "", // empty account — should be skipped
		"NZ_KIWISAVER": "2403",
	}
	splits := splitDeductionsByCode(perCode, accountMap)
	if len(splits) != 0 {
		t.Fatalf("expected 0 splits (all rows fail filters), got %+v", splits)
	}

	// Now flip NZ_ACC to a real account: it must come through.
	accountMap["NZ_ACC"] = "2402"
	splits = splitDeductionsByCode(perCode, accountMap)
	if len(splits) != 1 || splits[0].code != "NZ_ACC" {
		t.Fatalf("expected exactly NZ_ACC, got %+v", splits)
	}
	if !splits[0].amount.Equal(dec("12.34")) {
		t.Fatalf("NZ_ACC amount: got %s, want 12.34", splits[0].amount)
	}
	if splits[0].account != "2402" {
		t.Fatalf("NZ_ACC account: got %q, want 2402", splits[0].account)
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
