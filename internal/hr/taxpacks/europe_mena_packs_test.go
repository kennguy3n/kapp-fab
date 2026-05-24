package taxpacks

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ===== Switzerland =====
//
// CH is the most complex pack in PR-2c. Up to four deduction
// lines emit on a single slip:
//
//   - CH_FED_TAX   (Bundessteuer Quellensteuer A0, Quellensteuer-
//                   liable employees only — bracket walk on
//                   annualised gross, prorated back via days/365.25)
//   - CH_CANTONAL_TAX (canton average burden, Quellensteuer-liable
//                       only — flat per-canton rate × gross)
//   - CH_AHV       (5.3% AHV/IV/EO, every employee, no ceiling)
//   - CH_ALV       (1.1% ALV, every employee, capped via the
//                   annual ceiling CHF 148,200 prorated by
//                   period-days/365.25)
//
// Gating (chIsQuellensteuerLiable):
//   - Non-resident                          → liable (cross-border)
//   - Resident + PermitType == "C" / ""     → not liable
//   - Resident + any other permit (B, L, …) → liable
//
// Hand-derivations below all use the standard 31-day month
// (periodFraction = 31 / 365.25 ≈ 0.084875) and verify the
// bracket-walk math against the chFederalBrackets table pinned
// in TestBracketTablesAreContiguous.

// TestCHPackBPermitFullStack pins the full 4-line emission
// path: a CHF 8,000 / month B-permit holder resident in
// Zürich. Quellensteuer liability is on; the cantonal average
// uses the ZH key (0.103).
//
//   periodFraction = 31 / 365.25 ≈ 0.084875
//   annualGross    = 8,000 / 0.084875 ≈ 94,258.06
//   bracket walk   = bracket 6 (78,100 → 103,600, base 1,403.27,
//                    rate 6.6%)
//   annualTax      = 1,403.27 + 0.066 × (94,258.06 - 78,100)
//                  = 1,403.27 + 1,066.43 ≈ 2,469.70
//   periodTax      = 2,469.70 × 0.084875 ≈ 209.61
//   cantonal (ZH)  = 8,000 × 0.103 = 824.00
//   AHV (5.3%)     = 8,000 × 0.053 = 424.00
//   ALV (1.1%)     = 8,000 × 0.011 = 88.00 (under cap)
func TestCHPackBPermitFullStack(t *testing.T) {
	pack, err := Lookup("CH")
	if err != nil {
		t.Fatalf("lookup CH: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, PermitType: "B", Canton: "ZH",
	}, decimal.NewFromInt(8000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	codes := indexByCode(out)
	// Tolerant band for federal tax: bracket-walk math + period
	// fraction division accumulates tiny precision drift in the
	// shopspring/decimal pipeline; a ±2 CHF window catches a
	// bracket miscoding without flaking on rounding.
	if fed := codes["CH_FED_TAX"]; fed.LessThan(decimal.NewFromInt(207)) || fed.GreaterThan(decimal.NewFromInt(212)) {
		t.Errorf("CH_FED_TAX = %s; expected ~209.61 (band 207-212)", fed)
	}
	if cant := codes["CH_CANTONAL_TAX"]; !cant.Equal(decimal.NewFromInt(824)) {
		t.Errorf("CH_CANTONAL_TAX = %s; want 824.00 (8000 × ZH 0.103)", cant)
	}
	if ahv := codes["CH_AHV"]; !ahv.Equal(decimal.NewFromInt(424)) {
		t.Errorf("CH_AHV = %s; want 424.00 (8000 × 5.3%%)", ahv)
	}
	if alv := codes["CH_ALV"]; !alv.Equal(decimal.NewFromInt(88)) {
		t.Errorf("CH_ALV = %s; want 88.00 (8000 × 1.1%%)", alv)
	}
}

// TestCHPackCPermitNoQuellensteuer pins the C-permit branch:
// settlement-permit holders (and Swiss citizens) self-assess
// federal + cantonal tax annually, so the slip emits only AHV
// + ALV. The same 8,000 CHF gross from the B-permit test
// should produce exactly 424 AHV + 88 ALV and nothing else.
func TestCHPackCPermitNoQuellensteuer(t *testing.T) {
	pack, _ := Lookup("CH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, PermitType: "C", Canton: "ZH",
	}, decimal.NewFromInt(8000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["CH_FED_TAX"]; ok {
		t.Errorf("CH_FED_TAX should be absent for C-permit; codes=%+v", codes)
	}
	if _, ok := codes["CH_CANTONAL_TAX"]; ok {
		t.Errorf("CH_CANTONAL_TAX should be absent for C-permit; codes=%+v", codes)
	}
	if ahv := codes["CH_AHV"]; !ahv.Equal(decimal.NewFromInt(424)) {
		t.Errorf("CH_AHV = %s; want 424.00", ahv)
	}
	if alv := codes["CH_ALV"]; !alv.Equal(decimal.NewFromInt(88)) {
		t.Errorf("CH_ALV = %s; want 88.00", alv)
	}
}

// TestCHPackSwissCitizenEmptyPermit confirms the empty-permit
// default lands on the citizen / C-permit path (not liable). A
// resident with PermitType == "" should be treated like a Swiss
// citizen — no Quellensteuer.
func TestCHPackSwissCitizenEmptyPermit(t *testing.T) {
	pack, _ := Lookup("CH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, PermitType: "", Canton: "ZH",
	}, decimal.NewFromInt(8000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["CH_FED_TAX"]; ok {
		t.Errorf("empty PermitType should default to citizen/C path (no Quellensteuer); codes=%+v", codes)
	}
	if _, ok := codes["CH_CANTONAL_TAX"]; ok {
		t.Errorf("empty PermitType should default to citizen/C path (no Quellensteuer); codes=%+v", codes)
	}
}

// TestCHPackALVMonthlyCap pins the ALV ceiling for a standard
// calendar-month slip. The cap is held annually (CHF 148,200) and
// prorated by the slip's period-days/365.25 — for Jan 1-31 (31
// days) the cap is 148,200 × 31/365.25 ≈ 12,578.23, so a high
// earner at CHF 20,000 / month gross:
//   AHV: 20,000 × 5.3% = 1,060.00 (no cap)
//   ALV: min(20000, 12578.23) × 1.1% = 12,578.23 × 0.011 ≈ 138.36
func TestCHPackALVMonthlyCap(t *testing.T) {
	pack, _ := Lookup("CH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, PermitType: "C", Canton: "ZH",
	}, decimal.NewFromInt(20000), monthPeriod())
	codes := indexByCode(out)
	if ahv := codes["CH_AHV"]; !ahv.Equal(decimal.NewFromInt(1060)) {
		t.Errorf("CH_AHV = %s; want 1,060.00 (20000 × 5.3%%, no cap)", ahv)
	}
	if alv := codes["CH_ALV"]; !alv.Equal(decimal.NewFromFloat(138.36)) {
		t.Errorf("CH_ALV = %s; want 138.36 (capped at 12,578.23 × 1.1%% for a 31-day month)", alv)
	}
}

// TestCHPackALVCapProratedAcrossPeriods pins the period-aware ALV
// cap proration. The cap is held as an annual figure (CHF
// 148,200) and scaled by days/365.25 so non-monthly slips enforce
// the right ceiling. Without this proration the cap would either
// over-restrict a quarterly slip (cap = 12,350 << real ceiling)
// or under-restrict a fortnightly slip (cap = 12,350 >> real
// ceiling). Demonstrated against two off-cycle periods:
//
//   Fortnightly slip (Jan 1-14 = 14 days), gross CHF 7,000:
//     periodCap = 148,200 × 14/365.25 ≈ 5,680.49
//     alvBase   = min(7000, 5680.49) = 5680.49 (cap triggers)
//     ALV       = 5,680.49 × 0.011 ≈ 62.49
//
//   Quarterly slip (Jan 1-Apr 1 = 91 days), gross CHF 50,000:
//     periodCap = 148,200 × 91/365.25 ≈ 36,923.20
//     alvBase   = min(50000, 36923.20) = 36923.20 (cap triggers)
//     ALV       = 36,923.20 × 0.011 ≈ 406.16
func TestCHPackALVCapProratedAcrossPeriods(t *testing.T) {
	pack, _ := Lookup("CH")

	t.Run("fortnightly slip triggers prorated cap", func(t *testing.T) {
		out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
			Resident: true, PermitType: "C", Canton: "ZH",
		}, decimal.NewFromInt(7000), PayPeriod{
			Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC),
		})
		codes := indexByCode(out)
		if alv := codes["CH_ALV"]; !alv.Equal(decimal.NewFromFloat(62.49)) {
			t.Errorf("fortnightly CH_ALV = %s; want 62.49 (prorated cap 5680.49 × 1.1%%) "+
				"— a monthly-only cap of 12,350 would let the full 7,000 through and "+
				"yield 77.00, under-restricting the slip relative to BSV's annual ceiling", alv)
		}
	})

	t.Run("quarterly slip triggers prorated cap", func(t *testing.T) {
		out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
			Resident: true, PermitType: "C", Canton: "ZH",
		}, decimal.NewFromInt(50000), PayPeriod{
			Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		})
		codes := indexByCode(out)
		if alv := codes["CH_ALV"]; !alv.Equal(decimal.NewFromFloat(406.16)) {
			t.Errorf("quarterly CH_ALV = %s; want 406.16 (prorated cap 36923.20 × 1.1%%) "+
				"— a monthly-only cap of 12,350 would over-restrict at 135.85, "+
				"under-collecting against BSV's annual ceiling", alv)
		}
	})
}

// TestCHPackNonResidentFallbackCanton pins the cantonal-lookup
// fallback: a non-resident cross-border G-permit worker with no
// canton key (or an unknown one) should fall back to the
// national-average cantonal rate (0.108).
//
//   gross = 6,000 / month
//   annualGross = 6,000 / 0.084875 ≈ 70,693.55
//   bracket walk = bracket 4 (55,200 → 72,500, base 556.82, rate 2.97%)
//   annualTax    = 556.82 + 0.0297 × (70,693.55 - 55,200)
//                = 556.82 + 460.16 ≈ 1,016.98
//   periodTax    = 1,016.98 × 0.084875 ≈ 86.31
//   cantonal     = 6,000 × 0.108 = 648.00 (fallback)
//   AHV          = 6,000 × 0.053 = 318.00
//   ALV          = 6,000 × 0.011 = 66.00
func TestCHPackNonResidentFallbackCanton(t *testing.T) {
	pack, _ := Lookup("CH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: false, PermitType: "G", Canton: "XX",
	}, decimal.NewFromInt(6000), monthPeriod())
	codes := indexByCode(out)
	if fed := codes["CH_FED_TAX"]; fed.LessThan(decimal.NewFromInt(84)) || fed.GreaterThan(decimal.NewFromInt(89)) {
		t.Errorf("CH_FED_TAX = %s; expected ~86.31 (band 84-89)", fed)
	}
	if cant := codes["CH_CANTONAL_TAX"]; !cant.Equal(decimal.NewFromInt(648)) {
		t.Errorf("CH_CANTONAL_TAX = %s; want 648.00 (6000 × fallback 0.108)", cant)
	}
	if ahv := codes["CH_AHV"]; !ahv.Equal(decimal.NewFromInt(318)) {
		t.Errorf("CH_AHV = %s; want 318.00", ahv)
	}
	if alv := codes["CH_ALV"]; !alv.Equal(decimal.NewFromInt(66)) {
		t.Errorf("CH_ALV = %s; want 66.00", alv)
	}
}

// TestCHPackTopBracketHighEarner exercises the Bundessteuer top
// bracket (income > CHF 755,200 / year) plus the ALV cap. A
// resident B-permit holder in Geneva at CHF 100,000 / month:
//   annualGross ≈ 100,000 / 0.084875 ≈ 1,178,225.81
//   bracket walk = bracket 10 (top, floor 755,200, base 86,822.67, rate 11.5%)
//   annualTax    = 86,822.67 + 0.115 × (1,178,225.81 - 755,200) ≈ 135,470.64
//   periodTax    ≈ 135,470.64 × 0.084875 ≈ 11,497.85
//   cantonal (GE) = 100,000 × 0.130 = 13,000.00
//   AHV           = 100,000 × 0.053 = 5,300.00
//   ALV (cap)     = 12,578.23 × 0.011 ≈ 138.36 (31-day month cap)
func TestCHPackTopBracketHighEarner(t *testing.T) {
	pack, _ := Lookup("CH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true, PermitType: "B", Canton: "GE",
	}, decimal.NewFromInt(100000), monthPeriod())
	codes := indexByCode(out)
	if fed := codes["CH_FED_TAX"]; fed.LessThan(decimal.NewFromInt(11200)) || fed.GreaterThan(decimal.NewFromInt(11700)) {
		t.Errorf("CH_FED_TAX = %s; expected ~11,497.85 (band 11,200-11,700)", fed)
	}
	if cant := codes["CH_CANTONAL_TAX"]; !cant.Equal(decimal.NewFromInt(13000)) {
		t.Errorf("CH_CANTONAL_TAX = %s; want 13,000.00 (100k × GE 0.130)", cant)
	}
	if ahv := codes["CH_AHV"]; !ahv.Equal(decimal.NewFromInt(5300)) {
		t.Errorf("CH_AHV = %s; want 5,300.00", ahv)
	}
	if alv := codes["CH_ALV"]; !alv.Equal(decimal.NewFromFloat(138.36)) {
		t.Errorf("CH_ALV = %s; want 138.36 (31-day prorated cap × 1.1%%)", alv)
	}
}

// TestCHPackZeroAndNegativeGrossReturnsNil pins the input-guard
// path. Both branches return nil rather than a zero-amount slice.
func TestCHPackZeroAndNegativeGrossReturnsNil(t *testing.T) {
	pack, _ := Lookup("CH")
	for _, gross := range []decimal.Decimal{decimal.Zero, decimal.NewFromInt(-100)} {
		out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
			Resident: true, PermitType: "B", Canton: "ZH",
		}, gross, monthPeriod())
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if out != nil {
			t.Errorf("gross=%s: expected nil; got %+v", gross, out)
		}
	}
}

// ===== UAE =====

// TestAEPackNationalGPSSABelowCap pins the GPSSA branch: a UAE
// national (Nationality == "local") at AED 30,000 / month gets
// a single AE_GPSSA line at 5%. 30,000 × 0.05 = 1,500.00.
func TestAEPackNationalGPSSABelowCap(t *testing.T) {
	pack, err := Lookup("AE")
	if err != nil {
		t.Fatalf("lookup AE: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(30000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 || out[0].Code != "AE_GPSSA" {
		t.Fatalf("expected single AE_GPSSA line; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(1500)) {
		t.Errorf("AE_GPSSA = %s; want 1,500.00 (30,000 × 5%%)", out[0].Amount)
	}
}

// TestAEPackNationalGPSSACapsAtCeiling pins the AED 50,000 /
// month ceiling: a UAE national at 60,000 has GPSSA computed
// against the cap, not gross. min(60000, 50000) × 5% = 2,500.00.
func TestAEPackNationalGPSSACapsAtCeiling(t *testing.T) {
	pack, _ := Lookup("AE")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(60000), monthPeriod())
	if len(out) != 1 || !out[0].Amount.Equal(decimal.NewFromInt(2500)) {
		t.Fatalf("expected AE_GPSSA = 2,500.00 at cap; got %+v", out)
	}
}

// TestAEPackExpatNoDeduction pins the non-national branch:
// expat (default or explicit "expat") gets no AE_GPSSA line.
// No PIT, no other employee-side line — empty result.
func TestAEPackExpatNoDeduction(t *testing.T) {
	pack, _ := Lookup("AE")
	for _, nat := range []string{"", "expat", "indian", "filipino"} {
		out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
			Nationality: nat,
		}, decimal.NewFromInt(30000), monthPeriod())
		if out != nil {
			t.Errorf("Nationality=%q: expected nil for non-national; got %+v", nat, out)
		}
	}
}

// ===== Saudi Arabia =====

// TestSAPackSaudiGOSIBelowCap: 20,000 SAR / month Saudi national
//   SA_GOSI_PENSION = 20,000 × 9%    = 1,800.00
//   SA_GOSI_SANED   = 20,000 × 0.75% =   150.00
func TestSAPackSaudiGOSIBelowCap(t *testing.T) {
	pack, err := Lookup("SA")
	if err != nil {
		t.Fatalf("lookup SA: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(20000), monthPeriod())
	codes := indexByCode(out)
	if p := codes["SA_GOSI_PENSION"]; !p.Equal(decimal.NewFromInt(1800)) {
		t.Errorf("SA_GOSI_PENSION = %s; want 1,800.00", p)
	}
	if s := codes["SA_GOSI_SANED"]; !s.Equal(decimal.NewFromInt(150)) {
		t.Errorf("SA_GOSI_SANED = %s; want 150.00", s)
	}
}

// TestSAPackSaudiGOSIAtCap: 50,000 SAR / month → cap at 45,000
//   SA_GOSI_PENSION = 45,000 × 9%    = 4,050.00
//   SA_GOSI_SANED   = 45,000 × 0.75% =   337.50
func TestSAPackSaudiGOSIAtCap(t *testing.T) {
	pack, _ := Lookup("SA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if p := codes["SA_GOSI_PENSION"]; !p.Equal(decimal.NewFromInt(4050)) {
		t.Errorf("SA_GOSI_PENSION = %s; want 4,050.00 (capped at 45k × 9%%)", p)
	}
	if s := codes["SA_GOSI_SANED"]; !s.Equal(decimal.NewFromFloat(337.5)) {
		t.Errorf("SA_GOSI_SANED = %s; want 337.50 (capped at 45k × 0.75%%)", s)
	}
}

// TestSAPackNonSaudiNoDeduction: non-Saudi expat (Nationality
// != "local") gets no employee-side GOSI line. The employer-
// paid 2% Occupational Hazards branch is NOT a payroll
// deduction so the pack does not emit a line for it.
func TestSAPackNonSaudiNoDeduction(t *testing.T) {
	pack, _ := Lookup("SA")
	for _, nat := range []string{"", "expat", "indian"} {
		out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
			Nationality: nat,
		}, decimal.NewFromInt(30000), monthPeriod())
		if out != nil {
			t.Errorf("Nationality=%q: expected nil for non-Saudi; got %+v", nat, out)
		}
	}
}

// ===== Qatar =====

// TestQAPackQatariBelowCap: 30,000 QAR / month Qatari national
//   QA_RETIREMENT = 30,000 × 5% = 1,500.00
func TestQAPackQatariBelowCap(t *testing.T) {
	pack, err := Lookup("QA")
	if err != nil {
		t.Fatalf("lookup QA: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(30000), monthPeriod())
	if len(out) != 1 || out[0].Code != "QA_RETIREMENT" {
		t.Fatalf("expected single QA_RETIREMENT line; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(1500)) {
		t.Errorf("QA_RETIREMENT = %s; want 1,500.00", out[0].Amount)
	}
}

// TestQAPackQatariAtCap: 150,000 QAR / month → cap at 100,000
//   QA_RETIREMENT = min(150000, 100000) × 5% = 5,000.00
func TestQAPackQatariAtCap(t *testing.T) {
	pack, _ := Lookup("QA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(150000), monthPeriod())
	if len(out) != 1 || !out[0].Amount.Equal(decimal.NewFromInt(5000)) {
		t.Fatalf("expected QA_RETIREMENT = 5,000.00 at cap; got %+v", out)
	}
}

// TestQAPackExpatNoDeduction: non-Qatari has no GRSIA employee
// share (2028 PIT — Royal Decree No. 56/2025, 5% on income >
// QAR 42k annually, effective 1 Jan 2028 — is NOT yet wired;
// tracked in maintenance docs).
func TestQAPackExpatNoDeduction(t *testing.T) {
	pack, _ := Lookup("QA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "expat",
	}, decimal.NewFromInt(30000), monthPeriod())
	if out != nil {
		t.Errorf("expected nil for non-Qatari; got %+v", out)
	}
}

// ===== Kuwait =====

// TestKWPackKuwaitiBelowCap: 2,000 KWD / month Kuwaiti
//   KW_PIFSS = 2,000 × 10.5% = 210.00
func TestKWPackKuwaitiBelowCap(t *testing.T) {
	pack, err := Lookup("KW")
	if err != nil {
		t.Fatalf("lookup KW: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(2000), monthPeriod())
	if len(out) != 1 || out[0].Code != "KW_PIFSS" {
		t.Fatalf("expected single KW_PIFSS line; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(210)) {
		t.Errorf("KW_PIFSS = %s; want 210.00", out[0].Amount)
	}
}

// TestKWPackKuwaitiAtCap: 5,000 KWD → cap at 2,750
//   KW_PIFSS = 2,750 × 10.5% = 288.75
func TestKWPackKuwaitiAtCap(t *testing.T) {
	pack, _ := Lookup("KW")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(5000), monthPeriod())
	if len(out) != 1 || !out[0].Amount.Equal(decimal.NewFromFloat(288.75)) {
		t.Fatalf("expected KW_PIFSS = 288.75 at cap; got %+v", out)
	}
}

// TestKWPackNonKuwaitiNoDeduction
func TestKWPackNonKuwaitiNoDeduction(t *testing.T) {
	pack, _ := Lookup("KW")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "expat",
	}, decimal.NewFromInt(2000), monthPeriod())
	if out != nil {
		t.Errorf("expected nil for non-Kuwaiti; got %+v", out)
	}
}

// ===== Bahrain =====

// TestBHPackBahrainiSplitBelowCap: Bahraini national gets both
// the 8% pension line AND the 1% unemployment line.
//   BH_SIO_PENSION       = 1,000 × 8% =  80.00
//   BH_SIO_UNEMPLOYMENT  = 1,000 × 1% =  10.00
func TestBHPackBahrainiSplitBelowCap(t *testing.T) {
	pack, err := Lookup("BH")
	if err != nil {
		t.Fatalf("lookup BH: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(1000), monthPeriod())
	codes := indexByCode(out)
	if p := codes["BH_SIO_PENSION"]; !p.Equal(decimal.NewFromInt(80)) {
		t.Errorf("BH_SIO_PENSION = %s; want 80.00", p)
	}
	if u := codes["BH_SIO_UNEMPLOYMENT"]; !u.Equal(decimal.NewFromInt(10)) {
		t.Errorf("BH_SIO_UNEMPLOYMENT = %s; want 10.00", u)
	}
}

// TestBHPackBahrainiSplitAtCap: 5,000 BHD → cap at 4,000
//   BH_SIO_PENSION      = 4,000 × 8% = 320.00
//   BH_SIO_UNEMPLOYMENT = 4,000 × 1% =  40.00
func TestBHPackBahrainiSplitAtCap(t *testing.T) {
	pack, _ := Lookup("BH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(5000), monthPeriod())
	codes := indexByCode(out)
	if p := codes["BH_SIO_PENSION"]; !p.Equal(decimal.NewFromInt(320)) {
		t.Errorf("BH_SIO_PENSION = %s; want 320.00 (capped)", p)
	}
	if u := codes["BH_SIO_UNEMPLOYMENT"]; !u.Equal(decimal.NewFromInt(40)) {
		t.Errorf("BH_SIO_UNEMPLOYMENT = %s; want 40.00 (capped)", u)
	}
}

// TestBHPackNonBahrainiGetsUnemploymentOnly: non-Bahraini
// employees pay ONLY the 1% Unemployment Insurance — no
// pension line (employer pays 3% Occupational Hazards on their
// behalf, which is not a payroll deduction).
func TestBHPackNonBahrainiGetsUnemploymentOnly(t *testing.T) {
	pack, _ := Lookup("BH")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "expat",
	}, decimal.NewFromInt(1000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["BH_SIO_PENSION"]; ok {
		t.Errorf("BH_SIO_PENSION should be absent for non-Bahraini; codes=%+v", codes)
	}
	if u := codes["BH_SIO_UNEMPLOYMENT"]; !u.Equal(decimal.NewFromInt(10)) {
		t.Errorf("BH_SIO_UNEMPLOYMENT = %s; want 10.00 (non-Bahraini 1%%)", u)
	}
}

// ===== Oman =====

// TestOMPackOmaniBelowCap: 1,000 OMR Omani national
//   OM_PASI = 1,000 × 8% = 80.00
func TestOMPackOmaniBelowCap(t *testing.T) {
	pack, err := Lookup("OM")
	if err != nil {
		t.Fatalf("lookup OM: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(1000), monthPeriod())
	if len(out) != 1 || out[0].Code != "OM_PASI" {
		t.Fatalf("expected single OM_PASI line; got %+v", out)
	}
	if !out[0].Amount.Equal(decimal.NewFromInt(80)) {
		t.Errorf("OM_PASI = %s; want 80.00", out[0].Amount)
	}
}

// TestOMPackOmaniAtCap: 4,000 OMR → cap at 3,000
//   OM_PASI = 3,000 × 8% = 240.00
func TestOMPackOmaniAtCap(t *testing.T) {
	pack, _ := Lookup("OM")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "local",
	}, decimal.NewFromInt(4000), monthPeriod())
	if len(out) != 1 || !out[0].Amount.Equal(decimal.NewFromInt(240)) {
		t.Fatalf("expected OM_PASI = 240.00 at cap; got %+v", out)
	}
}

// TestOMPackNonOmaniNoDeduction: expat in Oman has no PASI
// (and no PIT — 2028 PIT per Royal Decree No. 56/2025 not yet
// wired; tracked in maintenance docs).
func TestOMPackNonOmaniNoDeduction(t *testing.T) {
	pack, _ := Lookup("OM")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Nationality: "expat",
	}, decimal.NewFromInt(1000), monthPeriod())
	if out != nil {
		t.Errorf("expected nil for non-Omani; got %+v", out)
	}
}

// ===== Cross-pack: GCC national-helper invariants =====

// TestIsGCCNationalIsCaseInsensitive pins the shared
// `isGCCNational` helper used by every GCC pack: it must be
// case-insensitive on "local" and reject everything else
// (including the empty string, which defaults to "expat" per
// EmployeeInfo's documented convention).
func TestIsGCCNationalIsCaseInsensitive(t *testing.T) {
	for _, nat := range []string{"local", "Local", "LOCAL", "  local  "} {
		if !isGCCNational(nat) {
			t.Errorf("isGCCNational(%q) = false; want true", nat)
		}
	}
	for _, nat := range []string{"", "expat", "Expat", "indian", "filipino", "non-local"} {
		if isGCCNational(nat) {
			t.Errorf("isGCCNational(%q) = true; want false", nat)
		}
	}
}

// ===== Registry assertions =====

// TestEMEAPacksAreRegistered confirms the seven Europe + MENA
// packs all self-register and resolve through Lookup.
func TestEMEAPacksAreRegistered(t *testing.T) {
	for _, code := range []string{"CH", "AE", "SA", "QA", "KW", "BH", "OM"} {
		if _, err := Lookup(code); err != nil {
			t.Errorf("Lookup(%q): %v", code, err)
		}
	}
}

// TestEMEAPacksExposeEffectiveYear pins each Europe + MENA
// pack's calibrated year. Bumps must be deliberate and land in
// the same PR as a bracket / rate update.
func TestEMEAPacksExposeEffectiveYear(t *testing.T) {
	cases := map[string]int{
		"CH": 2025,
		"AE": 2024,
		"SA": 2024,
		"QA": 2024,
		"KW": 2024,
		"BH": 2024,
		"OM": 2024,
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

// TestCHFederalBracketsAreContiguous extends the bracket-
// contiguity guarantee to the CH federal Quellensteuer schedule.
// The shared checker lives in apac_packs_test.go; this test
// projects chFederalBrackets through the same bracketRow shape
// and asserts the same two invariants (Top-contiguity and
// Base-consistency) so a typo in any future schedule update
// fails the build.
func TestCHFederalBracketsAreContiguous(t *testing.T) {
	chRows := make([]bracketRow, len(chFederalBrackets))
	for i, b := range chFederalBrackets {
		chRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	for i := 0; i < len(chRows)-1; i++ {
		cur, next := chRows[i], chRows[i+1]
		if !cur.top.Equal(next.floor) {
			t.Fatalf("CH brackets[%d].Top (%s) != brackets[%d].Floor (%s)",
				i, cur.top, i+1, next.floor)
		}
		want := cur.base.Add(next.floor.Sub(cur.floor).Mul(cur.rate))
		if !next.base.Equal(want) {
			t.Fatalf("CH brackets[%d].Base (%s) != Base[%d] + (Floor[%d]-Floor[%d])*Rate[%d] (= %s)",
				i+1, next.base, i, i+1, i, i, want)
		}
	}
	last := chRows[len(chRows)-1]
	if !last.top.IsZero() {
		t.Fatalf("CH last bracket Top should be 0 (open-ended), got %s", last.top)
	}
}
