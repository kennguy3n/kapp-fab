package taxpacks

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

// =====================================================================
// Session 14 — Expanded tax packs (MENA non-GCC + West Africa).
//
// Covers Jordan (JO), Lebanon (LB), Morocco (MA), Tunisia (TN) and
// Ghana (GH). All hand-derived expected values use the shared
// monthPeriod() helper (Jan 1-31 2026, 31 days). Annualised packs
// use periodFraction = 31 / 365.25 = 0.08487337..., annualGross =
// gross / periodFraction, walk the annual schedule, then prorate
// periodTax = annualTax × periodFraction (2dp). Ghana applies its
// genuinely-monthly PAYE table directly to the slip gross.
// =====================================================================

// ----- Jordan (annualised income tax + 1% national contribution + SSC) -----

// TestJOPackNominal: JOD 1,000 / month. annualGross ≈ 11,768; after
// the 9,000 personal exemption taxable ≈ 2,768 in the 5% band →
// annualTax ≈ 138 + 1% national contribution ≈ 28 ≈ 167; periodTax =
// 14.17. SSC: min(1,000, 3,484) × 7.5% = 75.00.
func TestJOPackNominal(t *testing.T) {
	pack, err := Lookup("JO")
	if err != nil {
		t.Fatalf("lookup JO: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(1000), monthPeriod())
	if d := findDeduction(out, "JO_INCOME_TAX"); !d.Amount.Equal(decimal.RequireFromString("14.17")) {
		t.Fatalf("income tax: got %s, want 14.17", d.Amount)
	}
	if d := findDeduction(out, "JO_SSC"); !d.Amount.Equal(decimal.NewFromInt(75)) {
		t.Fatalf("SSC: got %s, want 75.00", d.Amount)
	}
}

// TestJOPackFamilyExemption: JOD 5,000 / month with a dependent adds
// the 9,000 family exemption on top of the 9,000 personal exemption.
// annualGross ≈ 58,911; taxable ≈ 40,911 → income tax + national
// contribution periodTax = 690.61. SSC caps at the 3,484 subscription
// ceiling → 3,484 × 7.5% = 261.30.
func TestJOPackFamilyExemption(t *testing.T) {
	pack, _ := Lookup("JO")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true, NumDependents: 3},
		decimal.NewFromInt(5000), monthPeriod())
	if d := findDeduction(out, "JO_INCOME_TAX"); !d.Amount.Equal(decimal.RequireFromString("690.61")) {
		t.Fatalf("income tax: got %s, want 690.61", d.Amount)
	}
	if d := findDeduction(out, "JO_SSC"); !d.Amount.Equal(decimal.RequireFromString("261.30")) {
		t.Fatalf("SSC (capped): got %s, want 261.30", d.Amount)
	}
}

// TestJOPackZeroGross: empty-input edge case.
func TestJOPackZeroGross(t *testing.T) {
	pack, _ := Lookup("JO")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.Zero, monthPeriod())
	if len(out) != 0 {
		t.Fatalf("zero gross should yield no deductions, got %+v", out)
	}
}

// ----- Lebanon (annualised R10 payroll tax + NSSF sickness/maternity) -----

// TestLBPackNominal: LBP 100,000,000 / month. annualGross ≈
// 1,177,810,267; after the 450,000,000 personal exemption taxable ≈
// 727,810,267 → R10 periodTax = 3,409,856.26. NSSF: min(gross,
// 90,000,000) × 3% = 2,700,000.
func TestLBPackNominal(t *testing.T) {
	pack, err := Lookup("LB")
	if err != nil {
		t.Fatalf("lookup LB: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(100000000), monthPeriod())
	if d := findDeduction(out, "LB_INCOME_TAX"); !d.Amount.Equal(decimal.RequireFromString("3409856.26")) {
		t.Fatalf("income tax: got %s, want 3409856.26", d.Amount)
	}
	if d := findDeduction(out, "LB_NSSF"); !d.Amount.Equal(decimal.NewFromInt(2700000)) {
		t.Fatalf("NSSF (capped): got %s, want 2700000.00", d.Amount)
	}
}

// TestLBPackBelowExemption: a small slip whose annualised gross is
// under the 450,000,000 personal exemption yields no income tax, but
// the NSSF contribution still applies.
func TestLBPackBelowExemption(t *testing.T) {
	pack, _ := Lookup("LB")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(30000000), monthPeriod())
	if d := findDeduction(out, "LB_INCOME_TAX"); !d.Amount.IsZero() {
		t.Fatalf("expected no income tax below exemption, got %s", d.Amount)
	}
	if d := findDeduction(out, "LB_NSSF"); !d.Amount.Equal(decimal.NewFromInt(900000)) {
		t.Fatalf("NSSF: got %s, want 900000.00", d.Amount)
	}
}

// TestLBPackZeroGross: empty-input edge case.
func TestLBPackZeroGross(t *testing.T) {
	pack, _ := Lookup("LB")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.Zero, monthPeriod())
	if len(out) != 0 {
		t.Fatalf("zero gross should yield no deductions, got %+v", out)
	}
}

// ----- Morocco (annualised IR + professional deduction + CNSS + AMO) -----

// TestMAPackNominal: MAD 6,000 / month. annualGross ≈ 70,694; 20%
// professional deduction (capped 30,000) = 14,139; taxable ≈ 56,555
// in the 10% band → IR periodTax = 140.51. CNSS: min(6,000, 6,000) ×
// 4.48% = 268.80. AMO: 6,000 × 2.26% = 135.60.
func TestMAPackNominal(t *testing.T) {
	pack, err := Lookup("MA")
	if err != nil {
		t.Fatalf("lookup MA: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(6000), monthPeriod())
	if d := findDeduction(out, "MA_IR"); !d.Amount.Equal(decimal.RequireFromString("140.51")) {
		t.Fatalf("IR: got %s, want 140.51", d.Amount)
	}
	if d := findDeduction(out, "MA_CNSS"); !d.Amount.Equal(decimal.RequireFromString("268.80")) {
		t.Fatalf("CNSS: got %s, want 268.80", d.Amount)
	}
	if d := findDeduction(out, "MA_AMO"); !d.Amount.Equal(decimal.RequireFromString("135.60")) {
		t.Fatalf("AMO: got %s, want 135.60", d.Amount)
	}
}

// TestMAPackFamilyReduction: MAD 20,000 / month with three dependents
// applies a 3 × 500 = 1,500 annual family tax reduction, lowering IR
// to 4,005.07 (vs 4,132.38 with no dependents). CNSS still caps at
// 6,000 → 268.80; AMO: 20,000 × 2.26% = 452.00.
func TestMAPackFamilyReduction(t *testing.T) {
	pack, _ := Lookup("MA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true, NumDependents: 3},
		decimal.NewFromInt(20000), monthPeriod())
	if d := findDeduction(out, "MA_IR"); !d.Amount.Equal(decimal.RequireFromString("4005.07")) {
		t.Fatalf("IR with deps: got %s, want 4005.07", d.Amount)
	}
	if d := findDeduction(out, "MA_CNSS"); !d.Amount.Equal(decimal.RequireFromString("268.80")) {
		t.Fatalf("CNSS: got %s, want 268.80", d.Amount)
	}
	if d := findDeduction(out, "MA_AMO"); !d.Amount.Equal(decimal.NewFromInt(452)) {
		t.Fatalf("AMO: got %s, want 452.00", d.Amount)
	}
}

// TestMAPackZeroGross: empty-input edge case.
func TestMAPackZeroGross(t *testing.T) {
	pack, _ := Lookup("MA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.Zero, monthPeriod())
	if len(out) != 0 {
		t.Fatalf("zero gross should yield no deductions, got %+v", out)
	}
}

// ----- Tunisia (annualised IRPP, net of CNSS + professional deduction) -----

// TestTNPackNominal: TND 2,500 / month. CNSS: 2,500 × 9.18% = 229.50.
// annualGross ≈ 29,456; less annualised CNSS (≈ 2,704) and the 10%
// professional deduction (capped 2,000) → taxable ≈ 24,751 → IRPP
// periodTax = 396.82.
func TestTNPackNominal(t *testing.T) {
	pack, err := Lookup("TN")
	if err != nil {
		t.Fatalf("lookup TN: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(2500), monthPeriod())
	if d := findDeduction(out, "TN_IRPP"); !d.Amount.Equal(decimal.RequireFromString("396.82")) {
		t.Fatalf("IRPP: got %s, want 396.82", d.Amount)
	}
	if d := findDeduction(out, "TN_CNSS"); !d.Amount.Equal(decimal.RequireFromString("229.50")) {
		t.Fatalf("CNSS: got %s, want 229.50", d.Amount)
	}
}

// TestTNPackZeroGross: empty-input edge case.
func TestTNPackZeroGross(t *testing.T) {
	pack, _ := Lookup("TN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.Zero, monthPeriod())
	if len(out) != 0 {
		t.Fatalf("zero gross should yield no deductions, got %+v", out)
	}
}

// ----- Ghana (monthly PAYE table + SSNIT first tier) -----

// TestGHPackNominal: GHS 1,000 / month. SSNIT: 1,000 × 5.5% = 55.00.
// Chargeable = 945 in the 730–3,896.67 band (17.5%): 18.5 + (945 -
// 730) × 17.5% = 56.13.
func TestGHPackNominal(t *testing.T) {
	pack, err := Lookup("GH")
	if err != nil {
		t.Fatalf("lookup GH: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(1000), monthPeriod())
	if d := findDeduction(out, "GH_SSNIT"); !d.Amount.Equal(decimal.NewFromInt(55)) {
		t.Fatalf("SSNIT: got %s, want 55.00", d.Amount)
	}
	if d := findDeduction(out, "GH_PAYE"); !d.Amount.Equal(decimal.RequireFromString("56.13")) {
		t.Fatalf("PAYE: got %s, want 56.13", d.Amount)
	}
}

// TestGHPackTopBand: GHS 30,000 / month. SSNIT: 1,650.00; chargeable
// = 28,350 in the 19,896.67–50,416.67 band (30%): 4,572.67 + (28,350
// - 19,896.67) × 30% = 7,108.67.
func TestGHPackTopBand(t *testing.T) {
	pack, _ := Lookup("GH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(30000), monthPeriod())
	if d := findDeduction(out, "GH_SSNIT"); !d.Amount.Equal(decimal.NewFromInt(1650)) {
		t.Fatalf("SSNIT: got %s, want 1650.00", d.Amount)
	}
	if d := findDeduction(out, "GH_PAYE"); !d.Amount.Equal(decimal.RequireFromString("7108.67")) {
		t.Fatalf("PAYE: got %s, want 7108.67", d.Amount)
	}
}

// TestGHPackNonResident: non-residents pay a flat 25% with no SSNIT.
// 5,000 × 25% = 1,250.00.
func TestGHPackNonResident(t *testing.T) {
	pack, _ := Lookup("GH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: false},
		decimal.NewFromInt(5000), monthPeriod())
	if len(out) != 1 || out[0].Code != "GH_NONRESIDENT_TAX" || !out[0].Amount.Equal(decimal.NewFromInt(1250)) {
		t.Fatalf("nonres: got %+v, want GH_NONRESIDENT_TAX=1250.00", out)
	}
}

// TestGHPackZeroGross: empty-input edge case.
func TestGHPackZeroGross(t *testing.T) {
	pack, _ := Lookup("GH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.Zero, monthPeriod())
	if len(out) != 0 {
		t.Fatalf("zero gross should yield no deductions, got %+v", out)
	}
}
