package taxpacks

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// monthPeriod returns a calendar-month period for the tests below.
// All hand-derived expected values assume Jan 1-31 (31 days) so a
// shared helper keeps the calls terse and the math reproducible.
func monthPeriod() PayPeriod {
	return PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
}

// ----- Singapore -----

// TestSGPackResidentCPFBelowCeiling: a SGD 5,000 / month slip for
// a 30-year-old citizen / PR. Hand-derivation: tier 0 (age ≤55,
// 20%); OW base = min(5000, 7400) = 5000; CPF = 1,000.00. No PAYE
// (Singapore has no monthly income-tax withholding for residents).
func TestSGPackResidentCPFBelowCeiling(t *testing.T) {
	pack, err := Lookup("SG")
	if err != nil {
		t.Fatalf("lookup SG: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(5000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "SG_CPF_EMPLOYEE" {
		t.Fatalf("unexpected deductions: %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("CPF amount: got %s, want 1000.00", out[0].Amount)
	}
}

// TestSGPackResidentCPFCapsAtOWCeiling: SGD 10,000 / month exceeds
// the OW ceiling of 7,400 → CPF base capped at 7,400. 20% × 7,400 =
// 1,480.00.
func TestSGPackResidentCPFCapsAtOWCeiling(t *testing.T) {
	pack, _ := Lookup("SG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(10000), monthPeriod())
	if len(out) != 1 {
		t.Fatalf("expected one deduction, got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(1480)) {
		t.Fatalf("CPF amount: got %s, want 1480.00", out[0].Amount)
	}
}

// TestSGPackCPFAgeTier_60To65 covers the 60-65 tier (11.5%
// employee). SGD 6,000 × 0.115 = 690.00.
func TestSGPackCPFAgeTier_60To65(t *testing.T) {
	pack, _ := Lookup("SG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 62,
	}, decimal.NewFromInt(6000), monthPeriod())
	if len(out) != 1 {
		t.Fatalf("expected one deduction, got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromFloat(690)) {
		t.Fatalf("CPF amount: got %s, want 690.00", out[0].Amount)
	}
}

// TestSGPackCPFAgeTier_70Plus covers the oldest tier (5%
// employee). SGD 5,000 × 0.05 = 250.00.
func TestSGPackCPFAgeTier_70Plus(t *testing.T) {
	pack, _ := Lookup("SG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 72,
	}, decimal.NewFromInt(5000), monthPeriod())
	if len(out) != 1 {
		t.Fatalf("expected one deduction, got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(250)) {
		t.Fatalf("CPF amount: got %s, want 250.00", out[0].Amount)
	}
}

// TestSGPackNonResidentFlatRate: a non-resident gets the 15%
// withholding under Income Tax Act s.40A, and no CPF.
func TestSGPackNonResidentFlatRate(t *testing.T) {
	pack, _ := Lookup("SG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(5000), monthPeriod())
	if len(out) != 1 || out[0].Code != "SG_NONRESIDENT_TAX" {
		t.Fatalf("unexpected deductions: %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(750)) {
		t.Fatalf("non-resident tax: got %s, want 750.00", out[0].Amount)
	}
}

// TestSGPackUnknownAgeFallsToHighestRate confirms an age == 0
// KRecord (the legacy default) maps to the youngest / highest
// rate tier. SGD 5,000 × 0.20 = 1,000.
func TestSGPackUnknownAgeFallsToHighestRate(t *testing.T) {
	pack, _ := Lookup("SG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 0,
	}, decimal.NewFromInt(5000), monthPeriod())
	if len(out) != 1 {
		t.Fatalf("expected one deduction, got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("CPF amount: got %s, want 1000.00 (highest tier)", out[0].Amount)
	}
}

// TestSGPackCPFBoundaryAges pins every CPF Board tier edge
// against the published schedule:
//
//	≤55         → 20.0%
//	above 55–60 → 17.0%
//	above 60–65 → 11.5%
//	above 65–70 →  7.5%
//	above 70    →  5.0%
//
// The test sweeps ages 55/56, 60/61, 65/66, 70/71 — the
// inclusive edges of each tier — and asserts the rate flips on
// the *correct* side of each boundary. The historical
// regression where UpperAge values were the inclusive bound
// itself (55, 60, 65, 70) under-withheld at every exact
// boundary age; this test would fail under that table.
func TestSGPackCPFBoundaryAges(t *testing.T) {
	pack, _ := Lookup("SG")
	gross := decimal.NewFromInt(5000)
	cases := []struct {
		name string
		age  int
		rate string // expected employee CPF rate as a decimal string
	}{
		{"age 55 → 20%", 55, "0.20"},
		{"age 56 → 17%", 56, "0.17"},
		{"age 60 → 17%", 60, "0.17"},
		{"age 61 → 11.5%", 61, "0.115"},
		{"age 65 → 11.5%", 65, "0.115"},
		{"age 66 → 7.5%", 66, "0.075"},
		{"age 70 → 7.5%", 70, "0.075"},
		{"age 71 → 5%", 71, "0.05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
				Resident: true, Age: tc.age,
			}, gross, monthPeriod())
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			if len(out) != 1 || out[0].Code != "SG_CPF_EMPLOYEE" {
				t.Fatalf("unexpected deductions: %+v", out)
			}
			want := gross.Mul(dec(tc.rate)).Round(2)
			if !out[0].Amount.Equal(want) {
				t.Fatalf("CPF amount at age %d: got %s, want %s (rate %s)", tc.age, out[0].Amount, want, tc.rate)
			}
		})
	}
}

// ----- Malaysia -----

// TestMYPackBracketAndEPF: MYR 6,000 / month, age 30, 31-day
// period. Hand-derivation:
//
//	periodFraction = 31 / 365.25 ≈ 0.084875
//	annualGross    = 6,000 / 0.084875 ≈ 70,693.55
//	bracket        = (70k, 100k, base 3700, rate 19%)
//	annualTax      = 3700 + (70693.55 - 70000) × 0.19 ≈ 3831.77
//	periodTax      = 3831.77 × 0.084875 ≈ 325.16
//	EPF            = 6000 × 11% = 660.00
//	SOCSO          = min(6000, 5000) × 0.5% = 25.00
//	EIS            = min(6000, 5000) × 0.2% = 10.00
func TestMYPackBracketAndEPF(t *testing.T) {
	pack, err := Lookup("MY")
	if err != nil {
		t.Fatalf("lookup MY: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(6000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	codes := indexByCode(out)
	pcb := codes["MY_PCB"]
	if pcb.LessThan(decimal.NewFromInt(300)) || pcb.GreaterThan(decimal.NewFromInt(360)) {
		t.Fatalf("MY_PCB %s outside expected 300-360 band", pcb)
	}
	if !codes["MY_EPF"].Equal(decimal.NewFromInt(660)) {
		t.Fatalf("MY_EPF: got %s, want 660.00", codes["MY_EPF"])
	}
	if !codes["MY_SOCSO"].Equal(decimal.NewFromInt(25)) {
		t.Fatalf("MY_SOCSO: got %s, want 25.00", codes["MY_SOCSO"])
	}
	if !codes["MY_EIS"].Equal(decimal.NewFromInt(10)) {
		t.Fatalf("MY_EIS: got %s, want 10.00", codes["MY_EIS"])
	}
}

// TestMYPackEPFRateChangesAt60 covers the second EPF rate path
// (5.5% for 60+). MYR 6,000 × 5.5% = 330.00.
func TestMYPackEPFRateChangesAt60(t *testing.T) {
	pack, _ := Lookup("MY")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 62,
	}, decimal.NewFromInt(6000), monthPeriod())
	codes := indexByCode(out)
	if !codes["MY_EPF"].Equal(decimal.NewFromInt(330)) {
		t.Fatalf("MY_EPF (age 62): got %s, want 330.00", codes["MY_EPF"])
	}
}

// TestMYPackEPFBoundaryAge59Vs60 pins the exact-age-60 boundary:
// the EPF Act 1991 Third Schedule reduces the employee rate from
// 11% to 5.5% the year the employee "attains the age of 60",
// meaning a 60-year-old already pays the lower rate. The pack
// uses `e.Age >= 60` so age 59 must still see 11% and age 60
// must already see 5.5%. MYR 6,000 × 11% = 660, × 5.5% = 330.
// Matches the SG CPF boundary-test precedent at every tier edge.
func TestMYPackEPFBoundaryAge59Vs60(t *testing.T) {
	pack, _ := Lookup("MY")
	out59, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 59,
	}, decimal.NewFromInt(6000), monthPeriod())
	out60, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 60,
	}, decimal.NewFromInt(6000), monthPeriod())
	if epf := indexByCode(out59)["MY_EPF"]; !epf.Equal(decimal.NewFromInt(660)) {
		t.Errorf("MY_EPF (age 59) = %s; want 660.00 (still 11%%)", epf)
	}
	if epf := indexByCode(out60)["MY_EPF"]; !epf.Equal(decimal.NewFromInt(330)) {
		t.Errorf("MY_EPF (age 60) = %s; want 330.00 (already 5.5%%)", epf)
	}
}

// TestMYPackBelowThresholdProducesNoPCB confirms the 0-5,000 / year
// resident bracket yields no PCB. MYR 300 / month annualises to
// ~3,535 which is below the threshold.
func TestMYPackBelowThresholdProducesNoPCB(t *testing.T) {
	pack, _ := Lookup("MY")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(300), monthPeriod())
	codes := indexByCode(out)
	if v, ok := codes["MY_PCB"]; ok {
		t.Fatalf("expected no MY_PCB under threshold; got %s", v)
	}
}

// TestMYPackInsurableCeilingCaps confirms a high earner pays
// SOCSO/EIS on the RM 5,000 ceiling, not full gross. SOCSO = 25,
// EIS = 10 regardless of gross.
func TestMYPackInsurableCeilingCaps(t *testing.T) {
	pack, _ := Lookup("MY")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if !codes["MY_SOCSO"].Equal(decimal.NewFromInt(25)) {
		t.Fatalf("MY_SOCSO at high gross: got %s, want 25.00", codes["MY_SOCSO"])
	}
	if !codes["MY_EIS"].Equal(decimal.NewFromInt(10)) {
		t.Fatalf("MY_EIS at high gross: got %s, want 10.00", codes["MY_EIS"])
	}
}

// TestMYPackNonResidentFlat30: Income Tax Act 1967 s.45 +
// LHDN public ruling — non-resident employees pay 30% flat on
// MY-sourced employment income with no EPF / SOCSO / EIS
// (each scheme is restricted to citizens / permanent
// residents). MYR 8,000 × 30% = 2,400.00 is the only line
// emitted.
func TestMYPackNonResidentFlat30(t *testing.T) {
	pack, _ := Lookup("MY")
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(8000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "MY_NONRESIDENT_TAX" {
		t.Fatalf("non-resident MY slip should emit only MY_NONRESIDENT_TAX; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(2400)) {
		t.Errorf("MY_NONRESIDENT_TAX = %s; want 2,400 (8,000 × 30%%)", out[0].Amount)
	}
}

// ----- Thailand -----

// TestTHPackPIT_NoDependents: THB 50,000 / month, 31-day period,
// no dependents. Hand-derivation:
//
//	periodFraction = 31 / 365.25 ≈ 0.084875
//	annualGross    = 50,000 / 0.084875 ≈ 589,113
//	stdDed         = min(50% × 589,113, 100,000) = 100,000
//	allowances     = 60,000 (personal only)
//	taxable        = 589,113 - 100,000 - 60,000 = 429,113
//	bracket        = (300k, 500k, base 7500, rate 10%)
//	annualTax      = 7500 + (429,113 - 300,000) × 0.10 ≈ 20,411
//	periodTax      ≈ 20,411 × 0.084875 ≈ 1,732.40
//	SSF            = min(50000, 15000) × 5% = 750
func TestTHPackPIT_NoDependents(t *testing.T) {
	pack, err := Lookup("TH")
	if err != nil {
		t.Fatalf("lookup TH: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	pit := codes["TH_PIT_WITHHOLDING"]
	if pit.LessThan(decimal.NewFromInt(1600)) || pit.GreaterThan(decimal.NewFromInt(1900)) {
		t.Fatalf("TH_PIT_WITHHOLDING %s outside expected 1600-1900 band", pit)
	}
	if !codes["TH_SSF"].Equal(decimal.NewFromInt(750)) {
		t.Fatalf("TH_SSF: got %s, want 750.00", codes["TH_SSF"])
	}
}

// TestTHPackPIT_TwoDependents shows the dependent allowance
// reduces taxable income. Two dependents → additional 60,000 in
// allowances → taxable drops to 369,113 (still in the 300-500k
// bracket) → annual tax = 7500 + 69113 × 0.10 = 14,411 → period
// tax ≈ 1,223. Band 1100-1350.
func TestTHPackPIT_TwoDependents(t *testing.T) {
	pack, _ := Lookup("TH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, NumDependents: 2,
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	pit := codes["TH_PIT_WITHHOLDING"]
	if pit.LessThan(decimal.NewFromInt(1100)) || pit.GreaterThan(decimal.NewFromInt(1350)) {
		t.Fatalf("TH_PIT_WITHHOLDING w/ 2 deps %s outside expected 1100-1350 band", pit)
	}
}

// TestTHPackDependentSanityCap pins the defense-in-depth cap on
// EmployeeInfo.NumDependents in th.go. The Thai Revenue Code
// imposes no statutory hard cap on declared dependents (unlike
// Indonesia's 3-dependent ceiling), but the pack clamps the
// declared count at thMaxDependents=20 so a wizard / payroll
// import bug sending NumDependents=10_000 can't drive taxable
// income to zero and silently skip an employee's PIT line.
//
// The assertion is *value equivalence at the cap*: a slip with
// NumDependents = thMaxDependents must produce the same period
// tax as a slip with NumDependents = thMaxDependents + 1,
// thMaxDependents × 50, or any larger value. THB 200,000 /
// month is chosen so that even 20 dependents leaves residual
// taxable income (so a small mis-clamp would visibly change
// the PIT line rather than zeroing it out by coincidence).
func TestTHPackDependentSanityCap(t *testing.T) {
	pack, _ := Lookup("TH")
	monthly := decimal.NewFromInt(200000)
	baseline, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, NumDependents: thMaxDependents,
	}, monthly, monthPeriod())
	baselinePIT := indexByCode(baseline)["TH_PIT_WITHHOLDING"]
	if !baselinePIT.IsPositive() {
		t.Fatalf("expected positive PIT at cap; got %s", baselinePIT)
	}
	for _, deps := range []int{thMaxDependents + 1, thMaxDependents * 2, thMaxDependents * 50} {
		got, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
			Resident: true, NumDependents: deps,
		}, monthly, monthPeriod())
		gotPIT := indexByCode(got)["TH_PIT_WITHHOLDING"]
		if !gotPIT.Equal(baselinePIT) {
			t.Fatalf("NumDependents=%d: PIT=%s, want %s (clamp at %d not honored)",
				deps, gotPIT, baselinePIT, thMaxDependents)
		}
	}
}

// TestTHPackBelowThresholdProducesNoPIT covers the 0-150k
// bracket. THB 10,000 / month annualises to ~117,800, allowances
// take it negative → no PIT.
func TestTHPackBelowThresholdProducesNoPIT(t *testing.T) {
	pack, _ := Lookup("TH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(10000), monthPeriod())
	codes := indexByCode(out)
	if v, ok := codes["TH_PIT_WITHHOLDING"]; ok {
		t.Fatalf("expected no PIT under threshold; got %s", v)
	}
	// SSF still applies though — 10,000 × 5% = 500.
	if !codes["TH_SSF"].Equal(decimal.NewFromInt(500)) {
		t.Fatalf("TH_SSF: got %s, want 500.00", codes["TH_SSF"])
	}
}

// TestTHPackNonResidentFlat15: Revenue Code s.50(1) + s.41 +
// Ministerial Regulation 126 — non-residents whose Thai-sourced
// employment income is taxed at source pay 15% flat with no
// SSF (SSF eligibility under SSA s.33 is restricted to
// permanent Thai employment). THB 80,000 × 15% = 12,000.00 is
// the only line emitted.
func TestTHPackNonResidentFlat15(t *testing.T) {
	pack, _ := Lookup("TH")
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(80000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "TH_NONRESIDENT_TAX" {
		t.Fatalf("non-resident TH slip should emit only TH_NONRESIDENT_TAX; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(12000)) {
		t.Errorf("TH_NONRESIDENT_TAX = %s; want 12,000 (80,000 × 15%%)", out[0].Amount)
	}
}

// ----- Indonesia -----

// TestIDPackPPh21_TK0: IDR 15,000,000 / month, 31-day period, no
// dependents. Hand-derivation:
//
//	periodFraction = 31 / 365.25 ≈ 0.084875
//	annualGross    = 15M / 0.084875 ≈ 176,733,884
//	PTKP TK/0      = 54,000,000
//	taxable        = 176,733,884 - 54,000,000 = 122,733,884
//	bracket        = (60M, 250M, base 3M, rate 15%)
//	annualTax      = 3,000,000 + (122,733,884 - 60,000,000) × 0.15
//	               ≈ 12,410,083
//	periodTax      ≈ 12,410,083 × 0.084875 ≈ 1,053,300
//
//	BPJS Kes  = min(15M, 12M) × 1%  = 120,000
//	BPJS JHT  = 15M × 2%             = 300,000
//	BPJS JP   = min(15M, 10,547,400) × 1% = 105,474
func TestIDPackPPh21_TK0(t *testing.T) {
	pack, err := Lookup("ID")
	if err != nil {
		t.Fatalf("lookup ID: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(15000000), monthPeriod())
	codes := indexByCode(out)
	pph := codes["ID_PPH21"]
	if pph.LessThan(decimal.NewFromInt(900000)) || pph.GreaterThan(decimal.NewFromInt(1200000)) {
		t.Fatalf("ID_PPH21 %s outside expected 900,000-1,200,000 band", pph)
	}
	if !codes["ID_BPJS_KES"].Equal(decimal.NewFromInt(120000)) {
		t.Fatalf("ID_BPJS_KES: got %s, want 120,000", codes["ID_BPJS_KES"])
	}
	if !codes["ID_BPJS_JHT"].Equal(decimal.NewFromInt(300000)) {
		t.Fatalf("ID_BPJS_JHT: got %s, want 300,000", codes["ID_BPJS_JHT"])
	}
	// JP cap: 10,547,400 × 1% = 105,474.
	if !codes["ID_BPJS_JP"].Equal(decimal.NewFromInt(105474)) {
		t.Fatalf("ID_BPJS_JP: got %s, want 105,474", codes["ID_BPJS_JP"])
	}
}

// TestIDPackPTKPDependents: each dependent adds 4.5M to PTKP, max
// 3. Two dependents → PTKP = 63M, taxable drops by 9M, tax drops
// by ~9M × 0.15 = 1,350,000 / year ≈ 114,580 / month.
func TestIDPackPTKPDependents(t *testing.T) {
	pack, _ := Lookup("ID")

	outTK0, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(15000000), monthPeriod())
	outTK2, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, NumDependents: 2,
	}, decimal.NewFromInt(15000000), monthPeriod())

	pphTK0 := indexByCode(outTK0)["ID_PPH21"]
	pphTK2 := indexByCode(outTK2)["ID_PPH21"]
	delta := pphTK0.Sub(pphTK2)
	// Expected drop: 9M × 15% × periodFraction = 9M × 0.15 ×
	// 31/365.25 ≈ 114,580. Band 100,000-130,000.
	if delta.LessThan(decimal.NewFromInt(100000)) || delta.GreaterThan(decimal.NewFromInt(130000)) {
		t.Fatalf("PTKP delta %s outside expected 100,000-130,000 band", delta)
	}
}

// TestIDPackBelowThreshold confirms a low gross produces no
// PPh21 line (PTKP exceeds annual gross).
func TestIDPackBelowThreshold(t *testing.T) {
	pack, _ := Lookup("ID")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(3000000), monthPeriod())
	codes := indexByCode(out)
	if v, ok := codes["ID_PPH21"]; ok {
		t.Fatalf("expected no PPh21 under PTKP; got %s", v)
	}
	// BPJS still accrues.
	if !codes["ID_BPJS_JHT"].Equal(decimal.NewFromInt(60000)) {
		t.Fatalf("ID_BPJS_JHT: got %s, want 60,000", codes["ID_BPJS_JHT"])
	}
}

// TestIDPackNonResidentFlat20: UU PPh art. 26 — non-resident
// individuals pay 20% flat on Indonesian-sourced employment
// income with no BPJS Ketenagakerjaan / Kesehatan (each scheme
// requires Indonesian citizenship or a KITAS-permitted long
// stay). IDR 25,000,000 × 20% = 5,000,000 is the only line
// emitted.
func TestIDPackNonResidentFlat20(t *testing.T) {
	pack, _ := Lookup("ID")
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(25000000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "ID_NONRESIDENT_TAX" {
		t.Fatalf("non-resident ID slip should emit only ID_NONRESIDENT_TAX; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(5000000)) {
		t.Errorf("ID_NONRESIDENT_TAX = %s; want 5,000,000 (25,000,000 × 20%%)", out[0].Amount)
	}
}

// ----- Registry assertions -----

// TestAPACPacksAreRegistered confirms all four new packs self-
// register and resolve through Lookup.
func TestAPACPacksAreRegistered(t *testing.T) {
	for _, code := range []string{"SG", "MY", "TH", "ID"} {
		if _, err := Lookup(code); err != nil {
			t.Errorf("Lookup(%q): %v", code, err)
		}
	}
}

// TestAPACPacksExposeEffectiveYear pins each pack's calibrated
// year. Bumps must be deliberate (and accompanied by bracket-table
// updates in the same PR).
func TestAPACPacksExposeEffectiveYear(t *testing.T) {
	cases := map[string]int{
		"SG": 2025,
		"MY": 2024,
		"TH": 2024,
		"ID": 2024,
	}
	for code, want := range cases {
		pack, err := Lookup(code)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", code, err)
		}
		if got := pack.EffectiveYear(); got != want {
			t.Errorf("%s.EffectiveYear() = %d; want %d", code, got, want)
		}
	}
}

// indexByCode reshapes a Deduction slice into a Code → Amount
// map for terse assertions in the per-pack tests above.
func indexByCode(in []Deduction) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(in))
	for _, d := range in {
		out[d.Code] = d.Amount
	}
	return out
}

// bracketRow is a typed projection of any per-pack bracket
// struct used by TestBracketTablesAreContiguous. The per-pack
// struct types (myBracket / thBracket / idBracket / ...) stay
// distinct so a future schedule change to one pack cannot
// cross-leak into another, but every walk function relies on
// the same shape (Floor / Top / Base / Rate) so we project the
// per-pack rows through this view and check the shared
// invariants in one place.
type bracketRow struct {
	floor decimal.Decimal
	top   decimal.Decimal
	base  decimal.Decimal
	rate  decimal.Decimal
}

// TestBracketTablesAreContiguous pins the two invariants every
// walk function relies on:
//
//  1. Top-contiguity — adjacent rows satisfy
//     `Top[i] == Floor[i+1]`, and the last row is open-ended
//     (`Top == 0`). The walk functions ignore Top at runtime
//     (they trigger on Floor ordering), so a typo in Top
//     cannot break a payroll run — but it does mean an
//     off-by-one in a copy-pasted table can silently sit in
//     production.
//
//  2. Base-consistency — adjacent rows satisfy
//     `Base[i+1] == Base[i] + (Floor[i+1] - Floor[i]) * Rate[i]`.
//     This is the *real* correctness invariant: the walk
//     resolves annual tax as `Base + (income - Floor) * Rate`
//     for the matched bracket, so a wrong Base produces a
//     wrong tax at every income in that bracket. The
//     contiguity check on its own would not catch a `Base`
//     transcription error.
//
// Together these fail the build if any pack's table drifts out
// of adjacency *or* loses its cumulative-tax monotonicity.
func TestBracketTablesAreContiguous(t *testing.T) {
	checkRows := func(t *testing.T, label string, rows []bracketRow) {
		t.Helper()
		for i := 0; i < len(rows)-1; i++ {
			cur, next := rows[i], rows[i+1]
			if !cur.top.Equal(next.floor) {
				t.Fatalf("%s brackets[%d].Top (%s) != brackets[%d].Floor (%s)",
					label, i, cur.top, i+1, next.floor)
			}
			want := cur.base.Add(next.floor.Sub(cur.floor).Mul(cur.rate))
			if !next.base.Equal(want) {
				t.Fatalf("%s brackets[%d].Base (%s) != Base[%d] + (Floor[%d]-Floor[%d])*Rate[%d] (= %s)",
					label, i+1, next.base, i, i+1, i, i, want)
			}
		}
		last := rows[len(rows)-1]
		if !last.top.IsZero() {
			t.Fatalf("%s last bracket Top should be 0 (open-ended), got %s", label, last.top)
		}
	}

	myRows := make([]bracketRow, len(myBracketsResident))
	for i, b := range myBracketsResident {
		myRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	thRows := make([]bracketRow, len(thBracketsResident))
	for i, b := range thBracketsResident {
		thRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	idRows := make([]bracketRow, len(idBracketsResident))
	for i, b := range idBracketsResident {
		idRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}

	t.Run("MY resident", func(t *testing.T) { checkRows(t, "MY", myRows) })
	t.Run("TH resident", func(t *testing.T) { checkRows(t, "TH", thRows) })
	t.Run("ID resident", func(t *testing.T) { checkRows(t, "ID", idRows) })
}
