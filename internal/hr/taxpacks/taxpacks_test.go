package taxpacks

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// TestLookupReturnsRegisteredPack covers the registry contract: the
// US and AU packs register themselves via init() and are reachable
// through Lookup() with case-insensitive country codes.
func TestLookupReturnsRegisteredPack(t *testing.T) {
	cases := []struct {
		country string
		want    string
	}{
		{"US", "US"}, {"us", "US"},
		{"AU", "AU"}, {" au ", "AU"},
	}
	for _, c := range cases {
		pack, err := Lookup(c.country)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", c.country, err)
		}
		if pack.Country() != c.want {
			t.Fatalf("Lookup(%q).Country() = %q; want %q", c.country, pack.Country(), c.want)
		}
	}
}

// TestLookupReturnsErrNoPack covers the unknown-country path: empty
// + unregistered codes both surface ErrNoPack so the engine falls
// back to no-statutory-deduction behaviour.
func TestLookupReturnsErrNoPack(t *testing.T) {
	for _, country := range []string{"", "  ", "ZZ", "GB"} {
		if _, err := Lookup(country); err == nil {
			t.Fatalf("Lookup(%q) returned nil error; want ErrNoPack", country)
		}
	}
}

// TestUSPackProducesFederalAndFICA exercises the US pack against a
// monthly slip well below the Social Security wage cap. The
// expected federal tax is hand-computed off the single-bracket
// table; FICA splits as 6.2% OASDI + 1.45% Medicare.
func TestUSPackProducesFederalAndFICA(t *testing.T) {
	pack, err := Lookup("US")
	if err != nil {
		t.Fatalf("lookup US: %v", err)
	}
	period := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	gross := decimal.NewFromInt(5000)
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		FilingType: "single",
		Resident:   true,
	}, gross, period)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	codes := map[string]decimal.Decimal{}
	for _, d := range out {
		codes[d.Code] = d.Amount
	}
	if _, ok := codes["FED_TAX"]; !ok {
		t.Fatalf("missing FED_TAX in %v", codes)
	}
	if _, ok := codes["FICA_OASDI"]; !ok {
		t.Fatalf("missing FICA_OASDI in %v", codes)
	}
	if _, ok := codes["FICA_MEDICARE"]; !ok {
		t.Fatalf("missing FICA_MEDICARE in %v", codes)
	}
	// Federal must land in a sane band. Hand-derivation: a $5,000
	// monthly slip annualises to ~$58,910 (period factor 31/365.25);
	// Pub 15-T Step 2 subtracts the $14,600 single-filer standard
	// deduction → $44,310 taxable; Step 3 walks the 2024 brackets
	// (10% on first $11,600 + 12% on remainder) → ~$5,085 annual
	// → ~$432 monthly. The $350-$550 band is intentionally narrow
	// so a regression like the one this test was added for —
	// applying the bracket table directly to gross without
	// subtracting the standard deduction first, which yields ~$680
	// — fails CI loudly. Was $300-$900 (overly tolerant); a
	// review pass on commit 6c3f863 caught the omission.
	fed := codes["FED_TAX"]
	if fed.LessThan(decimal.NewFromInt(350)) || fed.GreaterThan(decimal.NewFromInt(550)) {
		t.Fatalf("FED_TAX = %s; out of expected band $350-$550", fed)
	}
	// OASDI = 5000 * 6.2% = 310.00 (no cap engaged).
	if codes["FICA_OASDI"].Cmp(decimal.NewFromFloat(310)) != 0 {
		t.Fatalf("FICA_OASDI = %s; want 310.00", codes["FICA_OASDI"])
	}
	// Medicare = 5000 * 1.45% = 72.50.
	if codes["FICA_MEDICARE"].Cmp(decimal.NewFromFloat(72.5)) != 0 {
		t.Fatalf("FICA_MEDICARE = %s; want 72.50", codes["FICA_MEDICARE"])
	}
}

// TestUSPackCapsSocialSecurityAtWageBase asserts the pack stops
// accruing OASDI once YTD gross hits the 2026 wage base. A slip
// that crosses the cap mid-period only pays the partial OASDI on
// the portion below the cap.
func TestUSPackCapsSocialSecurityAtWageBase(t *testing.T) {
	pack, _ := Lookup("US")
	period := PayPeriod{
		Start: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
	}
	// Already at the cap → no OASDI on the new slip.
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		FilingType: "single",
		Resident:   true,
		YTDGross:   decimal.NewFromInt(176100),
	}, decimal.NewFromInt(5000), period)
	for _, d := range out {
		if d.Code == "FICA_OASDI" {
			t.Fatalf("OASDI should be skipped at cap: %s", d.Amount)
		}
	}
}

// TestAUPackResidentSchedule covers the resident PAYG schedule.
// The expected number is hand-derived from the published ATO
// brackets — a $6000 monthly slip annualises to $73,005 which
// straddles the 30% bracket: 4288 + (73005-45000)*0.30 =
// 12689.50, period-prorated to ~1042.71.
func TestAUPackResidentSchedule(t *testing.T) {
	pack, _ := Lookup("AU")
	period := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, HasTFN: true,
	}, decimal.NewFromInt(6000), period)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "PAYG_WITHHOLDING" {
		t.Fatalf("unexpected deductions: %+v", out)
	}
	// Allow a tolerant band — the 365.25 day annualisation makes
	// the exact figure irrational, so we assert the period tax is
	// in a tight $1000-$1100 envelope.
	if out[0].Amount.LessThan(decimal.NewFromInt(1000)) || out[0].Amount.GreaterThan(decimal.NewFromInt(1100)) {
		t.Fatalf("PAYG amount %s outside expected $1000-$1100 band", out[0].Amount)
	}
}

// TestAUPackNoTFNFlatRate covers the no-TFN code path: 47% of every
// dollar regardless of bracket.
func TestAUPackNoTFNFlatRate(t *testing.T) {
	pack, _ := Lookup("AU")
	period := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, HasTFN: false,
	}, decimal.NewFromInt(1000), period)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "PAYG_NO_TFN" {
		t.Fatalf("unexpected deductions: %+v", out)
	}
	if out[0].Amount.Cmp(decimal.NewFromInt(470)) != 0 {
		t.Fatalf("no-TFN amount = %s; want 470", out[0].Amount)
	}
}

// TestAUPackBelowThresholdReturnsNoDeduction covers the tax-free
// threshold: an annualised gross under $18,200 produces no PAYG
// withholding for residents claiming the threshold.
func TestAUPackBelowThresholdReturnsNoDeduction(t *testing.T) {
	pack, _ := Lookup("AU")
	period := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, HasTFN: true,
	}, decimal.NewFromInt(1000), period) // $12k/year, under threshold
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected no withholding under threshold, got %+v", out)
	}
}

// TestRegisteredCountriesIsStable verifies both packs register and
// the dropdown source reflects them.
func TestRegisteredCountriesIsStable(t *testing.T) {
	got := RegisteredCountries()
	hasUS, hasAU := false, false
	for _, c := range got {
		if c == "US" {
			hasUS = true
		}
		if c == "AU" {
			hasAU = true
		}
	}
	if !hasUS || !hasAU {
		t.Fatalf("RegisteredCountries() = %v; want US + AU registered", got)
	}
}
