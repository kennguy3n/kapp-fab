package taxpacks

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// dptr returns a pointer to a decimal, for the *decimal.Decimal base
// fields on EmployeeInfo (nil = unset, non-nil = use verbatim).
func dptr(d decimal.Decimal) *decimal.Decimal { return &d }

// codeMap collapses a deduction slice to a code→amount lookup.
func codeMap(out []Deduction) map[string]decimal.Decimal {
	m := make(map[string]decimal.Decimal, len(out))
	for _, d := range out {
		m[d.Code] = d.Amount
	}
	return m
}

// TestUSPack401kReducesIncomeTaxNotFICA proves the two-base split: a
// pre-tax 401(k) deduction reduces the income-tax base but NOT the FICA
// contribution base. We compare a slip with a $1,000 pre-tax deferral
// (TaxableGross 9,000, ContributionGross 10,000) against an identical
// slip with no deferral (10,000 on both bases): federal income tax must
// drop, while OASDI + Medicare stay pinned to the full $10,000 gross.
func TestUSPack401kReducesIncomeTaxNotFICA(t *testing.T) {
	pack, err := Lookup("US")
	if err != nil {
		t.Fatalf("lookup US: %v", err)
	}
	period := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	gross := decimal.NewFromInt(10000)

	withDeferral, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		FilingType:        "single",
		Resident:          true,
		TaxableGross:      dptr(decimal.NewFromInt(9000)),  // income-tax base (post 401k)
		ContributionGross: dptr(decimal.NewFromInt(10000)), // FICA base unchanged
	}, gross, period)
	if err != nil {
		t.Fatalf("compute (deferral): %v", err)
	}
	noDeferral, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		FilingType:        "single",
		Resident:          true,
		TaxableGross:      dptr(decimal.NewFromInt(10000)),
		ContributionGross: dptr(decimal.NewFromInt(10000)),
	}, gross, period)
	if err != nil {
		t.Fatalf("compute (no deferral): %v", err)
	}

	a := codeMap(withDeferral)
	b := codeMap(noDeferral)

	// FICA OASDI on the FULL $10,000 = $620; Medicare = $145. Pre-tax
	// must NOT shift these.
	if a["FICA_OASDI"].Cmp(decimal.NewFromInt(620)) != 0 {
		t.Fatalf("FICA_OASDI = %s; want 620.00 (6.2%% of full gross)", a["FICA_OASDI"])
	}
	if a["FICA_MEDICARE"].Cmp(decimal.NewFromFloat(145)) != 0 {
		t.Fatalf("FICA_MEDICARE = %s; want 145.00 (1.45%% of full gross)", a["FICA_MEDICARE"])
	}
	if a["FICA_OASDI"].Cmp(b["FICA_OASDI"]) != 0 || a["FICA_MEDICARE"].Cmp(b["FICA_MEDICARE"]) != 0 {
		t.Fatalf("pre-tax deferral changed FICA: deferral=%v vs none=%v", a, b)
	}
	// Income tax MUST drop when the pre-tax deferral reduces the base.
	if !a["FED_TAX"].LessThan(b["FED_TAX"]) {
		t.Fatalf("FED_TAX did not drop with pre-tax deferral: deferral=%s none=%s",
			a["FED_TAX"], b["FED_TAX"])
	}
}

// TestUSPackSSCapUsesContributionYTD proves the OASDI wage-base cap reads
// the cumulative CONTRIBUTION base (YTDContributionGross), not the
// income-tax cumulative (YTDGross). An employee already over the cap on
// contribution wages stops accruing OASDI even though the income-tax YTD
// (reduced by a year of pre-tax deferrals) is well under the cap.
func TestUSPackSSCapUsesContributionYTD(t *testing.T) {
	pack, _ := Lookup("US")
	period := PayPeriod{
		Start: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		FilingType:           "single",
		Resident:             true,
		YTDGross:             decimal.NewFromInt(120000),       // income-tax cumulative, under cap
		YTDContributionGross: dptr(decimal.NewFromInt(176100)), // contribution cumulative, at cap
		ContributionGross:    dptr(decimal.NewFromInt(5000)),
	}, decimal.NewFromInt(5000), period)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	for _, d := range codeMapKeys(out) {
		if d == "FICA_OASDI" {
			t.Fatalf("OASDI must stop once contribution YTD hits the cap: %+v", out)
		}
	}
	// Medicare has no cap and must still accrue on the full slip.
	if codeMap(out)["FICA_MEDICARE"].Cmp(decimal.NewFromFloat(72.5)) != 0 {
		t.Fatalf("FICA_MEDICARE = %s; want 72.50", codeMap(out)["FICA_MEDICARE"])
	}
}

// TestUSPackZeroTaxableBaseYieldsZeroIncomeTax guards the *decimal.Decimal
// base fields: when pre-tax deductions wipe out the income-tax base
// (TaxableGross = &0, a legitimate computed zero — NOT "unset"), the pack
// must compute zero federal income tax, not fall back to taxing the full
// gross. FICA still accrues on the full contribution base. This is the
// regression the zero-vs-nil sentinel fix protects against.
func TestUSPackZeroTaxableBaseYieldsZeroIncomeTax(t *testing.T) {
	pack, _ := Lookup("US")
	period := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	gross := decimal.NewFromInt(10000)
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		FilingType:        "single",
		Resident:          true,
		TaxableGross:      dptr(decimal.Zero),              // pre-tax wiped the income-tax base
		ContributionGross: dptr(decimal.NewFromInt(10000)), // FICA base unchanged
	}, gross, period)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	m := codeMap(out)
	if m["FED_TAX"].IsPositive() {
		t.Fatalf("FED_TAX must be zero when the income-tax base is zero, got %s (fell back to full gross?)", m["FED_TAX"])
	}
	// FICA still on the full $10,000 contribution base.
	if m["FICA_OASDI"].Cmp(decimal.NewFromInt(620)) != 0 {
		t.Fatalf("FICA_OASDI = %s; want 620.00 (6.2%% of full gross)", m["FICA_OASDI"])
	}
}

// codeMapKeys returns the deduction codes present in the slice.
func codeMapKeys(out []Deduction) []string {
	keys := make([]string, 0, len(out))
	for _, d := range out {
		keys = append(keys, d.Code)
	}
	return keys
}

// TestMYPackEPFIgnoresPreTax proves a non-US contribution (Malaysia EPF /
// SOCSO) runs on the full contribution base regardless of any pre-tax
// deduction that reduced the income-tax base. EPF on a $9,000 income-tax
// base + $10,000 contribution base must equal EPF on a flat $10,000.
func TestMYPackEPFIgnoresPreTax(t *testing.T) {
	pack, err := Lookup("MY")
	if err != nil {
		t.Fatalf("lookup MY: %v", err)
	}
	period := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	gross := decimal.NewFromInt(10000)

	withPreTax, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident:          true,
		TaxableGross:      dptr(decimal.NewFromInt(9000)),
		ContributionGross: dptr(decimal.NewFromInt(10000)),
	}, gross, period)
	if err != nil {
		t.Fatalf("compute (pretax): %v", err)
	}
	noPreTax, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident:          true,
		TaxableGross:      dptr(decimal.NewFromInt(10000)),
		ContributionGross: dptr(decimal.NewFromInt(10000)),
	}, gross, period)
	if err != nil {
		t.Fatalf("compute (no pretax): %v", err)
	}

	a := codeMap(withPreTax)
	b := codeMap(noPreTax)
	if a["MY_EPF"].Cmp(b["MY_EPF"]) != 0 {
		t.Fatalf("MY_EPF changed with pre-tax: pretax=%s none=%s", a["MY_EPF"], b["MY_EPF"])
	}
	if !a["MY_EPF"].IsPositive() {
		t.Fatalf("expected positive MY_EPF, got %s", a["MY_EPF"])
	}
	// EPF is 11% of the full $10,000 contribution base = $1,100.
	if a["MY_EPF"].Cmp(decimal.NewFromInt(1100)) != 0 {
		t.Fatalf("MY_EPF = %s; want 1100.00 (11%% of full gross)", a["MY_EPF"])
	}
}
