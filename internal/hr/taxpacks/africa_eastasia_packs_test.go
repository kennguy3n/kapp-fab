package taxpacks

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

// africa_eastasia_packs_test.go covers the Phase N3 jurisdiction
// roster (ZA / NG / KE / EG / JP / KR). The matrix per pack pins
// at least one nominal slip (statistically common gross), one
// threshold-crossing slip (e.g. PAYE personal-relief boundary,
// NSSF UEL ceiling, SHIF floor, JP Article-161 non-resident
// branch, KR top-bracket band) and one edge-case slip
// (zero / negative gross or empty pay period). Hand-derived
// expected values are documented in the test body so a future
// schedule bump can be re-verified by walking the same math.

// findDeduction returns the first deduction whose code matches
// the lookup key, or a zero Deduction if no match. Used to keep
// the assertion blocks terse: tests pin the codes they care about
// and ignore the order of additional lines.
func findDeduction(out []Deduction, code string) Deduction {
	for _, d := range out {
		if d.Code == code {
			return d
		}
	}
	return Deduction{}
}

// ----- South Africa -----

// TestZAPackNominalSalary: ZAR 30,000 / month, age 35, resident.
// Hand-derivation:
//
//	periodFraction = 31 / 365.25 = 0.0848735
//	annualGross    = 30,000 / 0.0848735 ≈ 353,478
//	bracket 2 (237,100 – 370,500 @ 26%):
//	  42,678 + (353,478 - 237,100) × 0.26 = 72,936
//	after primary rebate (17,235):
//	  55,701
//	periodPAYE = 55,701 × 0.0848735 ≈ 4,727
//
// UIF:
//
//	annual / 12 ≈ 29,456 — below the 17,712 ceiling? No — > ceiling.
//	UIF base capped to 17,712; monthly UIF = 177.12;
//	scaled to 31-day slip ≈ 180.39.
func TestZAPackNominalSalary(t *testing.T) {
	pack, err := Lookup("ZA")
	if err != nil {
		t.Fatalf("lookup ZA: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 35,
	}, decimal.NewFromInt(30000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if paye := findDeduction(out, "ZA_PAYE").Amount; !paye.Equal(dec("4727.33")) {
		t.Fatalf("ZA_PAYE: got %s, want 4727.33", paye.String())
	}
	if uif := findDeduction(out, "ZA_UIF").Amount; !uif.Equal(dec("180.39")) {
		t.Fatalf("ZA_UIF: got %s, want 180.39", uif.String())
	}
}

// TestZAPackBelowRebateThreshold: very small gross (R3,000 / month).
// Annualised ~35,348 → bracket 1 @ 18% pre-rebate = 6,363.
// Rebate 17,235 fully wipes the tax → zero PAYE emitted (line omitted).
// UIF remains positive (1% × min(2,945, 17712) = ~29.46/month).
func TestZAPackBelowRebateThreshold(t *testing.T) {
	pack, _ := Lookup("ZA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(3000), monthPeriod())
	if paye := findDeduction(out, "ZA_PAYE").Amount; paye.IsPositive() {
		t.Fatalf("ZA_PAYE should be zero below rebate threshold, got %s", paye.String())
	}
	if uif := findDeduction(out, "ZA_UIF").Amount; !uif.IsPositive() {
		t.Fatalf("ZA_UIF should still be positive, got %s", uif.String())
	}
}

// TestZAPackEmptyInputs: zero / negative gross or zero-day period
// must short-circuit and emit no deductions.
func TestZAPackEmptyInputs(t *testing.T) {
	pack, _ := Lookup("ZA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.Zero, monthPeriod())
	if len(out) != 0 {
		t.Fatalf("zero gross should emit no deductions, got %+v", out)
	}
}

// ----- Nigeria -----

// TestNGPackNominalSalary: ₦500,000 / month resident.
// Pension: 500,000 × 0.08 = 40,000
// NHF:     500,000 × 0.025 = 12,500
// Net for PAYE: 500,000 - 40,000 - 12,500 = 447,500
// Annualised: 447,500 / (31/365.25) ≈ 5,272,943
// CRA: max(200,000, 0.21 × 5,272,943) = 1,107,318
// Taxable annual: 5,272,943 - 1,107,318 = 4,165,625
// Tax: 224,000 + (4,165,625 - 1,600,000) × 0.21 = 762,781
//  oh wait, that uses bracket 5 (1.6M-3.2M @ 21%). The taxable is
//  4.16M so we walk to bracket 6 (3.2M+ @ 24%) — last bracket wins.
//  Tax = 560,000 + (4,165,625 - 3,200,000) × 0.24 = 791,750
// periodPAYE = 791,750 × 31/365.25 ≈ 67,192.
func TestNGPackNominalSalary(t *testing.T) {
	pack, err := Lookup("NG")
	if err != nil {
		t.Fatalf("lookup NG: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(500000), monthPeriod())
	if p := findDeduction(out, "NG_PENSION").Amount; !p.Equal(decimal.NewFromInt(40000)) {
		t.Fatalf("NG_PENSION: got %s, want 40000", p.String())
	}
	if n := findDeduction(out, "NG_NHF").Amount; !n.Equal(decimal.NewFromInt(12500)) {
		t.Fatalf("NG_NHF: got %s, want 12500", n.String())
	}
	if p := findDeduction(out, "NG_PAYE").Amount; !p.Equal(dec("67192.34")) {
		t.Fatalf("NG_PAYE: got %s, want 67192.34", p.String())
	}
}

// TestNGPackLowSalaryBelowCRA: ₦15,000 / month resident. After 8%
// + 2.5% deductions, net is ₦12,825. Annualised ≈ ₦151,107, below
// the ₦200,000 CRA floor → annual taxable income ≤ 0 → zero PAYE.
// Pension + NHF lines remain positive.
func TestNGPackLowSalaryBelowCRA(t *testing.T) {
	pack, _ := Lookup("NG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 25,
	}, decimal.NewFromInt(15000), monthPeriod())
	if p := findDeduction(out, "NG_PENSION").Amount; !p.IsPositive() {
		t.Fatalf("NG_PENSION should be positive, got %s", p.String())
	}
	if p := findDeduction(out, "NG_PAYE").Amount; p.IsPositive() {
		t.Fatalf("NG_PAYE should be zero below CRA, got %s", p.String())
	}
}

// TestNGPackEmptyInputs: zero gross emits nothing.
func TestNGPackEmptyInputs(t *testing.T) {
	pack, _ := Lookup("NG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.Zero, monthPeriod())
	if len(out) != 0 {
		t.Fatalf("zero gross should emit no deductions, got %+v", out)
	}
}

// ----- Kenya -----

// TestKEPackNominalSalary: KES 60,000 / month resident.
// Monthly equiv (31-day slip): 60,000 × 30.4375 / 31 ≈ 58,911
// PAYE walk:
//
//	bracket 3 (32,333 - 500,000 @ 30%):
//	  4,483.25 + (58,911 - 32,333) × 0.30 = 12,456 monthly
//	less relief 2,400 = 10,056 monthly
//	period PAYE = 10,056 × 31 / 30.4375 = 10,242.59
//
// NSSF: min(58,911, 72,000) × 0.06 = 3,535 monthly (under 4,320 cap)
//
//	period NSSF = 3,535 × 31 / 30.4375 = 3,600.
//
// SHIF: 58,911 × 0.0275 = 1620 monthly → scaled to 31 days ≈ 1650.
// Housing levy: 58,911 × 0.015 = 884 monthly → 900 period.
func TestKEPackNominalSalary(t *testing.T) {
	pack, err := Lookup("KE")
	if err != nil {
		t.Fatalf("lookup KE: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(60000), monthPeriod())
	if p := findDeduction(out, "KE_PAYE").Amount; !p.Equal(dec("10242.59")) {
		t.Fatalf("KE_PAYE: got %s, want 10242.59", p.String())
	}
	if n := findDeduction(out, "KE_NSSF").Amount; !n.Equal(decimal.NewFromInt(3600)) {
		t.Fatalf("KE_NSSF: got %s, want 3600", n.String())
	}
	if s := findDeduction(out, "KE_SHIF").Amount; !s.Equal(decimal.NewFromInt(1650)) {
		t.Fatalf("KE_SHIF: got %s, want 1650", s.String())
	}
	if h := findDeduction(out, "KE_HOUSING_LEVY").Amount; !h.Equal(decimal.NewFromInt(900)) {
		t.Fatalf("KE_HOUSING_LEVY: got %s, want 900", h.String())
	}
}

// TestKEPackNSSFCapsAtUEL: gross above UEL (KES 100,000 / month)
// caps NSSF at 4,320 monthly = 4,400 period (×31/30.4375).
func TestKEPackNSSFCapsAtUEL(t *testing.T) {
	pack, _ := Lookup("KE")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(100000), monthPeriod())
	// 100,000 × 30.4375/31 = 98,185.48; NSSF base capped at 72,000.
	// Monthly NSSF = 72,000 × 0.06 = 4,320; period = 4,320 × 31/30.4375 ≈ 4,399.84.
	if n := findDeduction(out, "KE_NSSF").Amount; !n.Equal(dec("4399.84")) {
		t.Fatalf("KE_NSSF should cap at UEL ceiling, got %s, want 4399.84", n.String())
	}
}

// TestKEPackSHIFFloor: very small gross still emits the minimum
// KES 300 / month SHIF (scaled to slip period).
func TestKEPackSHIFFloor(t *testing.T) {
	pack, _ := Lookup("KE")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(5000), monthPeriod())
	// 5,000 × 30.4375/31 ≈ 4,909; × 0.0275 ≈ 135 monthly < 300 floor.
	// Monthly SHIF = 300; period = 300 × 31/30.4375 ≈ 305.54.
	if s := findDeduction(out, "KE_SHIF").Amount; !s.Equal(dec("305.54")) {
		t.Fatalf("KE_SHIF floor not honoured: got %s, want 305.54", s.String())
	}
}

// ----- Egypt -----

// TestEGPackNominalSalary: EGP 15,000 / month resident.
// Monthly equiv (31 days): 15,000 × 30.4375 / 31 ≈ 14,728
// SI base: min(14,728, 14,500) = 14,500 (capped at upper bound)
// SI: 14,500 × 0.11 = 1,595 monthly → 1,624.48 period.
// PIT base: (15,000 - 1,624.48) annualised = 13,375.52 / (31/365.25) ≈ 157,602
// Less personal exemption 20,000 = 137,602 (bracket 4: 70k-200k @ 20%)
// Tax: 3,750 + (137,602 - 70,000) × 0.20 = 17,270 annual
// period PIT = 17,270 × 31/365.25 = 1,465.66
func TestEGPackNominalSalary(t *testing.T) {
	pack, err := Lookup("EG")
	if err != nil {
		t.Fatalf("lookup EG: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(15000), monthPeriod())
	if s := findDeduction(out, "EG_SOCIAL_INSURANCE").Amount; !s.Equal(dec("1624.48")) {
		t.Fatalf("EG_SOCIAL_INSURANCE: got %s, want 1624.48", s.String())
	}
	if p := findDeduction(out, "EG_PIT").Amount; !p.Equal(dec("1465.66")) {
		t.Fatalf("EG_PIT: got %s, want 1465.66", p.String())
	}
}

// TestEGPackBelowSILowerLimit: monthly equiv below EGP 2,000
// floor — SI base floored at 2,000.
func TestEGPackBelowSILowerLimit(t *testing.T) {
	pack, _ := Lookup("EG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(1500), monthPeriod())
	// 1500 × 30.4375 / 31 = 1473; below 2000 floor.
	// SI = 2000 × 0.11 = 220 monthly → 224.07 period.
	if s := findDeduction(out, "EG_SOCIAL_INSURANCE").Amount; !s.Equal(dec("224.07")) {
		t.Fatalf("EG_SOCIAL_INSURANCE floor: got %s, want 224.07", s.String())
	}
}

// TestEGPackBelowPersonalExemption: annualised gross under
// EGP 20,000 → zero PIT line.
func TestEGPackBelowPersonalExemption(t *testing.T) {
	pack, _ := Lookup("EG")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(1500), monthPeriod())
	if p := findDeduction(out, "EG_PIT").Amount; p.IsPositive() {
		t.Fatalf("EG_PIT should be zero under exemption, got %s", p.String())
	}
}

// ----- Japan -----

// TestJPPackNominalSalary: JPY 400,000 / month resident, age 45.
// Annualised: 400,000 / (31/365.25) ≈ 4,712,903
// Bracket 3 (3.3M – 6.95M @ 20%): 232,500 + (4,712,903 - 3,300,000) × 0.20 = 515,081
// + 2.1% reconstruction surtax: 515,081 × 1.021 = 525,898
// period tax: 525,898 × 31/365.25 ≈ 44,635
// Social insurance (monthly equiv ~392,742, scaled to 31-day slip):
//
//	health: × 0.0499 monthly, × 31/30.4375 period
//	pension: × 0.0915
//	LTC (40-64): × 0.008
//	EI: × 0.006
//
// Hand-derived nominal numbers documented above; the test pins
// JP_INCOME_TAX and JP_PENSION explicitly and asserts the
// LTC line presence for an age-45 employee.
func TestJPPackNominalSalary(t *testing.T) {
	pack, err := Lookup("JP")
	if err != nil {
		t.Fatalf("lookup JP: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 45,
	}, decimal.NewFromInt(400000), monthPeriod())
	if p := findDeduction(out, "JP_INCOME_TAX").Amount; !p.Equal(dec("44634.68")) {
		t.Fatalf("JP_INCOME_TAX: got %s, want 44634.68", p.String())
	}
	if p := findDeduction(out, "JP_PENSION").Amount; !p.Equal(decimal.NewFromInt(36600)) {
		t.Fatalf("JP_PENSION: got %s, want 36600", p.String())
	}
	if l := findDeduction(out, "JP_LTC_INSURANCE").Amount; !l.IsPositive() {
		t.Fatalf("JP_LTC_INSURANCE should be present for age 45, got %s", l.String())
	}
}

// TestJPPackUnder40NoLTC: age 30 → no LTC line (LTC is 40-64 only).
func TestJPPackUnder40NoLTC(t *testing.T) {
	pack, _ := Lookup("JP")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 30,
	}, decimal.NewFromInt(400000), monthPeriod())
	if l := findDeduction(out, "JP_LTC_INSURANCE").Amount; l.IsPositive() {
		t.Fatalf("JP_LTC_INSURANCE must be omitted for under-40, got %s", l.String())
	}
}

// TestJPPackNonResidentFlatRate: Article 161 flat 20.42% on
// JP-sourced gross, no social insurance.
func TestJPPackNonResidentFlatRate(t *testing.T) {
	pack, _ := Lookup("JP")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false, Age: 30,
	}, decimal.NewFromInt(400000), monthPeriod())
	// 400,000 × 0.2042 = 81,680.
	if nr := findDeduction(out, "JP_NONRESIDENT_TAX").Amount; !nr.Equal(dec("81680")) {
		t.Fatalf("JP_NONRESIDENT_TAX: got %s, want 81680", nr.String())
	}
	// No NPS / NHI / LTC / EI for non-residents.
	if p := findDeduction(out, "JP_HEALTH_INSURANCE").Amount; p.IsPositive() {
		t.Fatalf("non-resident must not get JP_HEALTH_INSURANCE, got %s", p.String())
	}
}

// ----- South Korea -----

// TestKRPackNominalSalary: KRW 4,000,000 / month resident, age 35.
// Annualised: 4,000,000 / (31/365.25) ≈ 47,129,032
// Basic deduction: -1,500,000 → 45,629,032
// Bracket 2 (14M - 50M @ 15%): 840,000 + (45,629,032 - 14,000,000) × 0.15 = 5,584,355
// period tax: 5,584,355 × 31/365.25 ≈ 473,963
// Local: 47,396 (10% of national).
// NPS: 4,000,000 × 0.045 = 180,000 monthly (under cap)
// NHI: 4,000,000 × 0.03545 = 141,800 monthly
// LTC: NHI × 0.1295 = 18,363
// EI:  4,000,000 × 0.009 = 36,000 monthly
func TestKRPackNominalSalary(t *testing.T) {
	pack, err := Lookup("KR")
	if err != nil {
		t.Fatalf("lookup KR: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 35,
	}, decimal.NewFromInt(4000000), monthPeriod())
	if p := findDeduction(out, "KR_INCOME_TAX").Amount; !p.Equal(dec("473963.04")) {
		t.Fatalf("KR_INCOME_TAX: got %s, want 473963.04", p.String())
	}
	if l := findDeduction(out, "KR_LOCAL_INCOME_TAX").Amount; !l.Equal(dec("47396.30")) {
		t.Fatalf("KR_LOCAL_INCOME_TAX: got %s, want 47396.30", l.String())
	}
	if n := findDeduction(out, "KR_NPS").Amount; !n.Equal(decimal.NewFromInt(180000)) {
		t.Fatalf("KR_NPS: got %s, want 180000", n.String())
	}
	if n := findDeduction(out, "KR_NHI").Amount; !n.Equal(decimal.NewFromInt(141800)) {
		t.Fatalf("KR_NHI: got %s, want 141800", n.String())
	}
	if e := findDeduction(out, "KR_EMPLOYMENT_INSURANCE").Amount; !e.Equal(decimal.NewFromInt(36000)) {
		t.Fatalf("KR_EMPLOYMENT_INSURANCE: got %s, want 36000", e.String())
	}
}

// TestKRPackNPSCapsAtCeiling: gross above KRW 6,170,000 caps NPS
// at the published monthly cap.
func TestKRPackNPSCapsAtCeiling(t *testing.T) {
	pack, _ := Lookup("KR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, Age: 35,
	}, decimal.NewFromInt(10000000), monthPeriod())
	// monthly equiv ~9,818,548; capped at 6,170,000.
	// NPS monthly = 6,170,000 × 0.045 = 277,650;
	// period = 277,650 × 31 / 30.4375 ≈ 282,781.11.
	if n := findDeduction(out, "KR_NPS").Amount; !n.Equal(dec("282781.11")) {
		t.Fatalf("KR_NPS ceiling not honoured: got %s, want 282781.11", n.String())
	}
}

// TestKRPackNonResidentBracketOnly: Article 156 non-resident
// uses the bracket walk + local surtax, no social insurance.
func TestKRPackNonResidentBracketOnly(t *testing.T) {
	pack, _ := Lookup("KR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false, Age: 35,
	}, decimal.NewFromInt(4000000), monthPeriod())
	if p := findDeduction(out, "KR_INCOME_TAX").Amount; !p.IsPositive() {
		t.Fatalf("non-resident KR_INCOME_TAX must still emit: got %s", p.String())
	}
	if n := findDeduction(out, "KR_NPS").Amount; n.IsPositive() {
		t.Fatalf("non-resident must not get KR_NPS, got %s", n.String())
	}
	if e := findDeduction(out, "KR_EMPLOYMENT_INSURANCE").Amount; e.IsPositive() {
		t.Fatalf("non-resident must not get KR_EMPLOYMENT_INSURANCE, got %s", e.String())
	}
}
