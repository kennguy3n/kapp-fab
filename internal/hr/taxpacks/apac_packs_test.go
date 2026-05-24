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

// ----- Vietnam -----

// TestVNPackResidentNoDependents: a 30,000,000 VND / month slip
// with no dependents. Hand-derivation against the Article 22
// schedule:
//   monthFraction = 31 / 30.4375 = 1.018503
//   monthlyGross  = 30,000,000 / 1.018503 = 29,454,994
//   personalDed   = 11,000,000
//   taxable       = 18,454,994
//   bracket       = 18M-32M (base 1,950,000, rate 20%)
//   monthlyTax    = 1,950,000 + 0.20 × (18,454,994 - 18,000,000) = 2,040,999
//   periodTax     = 2,040,999 × 1.018503 ≈ 2,078,765
//
// SI/HI/UI on gross 30M (below all caps): 8% / 1.5% / 1%.
func TestVNPackResidentNoDependents(t *testing.T) {
	pack, err := Lookup("VN")
	if err != nil {
		t.Fatalf("lookup VN: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(30000000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	codes := indexByCode(out)
	if pit := codes["VN_PIT"]; pit.LessThan(decimal.NewFromInt(2000000)) || pit.GreaterThan(decimal.NewFromInt(2150000)) {
		t.Errorf("VN_PIT = %s; expected ~2,078,765 (band 2,000,000-2,150,000)", pit)
	}
	if si := codes["VN_SI"]; !si.Equal(decimal.NewFromInt(2400000)) {
		t.Errorf("VN_SI = %s; want 2,400,000", si)
	}
	if hi := codes["VN_HI"]; !hi.Equal(decimal.NewFromInt(450000)) {
		t.Errorf("VN_HI = %s; want 450,000", hi)
	}
	if ui := codes["VN_UI"]; !ui.Equal(decimal.NewFromInt(300000)) {
		t.Errorf("VN_UI = %s; want 300,000", ui)
	}
}

// TestVNPackWithDependents: same gross, two dependents → larger
// personal deduction (11M + 2 × 4.4M = 19.8M) drops taxable into
// the 5M-10M bracket and slashes PIT.
//   taxable = monthlyGross - 19.8M = 29,454,994 - 19,800,000 = 9,654,994
//   bracket = 5M-10M (base 250,000, rate 10%)
//   monthlyTax = 250,000 + 0.10 × (9,654,994 - 5,000,000) = 715,499
//   periodTax  = 715,499 × 1.018503 ≈ 728,743
func TestVNPackWithDependents(t *testing.T) {
	pack, _ := Lookup("VN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, NumDependents: 2,
	}, decimal.NewFromInt(30000000), monthPeriod())
	codes := indexByCode(out)
	if pit := codes["VN_PIT"]; pit.LessThan(decimal.NewFromInt(680000)) || pit.GreaterThan(decimal.NewFromInt(770000)) {
		t.Errorf("VN_PIT with 2 deps = %s; expected ~728,743 (band 680,000-770,000)", pit)
	}
}

// TestVNPackBelowDeductionFloor: a 10M VND slip falls under the
// personal deduction (11M) entirely → no PIT line, social
// insurance still accrues.
func TestVNPackBelowDeductionFloor(t *testing.T) {
	pack, _ := Lookup("VN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(10000000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["VN_PIT"]; ok {
		t.Fatalf("expected no VN_PIT under personal deduction floor; got %+v", codes)
	}
	if si := codes["VN_SI"]; !si.Equal(decimal.NewFromInt(800000)) {
		t.Errorf("VN_SI = %s; want 800,000", si)
	}
}

// TestVNPackNonResidentFlat20: a non-resident foreign individual
// (PIT Law art. 26) — present in Vietnam < 183 days in the tax
// year — pays a flat 20% on VN-sourced employment income and
// makes no SI / HI / UI contributions. 30,000,000 VND × 20% =
// 6,000,000 should be the only deduction line emitted.
func TestVNPackNonResidentFlat20(t *testing.T) {
	pack, _ := Lookup("VN")
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(30000000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "VN_NONRESIDENT_TAX" {
		t.Fatalf("non-resident VN slip should emit only VN_NONRESIDENT_TAX; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(6000000)) {
		t.Errorf("VN_NONRESIDENT_TAX = %s; want 6,000,000 (30,000,000 × 20%%)", out[0].Amount)
	}
}

// TestVNPackDependentCountClampedAtCeiling pins the defense-in-depth
// upper bound on NumDependents (vnMaxDependents = 20). A bug in the
// wizard or a payroll-import error could send a runaway dependent
// count; without the cap, an attacker / faulty integration could
// silently zero out the VN_PIT line by inflating the dependent
// deduction beyond gross. The cap ensures the dependent allowance
// stays within plausible bounds (max 20 × 4.4M = 88M, plus 11M
// personal = 99M VND total deduction at the cap).
//
// Slip: 200,000,000 VND / month, NumDependents = 10,000.
//   With cap:   deduction = 11M + 20×4.4M = 99M VND → taxable
//               monthly = 200M - 99M = 101M VND → progressive walk
//               yields a positive VN_PIT line.
//   Without:    deduction = 11M + 10000×4.4M = 44,011M VND → taxable
//               clamped to zero → VN_PIT silently suppressed.
func TestVNPackDependentCountClampedAtCeiling(t *testing.T) {
	pack, _ := Lookup("VN")
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, NumDependents: 10000,
	}, decimal.NewFromInt(200000000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	codes := indexByCode(out)
	pit, ok := codes["VN_PIT"]
	if !ok {
		t.Fatalf("VN_PIT line was silently suppressed by an uncapped "+
			"dependent count — expected the cap at %d to keep VN_PIT "+
			"positive; got %+v", vnMaxDependents, codes)
	}
	if !pit.IsPositive() {
		t.Fatalf("VN_PIT = %s; expected positive after dependent cap", pit)
	}
}

// ----- Philippines -----

// TestPHPackResidentWithholding: PHP 50,000 / month slip.
// Hand-derivation against BIR RMO 23-2023:
//   periodFraction = 31 / 365.25 = 0.084875
//   annualGross    = 50,000 / 0.084875 ≈ 589,113
//   bracket        = 400,000-800,000 (base 22,500, rate 20%)
//   annualTax      = 22,500 + 0.20 × (589,113 - 400,000) = 60,322.60
//   periodTax      = 60,322.60 × 0.084875 ≈ 5,119.88
//
// SSS:   50k > MSC 30k ceiling → 30,000 × 5% = 1,500.00
// PHIC:  in band (10k-100k) → 50,000 × 2.5% = 1,250.00
// HDMF:  capped at 10k → 10,000 × 2% = 200.00
func TestPHPackResidentWithholding(t *testing.T) {
	pack, err := Lookup("PH")
	if err != nil {
		t.Fatalf("lookup PH: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if wt := codes["PH_WITHHOLDING_TAX"]; wt.LessThan(decimal.NewFromInt(4900)) || wt.GreaterThan(decimal.NewFromInt(5300)) {
		t.Errorf("PH_WITHHOLDING_TAX = %s; expected ~5,119.88 (band 4,900-5,300)", wt)
	}
	if sss := codes["PH_SSS"]; !sss.Equal(decimal.NewFromInt(1500)) {
		t.Errorf("PH_SSS = %s; want 1,500", sss)
	}
	if phic := codes["PH_PHILHEALTH"]; !phic.Equal(decimal.NewFromInt(1250)) {
		t.Errorf("PH_PHILHEALTH = %s; want 1,250", phic)
	}
	if hdmf := codes["PH_PAGIBIG"]; !hdmf.Equal(decimal.NewFromInt(200)) {
		t.Errorf("PH_PAGIBIG = %s; want 200", hdmf)
	}
}

// TestPHPackBelowExemptThreshold: PHP 15,000 / month → annualises
// to ~176,734, well under the 250,000 exempt threshold. No
// withholding line; social contributions still emit.
func TestPHPackBelowExemptThreshold(t *testing.T) {
	pack, _ := Lookup("PH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(15000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["PH_WITHHOLDING_TAX"]; ok {
		t.Fatalf("expected no withholding tax under exempt threshold; got %+v", codes)
	}
	// PhilHealth: 15k × 2.5% = 375.00
	if phic := codes["PH_PHILHEALTH"]; !phic.Equal(decimal.NewFromFloat(375)) {
		t.Errorf("PH_PHILHEALTH = %s; want 375", phic)
	}
}

// TestPHPackNonResidentFlat25: NRANETB (non-resident alien not
// engaged in trade or business, NIRC s.25(B)) gets a flat 25% on
// gross PH-sourced income and *no* social contributions (SSS /
// PhilHealth / Pag-IBIG are limited to citizens, resident aliens
// employed in the Philippines, and NRAETBs). PHP 50,000 × 25% =
// 12,500.00 is the only line emitted.
func TestPHPackNonResidentFlat25(t *testing.T) {
	pack, _ := Lookup("PH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(50000), monthPeriod())
	if len(out) != 1 || out[0].Code != "PH_NRANETB_TAX" {
		t.Fatalf("non-resident PH slip should emit only PH_NRANETB_TAX; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(12500)) {
		t.Errorf("PH_NRANETB_TAX = %s; want 12,500 (50,000 × 25%%)", out[0].Amount)
	}
}

// ----- New Zealand -----

// TestNZPackResidentPAYEAndACC: NZD 6,000 / month slip, KiwiSaver
// at the default 3%. Hand-derivation against IR340 post-Budget-2024:
//   periodFraction = 31 / 365.25 = 0.084875
//   annualGross    = 6,000 / 0.084875 ≈ 70,694
//   bracket        = 53,500-78,100 (base 8,270.50, rate 30%)
//   annualTax      = 8,270.50 + 0.30 × (70,694 - 53,500) = 13,428.70
//   periodTax      = 13,428.70 × 0.084875 ≈ 1,139.76
// ACC: gross under period-pro-rated ceiling (142,283 × 0.084875 ≈
//   12,076) → accBase = 6,000 → 6,000 × 1.6% = 96.00
// KiwiSaver: 6,000 × 3% = 180.00
func TestNZPackResidentPAYEAndACC(t *testing.T) {
	pack, err := Lookup("NZ")
	if err != nil {
		t.Fatalf("lookup NZ: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, KiwiSaverRate: decimal.NewFromFloat(0.03),
	}, decimal.NewFromInt(6000), monthPeriod())
	codes := indexByCode(out)
	if paye := codes["NZ_PAYE"]; paye.LessThan(decimal.NewFromInt(1080)) || paye.GreaterThan(decimal.NewFromInt(1200)) {
		t.Errorf("NZ_PAYE = %s; expected ~1,139.76 (band 1,080-1,200)", paye)
	}
	if acc := codes["NZ_ACC"]; !acc.Equal(decimal.NewFromInt(96)) {
		t.Errorf("NZ_ACC = %s; want 96.00", acc)
	}
	if ks := codes["NZ_KIWISAVER"]; !ks.Equal(decimal.NewFromInt(180)) {
		t.Errorf("NZ_KIWISAVER = %s; want 180.00", ks)
	}
}

// TestNZPackKiwiSaverOptOut: KiwiSaverRate at zero → no KS line.
func TestNZPackKiwiSaverOptOut(t *testing.T) {
	pack, _ := Lookup("NZ")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, // KiwiSaverRate omitted = zero
	}, decimal.NewFromInt(6000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["NZ_KIWISAVER"]; ok {
		t.Fatalf("expected no KiwiSaver line at zero rate; got %+v", codes)
	}
}

// TestNZPackTopBracket: NZD 20,000 / month → annualises to
// ~235,649, into the 39% top bracket.
//   annualTax = 49,277.50 + 0.39 × (235,649 - 180,000) = 70,980.61
//   periodTax = 70,980.61 × 0.084875 ≈ 6,025.61
func TestNZPackTopBracket(t *testing.T) {
	pack, _ := Lookup("NZ")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(20000), monthPeriod())
	codes := indexByCode(out)
	if paye := codes["NZ_PAYE"]; paye.LessThan(decimal.NewFromInt(5800)) || paye.GreaterThan(decimal.NewFromInt(6300)) {
		t.Errorf("NZ_PAYE = %s; expected ~6,025.61 (band 5,800-6,300)", paye)
	}
}

// TestNZPackNonResidentSamePAYEAndACC pins the intentional NZ
// design: PAYE rates do NOT vary by tax-residency for income
// taxed via IRD payroll (Income Tax Act 2007 s.RD 5(1) — the
// schedule rates apply uniformly to all employees subject to
// PAYE, including non-resident workers on NZ payroll, casual
// agricultural workers, election-day workers, NRCT, etc.). ACC
// Earners' Levy similarly applies to all employees regardless
// of residency (ACC Act 2001 s.219). KiwiSaver remains opt-in
// via EmployeeInfo.KiwiSaverRate — non-residents (and any
// employee not enrolled) naturally get no KS deduction since
// the rate field is zero. This test pins the NZ pack's lack
// of a Resident branch as a deliberate design choice: a
// future regression that adds a non-resident flat-rate branch
// (mistaking NZ for AU's foreign-resident schedule) would
// break this test.
func TestNZPackNonResidentSamePAYEAndACC(t *testing.T) {
	pack, _ := Lookup("NZ")
	resOut, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(6000), monthPeriod())
	nrOut, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(6000), monthPeriod())
	resCodes := indexByCode(resOut)
	nrCodes := indexByCode(nrOut)
	if !resCodes["NZ_PAYE"].Equal(nrCodes["NZ_PAYE"]) {
		t.Errorf("NZ_PAYE differs by residency: resident=%s non-resident=%s; "+
			"expected the same rate per Income Tax Act 2007 s.RD 5(1)",
			resCodes["NZ_PAYE"], nrCodes["NZ_PAYE"])
	}
	if !resCodes["NZ_ACC"].Equal(nrCodes["NZ_ACC"]) {
		t.Errorf("NZ_ACC differs by residency: resident=%s non-resident=%s; "+
			"expected the same levy per ACC Act 2001 s.219",
			resCodes["NZ_ACC"], nrCodes["NZ_ACC"])
	}
	if _, ok := nrCodes["NZ_KIWISAVER"]; ok {
		t.Errorf("non-resident slip emitted NZ_KIWISAVER without an opt-in "+
			"KiwiSaverRate; codes=%+v", nrCodes)
	}
}

// ----- India -----

// TestINPackNewRegimeMidBracket: INR 100,000 / month, no state
// (no PT). Hand-derivation against Finance Act 2024:
//   periodFraction = 31 / 365.25 = 0.084875
//   annualGross    = 100,000 / 0.084875 ≈ 1,178,226
//   taxableAnnual  = 1,178,226 - 75,000 = 1,103,226
//   bracket        = 10L-12L (base 50,000, rate 15%)
//   annualTax      = 50,000 + 0.15 × (1,103,226 - 1,000,000) = 65,483.90
//   87A: taxableAnnual > 7L, no rebate
//   periodTax      = 65,483.90 × 0.084875 ≈ 5,558.20
//
// EPF: min(100k, 15k) × 12% = 1,800.00
// ESI: gross > 21k → no ESI
// No PT (PermitType empty)
func TestINPackNewRegimeMidBracket(t *testing.T) {
	pack, err := Lookup("IN")
	if err != nil {
		t.Fatalf("lookup IN: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(100000), monthPeriod())
	codes := indexByCode(out)
	if tds := codes["IN_TDS"]; tds.LessThan(decimal.NewFromInt(5300)) || tds.GreaterThan(decimal.NewFromInt(5800)) {
		t.Errorf("IN_TDS = %s; expected ~5,558.20 (band 5,300-5,800)", tds)
	}
	if epf := codes["IN_EPF"]; !epf.Equal(decimal.NewFromInt(1800)) {
		t.Errorf("IN_EPF = %s; want 1,800", epf)
	}
	if _, ok := codes["IN_ESI"]; ok {
		t.Errorf("ESI should not apply above 21k threshold; got %+v", codes)
	}
	if _, ok := codes["IN_PT"]; ok {
		t.Errorf("PT should not apply without state; got %+v", codes)
	}
}

// TestINPackNewRegime87ARebate: INR 50,000 / month → 87A wipes
// out TDS entirely (annual taxable 514k ≤ 700k AND tax 10,705.65 ≤
// 25,000). EPF still 1,800; ESI still skipped (50k > 21k floor).
func TestINPackNewRegime87ARebate(t *testing.T) {
	pack, _ := Lookup("IN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["IN_TDS"]; ok {
		t.Fatalf("expected no IN_TDS under 87A rebate; got %+v", codes)
	}
	if epf := codes["IN_EPF"]; !epf.Equal(decimal.NewFromInt(1800)) {
		t.Errorf("IN_EPF = %s; want 1,800", epf)
	}
}

// TestINPackNewRegime87AMarginalRelief covers the proviso to
// s.87A added by Finance Act 2023: when taxable income marginally
// exceeds ₹7,00,000, the tax payable is capped at the excess over
// the limit, preventing the historical cliff where ₹7,00,001
// produced ~₹25,000 of tax. Slip used:
//
//	monthly        = 66,000
//	periodFraction = 31 / 365.25 ≈ 0.084875
//	annualGross    = 66,000 / 0.084875 ≈ 777,629
//	taxableAnnual  = 777,629 - 75,000 = 702,629
//	bracket        = 7L-10L (base 20,000, rate 10%)
//	annualTax_pre  = 20,000 + 10% × (702,629 - 700,000) = 20,263
//	excess         = 702,629 - 700,000 = 2,629
//	annualTax_post = min(20,263, 2,629) = 2,629 — marginal relief active
//	periodTax      = 2,629 × 0.084875 ≈ 223
//
// Without the proviso the slip would over-withhold by ~₹1,500 /
// month, which is the cliff the proviso was enacted to prevent.
func TestINPackNewRegime87AMarginalRelief(t *testing.T) {
	pack, _ := Lookup("IN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(66000), monthPeriod())
	codes := indexByCode(out)
	tds, ok := codes["IN_TDS"]
	if !ok {
		t.Fatalf("expected IN_TDS with marginal relief active; got %+v", codes)
	}
	// Marginal relief band: tax ≈ excess × periodFraction ≈ 223.
	// The pre-relief amount would be ~1,720; assert we land in
	// the marginal-relief band, not the pre-relief band.
	if tds.LessThan(decimal.NewFromInt(150)) || tds.GreaterThan(decimal.NewFromInt(300)) {
		t.Errorf("IN_TDS = %s; expected ~223 marginal relief (band 150-300, pre-relief would be ~1,720)", tds)
	}
}

// TestINPackNewRegime87AAboveBreakeven: at high enough income the
// marginal-relief cap is no longer the binding constraint and the
// bracket-walk result is used. INR 100,000 / month resolves to
// taxable ≈ ₹1,103,226 → tax ≈ ₹65,484. Excess = 403,226; since
// 65,484 < 403,226 the marginal relief does not apply and the
// pre-relief tax is preserved. This is the same slip as
// TestINPackNewRegimeMidBracket, but the assertion here pins the
// "relief does not over-relieve high earners" invariant explicitly.
func TestINPackNewRegime87AAboveBreakeven(t *testing.T) {
	pack, _ := Lookup("IN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(100000), monthPeriod())
	codes := indexByCode(out)
	tds := codes["IN_TDS"]
	// Pre-relief tax ≈ 5,558; if relief mistakenly clamped here
	// we would see a number close to 100,000 - 775,000 × periodFraction
	// ≈ 34,224 in periodTax, which is wildly off. Assert the
	// bracket-walk result survives.
	if tds.LessThan(decimal.NewFromInt(5300)) || tds.GreaterThan(decimal.NewFromInt(5800)) {
		t.Errorf("IN_TDS = %s; expected ~5,558 (bracket walk, relief inactive)", tds)
	}
}

// TestINPackESIBelowThreshold: INR 15,000 / month → ESI applies
// (≤ 21k floor) at 0.75%. TDS is zero (under 87A and under the
// first taxable bracket).
//   ESI = 15,000 × 0.75% = 112.50
//   EPF = min(15k, 15k) × 12% = 1,800
func TestINPackESIBelowThreshold(t *testing.T) {
	pack, _ := Lookup("IN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, decimal.NewFromInt(15000), monthPeriod())
	codes := indexByCode(out)
	if esi := codes["IN_ESI"]; !esi.Equal(decimal.NewFromFloat(112.5)) {
		t.Errorf("IN_ESI = %s; want 112.50", esi)
	}
	if epf := codes["IN_EPF"]; !epf.Equal(decimal.NewFromInt(1800)) {
		t.Errorf("IN_EPF = %s; want 1,800", epf)
	}
}

// TestINPackMaharashtraPT: state "MH" picks up the ₹200 / month
// professional-tax line for gross > ₹10,000.
func TestINPackMaharashtraPT(t *testing.T) {
	pack, _ := Lookup("IN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, PermitType: "MH",
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if pt := codes["IN_PT"]; !pt.Equal(decimal.NewFromInt(200)) {
		t.Errorf("IN_PT (MH, Jan) = %s; want 200", pt)
	}
}

// TestINPackMaharashtraPTFebruary: February in Maharashtra adds
// the ₹100 catch-up to total ₹300 (annual maximum ₹2,500).
func TestINPackMaharashtraPTFebruary(t *testing.T) {
	pack, _ := Lookup("IN")
	febPeriod := PayPeriod{
		Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC),
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, PermitType: "MH",
	}, decimal.NewFromInt(50000), febPeriod)
	codes := indexByCode(out)
	if pt := codes["IN_PT"]; !pt.Equal(decimal.NewFromInt(300)) {
		t.Errorf("IN_PT (MH, Feb) = %s; want 300", pt)
	}
}

// TestINPackUnknownRegimeDefaultsNew pins the defense-in-depth
// fallback: any non-"old" TaxRegime value (typos, garbage,
// future codes the wizard doesn't sanitise) must collapse to
// the new regime so the pack never silently zero-outs TDS. The
// resident-IN slip emits the same IN_TDS line whether
// TaxRegime is "", "new", or junk like "nwe" / "auto".
func TestINPackUnknownRegimeDefaultsNew(t *testing.T) {
	pack, _ := Lookup("IN")
	want, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, TaxRegime: "new",
	}, decimal.NewFromInt(100000), monthPeriod())
	wantTDS := indexByCode(want)["IN_TDS"]
	if !wantTDS.IsPositive() {
		t.Fatalf("baseline IN_TDS should be positive at 100k/month gross; got %s", wantTDS)
	}
	for _, regime := range []string{"", "nwe", "auto", "garbage", "NEW", " new "} {
		got, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
			Resident: true, TaxRegime: regime,
		}, decimal.NewFromInt(100000), monthPeriod())
		gotTDS := indexByCode(got)["IN_TDS"]
		if !gotTDS.Equal(wantTDS) {
			t.Errorf("TaxRegime=%q: IN_TDS = %s; want %s (must fall back to new regime)", regime, gotTDS, wantTDS)
		}
	}
	// "old" stays inert until PR-2c+ wires Form 10-IEA.
	got, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, TaxRegime: "old",
	}, decimal.NewFromInt(100000), monthPeriod())
	if _, ok := indexByCode(got)["IN_TDS"]; ok {
		t.Errorf("TaxRegime=old should remain inert (no IN_TDS) until 10-IEA wired; got %+v", got)
	}
}

// TestINPackNonResidentNo87ARebate pins the s.87A residency gate:
// s.87A explicitly limits the rebate to "an individual resident
// in India". A resident at the rebate envelope (≤ ₹7L taxable)
// has IN_TDS zeroed out; an otherwise identical non-resident
// pays the full bracket-walk tax (no rebate, no marginal-relief
// proviso). Standard deduction stays — s.16(ia) has no residency
// restriction.
//
// Test vector: monthly gross ₹58,400 on a 31-day period →
// annual ₹58,400 × 365.25 / 31 ≈ ₹688,082.58 → minus ₹75,000
// standard deduction = ₹613,082.58 taxable annual (under the
// ₹7L rebate envelope). The bracket walk yields ₹15,654.13
// (5% × ₹313,082.58), which is ≤ ₹25,000 so the resident gets
// full rebate (IN_TDS = 0). The non-resident pays the full
// ₹15,654.13 × 31 / 365.25 ≈ ₹1,328.62 for the period.
func TestINPackNonResidentNo87ARebate(t *testing.T) {
	pack, _ := Lookup("IN")
	gross := decimal.NewFromInt(58400)

	// Resident — 87A wipes IN_TDS.
	res, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
	}, gross, monthPeriod())
	if v, ok := indexByCode(res)["IN_TDS"]; ok {
		t.Errorf("resident at sub-7L taxable: expected 87A to zero IN_TDS, got %s", v)
	}

	// Non-resident — same gross, no 87A, must pay positive TDS.
	nr, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, gross, monthPeriod())
	nrTDS := indexByCode(nr)["IN_TDS"]
	if !nrTDS.IsPositive() {
		t.Fatalf("non-resident at 58,400/month gross: expected positive IN_TDS (87A doesn't apply), got %s", nrTDS)
	}
	// Sanity-check the figure against the hand-derived ₹1,328.62.
	// Tolerance ±2 paise covers prorate rounding noise.
	expected := decimal.NewFromFloat(1328.62)
	diff := nrTDS.Sub(expected).Abs()
	if diff.GreaterThan(decimal.NewFromFloat(0.02)) {
		t.Errorf("non-resident IN_TDS = %s; want ≈ %s (±0.02)", nrTDS, expected)
	}

	// Pin the statutorily-correct carry-through: EPF (Para 26A)
	// and ESI (s.2(9)) apply to all employees on Indian payroll
	// regardless of residency status. The earlier IN pack
	// implementation already gets this right because the
	// EPF/ESI/PT branches are unguarded; this assertion locks
	// the behaviour so a future refactor that adds a non-resident
	// early-return (mirroring the VN/PH pattern) doesn't
	// silently drop the social-security lines.
	nrCodes := indexByCode(nr)
	if epf := nrCodes["IN_EPF"]; !epf.IsPositive() {
		t.Errorf("non-resident IN_EPF = %s; want positive (Para 26A applies regardless of residency)", epf)
	}
	// At gross ₹58,400 (> ₹21,000 ESI ceiling) ESI should NOT
	// appear — the threshold is the gating factor, not residency.
	if v, ok := nrCodes["IN_ESI"]; ok {
		t.Errorf("non-resident IN_ESI = %s; want absent at gross > 21k (ESI threshold gates by wage, not residency)", v)
	}
}

// TestINPackNonResidentBelowESIThresholdGetsESI complements
// TestINPackNonResidentNo87ARebate by pinning the other half of
// the EPF/ESI carry-through: a non-resident earning ≤ ₹21,000 /
// month must still receive IN_ESI because s.2(9) of the ESI Act
// gates on wages, not on residency status.
func TestINPackNonResidentBelowESIThresholdGetsESI(t *testing.T) {
	pack, _ := Lookup("IN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false,
	}, decimal.NewFromInt(15000), monthPeriod())
	codes := indexByCode(out)
	// 15,000 × 0.75% = 112.50
	if esi := codes["IN_ESI"]; !esi.Equal(decimal.NewFromFloat(112.50)) {
		t.Errorf("non-resident IN_ESI at ₹15,000/month: got %s; want 112.50", esi)
	}
	// EPF at ₹15,000 → 15,000 × 12% = 1,800.00 (the cap is hit
	// exactly at the ceiling).
	if epf := codes["IN_EPF"]; !epf.Equal(decimal.NewFromInt(1800)) {
		t.Errorf("non-resident IN_EPF at ₹15,000/month: got %s; want 1800.00", epf)
	}
}

// ----- Registry assertions -----

// TestAPACPacksAreRegistered confirms all eight APAC packs self-
// register and resolve through Lookup.
func TestAPACPacksAreRegistered(t *testing.T) {
	for _, code := range []string{"SG", "MY", "TH", "ID", "VN", "PH", "NZ", "IN"} {
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
		"VN": 2024,
		"PH": 2024,
		"NZ": 2024,
		"IN": 2024,
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
	vnRows := make([]bracketRow, len(vnBracketsResident))
	for i, b := range vnBracketsResident {
		vnRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	phRows := make([]bracketRow, len(phBracketsResident))
	for i, b := range phBracketsResident {
		phRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	nzRows := make([]bracketRow, len(nzBracketsResident))
	for i, b := range nzBracketsResident {
		nzRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	inRows := make([]bracketRow, len(inBracketsNewRegime))
	for i, b := range inBracketsNewRegime {
		inRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}

	t.Run("MY resident", func(t *testing.T) { checkRows(t, "MY", myRows) })
	t.Run("TH resident", func(t *testing.T) { checkRows(t, "TH", thRows) })
	t.Run("ID resident", func(t *testing.T) { checkRows(t, "ID", idRows) })
	t.Run("VN resident", func(t *testing.T) { checkRows(t, "VN", vnRows) })
	t.Run("PH resident", func(t *testing.T) { checkRows(t, "PH", phRows) })
	t.Run("NZ resident", func(t *testing.T) { checkRows(t, "NZ", nzRows) })
	t.Run("IN new regime", func(t *testing.T) { checkRows(t, "IN", inRows) })
}
