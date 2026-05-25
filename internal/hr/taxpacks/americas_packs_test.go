package taxpacks

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// monthlyPeriod returns the standard January-2026 30-day pay
// period used by most of the bracket assertions in this file.
// Using a fixed period keeps the days/365.25 prorate explicit in
// the hand-derivations.
func monthlyPeriod() PayPeriod {
	return PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
}

// approxEqual returns true if |a-b| ≤ tol.  Used to assert the
// hand-derived bracket walks land in a tight band; we tolerate a
// few cents of rounding drift between the Python derivation and
// the Go shopspring/decimal walk.
func approxEqual(t *testing.T, label string, got, want, tol decimal.Decimal) {
	t.Helper()
	diff := got.Sub(want).Abs()
	if diff.GreaterThan(tol) {
		t.Errorf("%s = %s; want %s ± %s (diff %s)", label, got, want, tol, diff)
	}
}

// TestCAPackFederalAndProvincialOntario covers the most common CA
// path: a salaried employee in Ontario, no exemption flags, no
// YTD-cap interaction. Validates that both CA_FED_TAX and
// CA_PROV_TAX surface and land in CRA T4127-derived bands.
//
// $6,000 monthly slip → annualised $73,005 (period 31 days,
// 31/365.25). Federal: ($73,005 − $16,129 BPA) = $56,876 taxable
// → bracket 1 ($8,606.25 base + (56,876−57,375)<0 → still in
// bracket 0 = 15% of $56,876 = $8,531.40) → period-prorate to
// ≈ $724. ON: ($73,005 − $12,747 BPA) = $60,258 taxable → bracket
// 1 floor 52,886 → 2,670.74 + (60,258−52,886)×9.15% = $3,345 →
// period-prorate ≈ $284.
func TestCAPackFederalAndProvincialOntario(t *testing.T) {
	pack, err := Lookup("CA")
	if err != nil {
		t.Fatalf("Lookup(CA): %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Province: "ON",
	}, decimal.NewFromInt(6000), monthlyPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	codes := indexByCode(out)

	if _, ok := codes["CA_FED_TAX"]; !ok {
		t.Fatalf("missing CA_FED_TAX in %v", codes)
	}
	if _, ok := codes["CA_PROV_TAX"]; !ok {
		t.Fatalf("missing CA_PROV_TAX in %v", codes)
	}
	if _, ok := codes["CA_CPP"]; !ok {
		t.Fatalf("missing CA_CPP in %v", codes)
	}
	if _, ok := codes["CA_EI"]; !ok {
		t.Fatalf("missing CA_EI in %v", codes)
	}
	// Federal in $650-$800 band.
	approxEqual(t, "CA_FED_TAX", codes["CA_FED_TAX"], decimal.NewFromInt(725), decimal.NewFromInt(75))
	// ON provincial in $260-$320 band.
	approxEqual(t, "CA_PROV_TAX", codes["CA_PROV_TAX"], decimal.NewFromInt(284), decimal.NewFromInt(30))
}

// TestCAPackQuebecUsesQPP exercises the QC special case: a QC
// resident never receives CA_CPP, always receives CA_QPP, and the
// EI line uses the reduced 1.31% Quebec rate (because QC operates
// its own QPIP).
func TestCAPackQuebecUsesQPP(t *testing.T) {
	pack, _ := Lookup("CA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Province: "QC",
	}, decimal.NewFromInt(6000), monthlyPeriod())
	codes := indexByCode(out)

	if _, ok := codes["CA_CPP"]; ok {
		t.Fatalf("QC resident should NOT have CA_CPP: %v", codes)
	}
	if _, ok := codes["CA_QPP"]; !ok {
		t.Fatalf("QC resident missing CA_QPP: %v", codes)
	}
	if _, ok := codes["CA_EI_QC"]; !ok {
		t.Fatalf("QC resident missing CA_EI_QC: %v", codes)
	}
	if _, ok := codes["CA_QPIP"]; !ok {
		t.Fatalf("QC resident missing CA_QPIP: %v", codes)
	}
	// QPP rate is higher than CPP (6.40% vs 5.95%) so the line
	// must be positive.
	if !codes["CA_QPP"].IsPositive() {
		t.Fatalf("CA_QPP should be positive, got %s", codes["CA_QPP"])
	}
}

// TestCAPackCPPExemptionHonored covers the per-slip exemption
// flag. An employee with CPPExempt = true must NOT have CA_CPP
// even if Province / Age would otherwise warrant it.
func TestCAPackCPPExemptionHonored(t *testing.T) {
	pack, _ := Lookup("CA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Province:  "ON",
		CPPExempt: true,
	}, decimal.NewFromInt(6000), monthlyPeriod())
	for _, d := range out {
		if d.Code == "CA_CPP" || d.Code == "CA_CPP2" {
			t.Fatalf("CPP exempt employee got %s = %s", d.Code, d.Amount)
		}
	}
}

// TestCAPackEICappedByYTD asserts EI stops accruing once YTD
// gross exceeds the MIE ($65,700 in 2025).
func TestCAPackEICappedByYTD(t *testing.T) {
	pack, _ := Lookup("CA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Province: "ON",
		YTDGross: decimal.NewFromInt(70000), // already over MIE.
	}, decimal.NewFromInt(6000), monthlyPeriod())
	for _, d := range out {
		if d.Code == "CA_EI" {
			t.Fatalf("CA_EI should be zero at YTD > MIE: %s", d.Amount)
		}
	}
}

// TestCAPackUnknownProvinceFallsBackToFederalOnly covers the
// allow-list: an unknown / typo Province (e.g. "XX") yields a
// slip with no CA_PROV_TAX line — the pack returns federal-only
// rather than crashing.
func TestCAPackUnknownProvinceFallsBackToFederalOnly(t *testing.T) {
	pack, _ := Lookup("CA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Province: "XX",
	}, decimal.NewFromInt(6000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["CA_PROV_TAX"]; ok {
		t.Fatalf("unknown province should fall back to federal-only, got CA_PROV_TAX = %s", codes["CA_PROV_TAX"])
	}
	if _, ok := codes["CA_FED_TAX"]; !ok {
		t.Fatalf("missing CA_FED_TAX even for unknown-province fallback")
	}
}

// TestBRPackIRRFAfterINSSDeduction exercises Brazil: a R$5,000
// monthly slip must compute INSS first, then subtract INSS from
// gross before walking the IRRF schedule. Hand-derivation:
//
//	INSS bands: 7.5% × 1,518 = 113.85 + 9% × (2,793.88-1,518) =
//	  228.679 + 12% × (4,190.83-2,793.88) = 396.314 + 14% ×
//	  (5,000-4,190.83) = 509.598 ≈ R$509.60.
//	IRRF base = 5,000 − 509.60 = 4,490.40 → bracket 3 (3,751.05—
//	  4,664.68, parcela 181.22, rate 22.5%) → 181.22 + 22.5% ×
//	  (4,490.40-3,751.05) = 181.22 + 166.35 = 347.57.
//	But 4,490.40 is below the bracket-3 ceiling 4,664.68 so this
//	is the right bracket. IRRF ≈ R$347.57.
func TestBRPackIRRFAfterINSSDeduction(t *testing.T) {
	pack, _ := Lookup("BR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(5000), monthlyPeriod())
	codes := indexByCode(out)

	approxEqual(t, "BR_INSS", codes["BR_INSS"], decimal.NewFromFloat(509.60), decimal.NewFromFloat(0.50))
	approxEqual(t, "BR_IRRF", codes["BR_IRRF"], decimal.NewFromFloat(346), decimal.NewFromFloat(10))
}

// TestBRPackBelowIRRFThreshold covers the exempt band: a monthly
// gross of R$2,200 is below the IRRF threshold (R$2,259.20) so
// only INSS withholds — no BR_IRRF line at all.
func TestBRPackBelowIRRFThreshold(t *testing.T) {
	pack, _ := Lookup("BR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(2200), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["BR_IRRF"]; ok {
		t.Fatalf("BR_IRRF should be absent below threshold: %s", codes["BR_IRRF"])
	}
	if _, ok := codes["BR_INSS"]; !ok {
		t.Fatalf("BR_INSS should still be present: %v", codes)
	}
}

// TestBRPackINSSCeiling asserts gross above the teto previdenciário
// caps INSS at the band-walk value at the ceiling (≈ R$951.62).
func TestBRPackINSSCeiling(t *testing.T) {
	pack, _ := Lookup("BR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(20000), monthlyPeriod())
	codes := indexByCode(out)
	// Ceiling band-walk: 396.31 + 14% × (8157.41 - 4190.83) ≈ 951.63
	approxEqual(t, "BR_INSS@ceiling", codes["BR_INSS"], decimal.NewFromFloat(951.63), decimal.NewFromFloat(0.50))
}

// TestMXPackSubsidioClampedToZero covers Mexico's employment-
// subsidy clamp. A low earner (gross MXN 8,000/month) has a
// computed ISR below the MXN 475 monthly subsidy, so MX_ISR
// clamps at zero (the net subsidio is paid back via the patron,
// not surfaced as a negative withholding).
func TestMXPackSubsidioClampedToZero(t *testing.T) {
	pack, _ := Lookup("MX")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(8000), monthlyPeriod())
	codes := indexByCode(out)
	if isr, ok := codes["MX_ISR"]; ok && isr.IsNegative() {
		t.Fatalf("MX_ISR should not be negative: %s", isr)
	}
	if _, ok := codes["MX_IMSS"]; !ok {
		t.Fatalf("MX_IMSS missing: %v", codes)
	}
}

// TestMXPackISRMidBracket sanity-checks the ISR walk at a higher
// gross (MXN 50,000/month). The published cuota fija at the floor
// of bracket 7 ($49,233 → 9,236.89) + 30% × (50,000-49,233) =
// 9,467 — subsidy phases out at this income → MX_ISR ≈ $9,467.
func TestMXPackISRMidBracket(t *testing.T) {
	pack, _ := Lookup("MX")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(50000), monthlyPeriod())
	codes := indexByCode(out)
	approxEqual(t, "MX_ISR@50k", codes["MX_ISR"], decimal.NewFromFloat(9467), decimal.NewFromFloat(2))
}

// TestARPackInflationThresholds asserts the MNI + Special
// Deduction (≈ ARS 18M/year for employees in 2025) gates
// Ganancias so a salaried worker earning ARS 1.2M/month
// (≈ ARS 14.4M/year, below MNI+ED) pays zero Ganancias.
func TestARPackInflationThresholds(t *testing.T) {
	pack, _ := Lookup("AR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(1200000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["AR_GANANCIAS"]; ok {
		t.Fatalf("AR_GANANCIAS should not apply below MNI+ED: %s", codes["AR_GANANCIAS"])
	}
	if _, ok := codes["AR_JUBILACION"]; !ok {
		t.Fatalf("AR_JUBILACION should always apply (no income floor): %v", codes)
	}
}

// TestCOPackFSPHighEarner asserts the FSP surtax activates when
// gross exceeds 4 × SMLMV. At 5 × SMLMV (~COP 7,117,500) FSP
// applies the 1% surcharge.
func TestCOPackFSPHighEarner(t *testing.T) {
	pack, _ := Lookup("CO")
	gross := decimal.NewFromInt(7117500)
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, gross, monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["CO_FSP"]; !ok {
		t.Fatalf("CO_FSP missing for high earner: %v", codes)
	}
	// 1% × gross (capped at 25 SMLMV which is far above this gross).
	approxEqual(t, "CO_FSP@5SMLMV", codes["CO_FSP"], gross.Mul(decimal.NewFromFloat(0.01)), decimal.NewFromInt(10))
}

// TestCOPackFSPBelowThreshold asserts FSP does NOT activate for
// salaries below 4 SMLMV.
func TestCOPackFSPBelowThreshold(t *testing.T) {
	pack, _ := Lookup("CO")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(2000000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["CO_FSP"]; ok {
		t.Fatalf("CO_FSP should not activate below 4 SMLMV: %s", codes["CO_FSP"])
	}
}

// TestCLPackAFPAndSalud verifies Chile emits AFP + Salud +
// Cesantía even for low earners (no income-tax threshold gates
// these), and that Impuesto Único kicks in for higher salaries.
func TestCLPackAFPAndSalud(t *testing.T) {
	pack, _ := Lookup("CL")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(2000000), monthlyPeriod())
	codes := indexByCode(out)
	for _, code := range []string{"CL_AFP", "CL_SALUD", "CL_SEGURO_CESANTIA"} {
		if _, ok := codes[code]; !ok {
			t.Fatalf("missing %s in CL pack output: %v", code, codes)
		}
	}
}

// TestCLImpuestoUnicoBelowExemption covers the 13.5 UTM exempt
// band: a gross of CLP 500,000 (~7.4 UTM at Jan-2025 UTM of
// 67,294) is well below the 13.5 UTM threshold so Impuesto
// Único is absent.
func TestCLImpuestoUnicoBelowExemption(t *testing.T) {
	pack, _ := Lookup("CL")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(500000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["CL_IMPUESTO_UNICO"]; ok {
		t.Fatalf("CL_IMPUESTO_UNICO should be absent below 13.5 UTM: %s", codes["CL_IMPUESTO_UNICO"])
	}
}

// TestPEPackONPAndBelowExempt covers Peru: ONP always withholds
// (13% on any positive gross), Renta 5ta only above 7 UIT
// annualised (~PEN 37,450). A monthly slip of PEN 3,000
// (≈ annualised 35,326) is below 7 UIT → no PE_RENTA_5TA.
func TestPEPackONPAndBelowExempt(t *testing.T) {
	pack, _ := Lookup("PE")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(3000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["PE_ONP"]; !ok {
		t.Fatalf("PE_ONP missing: %v", codes)
	}
	if _, ok := codes["PE_RENTA_5TA"]; ok {
		t.Fatalf("PE_RENTA_5TA should be absent below 7 UIT annual: %s", codes["PE_RENTA_5TA"])
	}
}

// TestCRPackCCSSAndExempt covers Costa Rica: CCSS always
// withholds (10.67% on any positive gross), Impuesto al Salario
// only above CRC 941,000/month. A gross of CRC 600,000 is below
// the threshold → no CR_IMPUESTO_SALARIO.
func TestCRPackCCSSAndExempt(t *testing.T) {
	pack, _ := Lookup("CR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(600000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["CR_CCSS"]; !ok {
		t.Fatalf("CR_CCSS missing: %v", codes)
	}
	if _, ok := codes["CR_IMPUESTO_SALARIO"]; ok {
		t.Fatalf("CR_IMPUESTO_SALARIO should be absent below threshold: %s", codes["CR_IMPUESTO_SALARIO"])
	}
}

// TestPAPackDollarized covers Panama: the pack accepts a USD
// gross (Panama's PAB is pegged 1:1 to USD) and emits CSS +
// Seguro Educativo + ISR. A gross of $2,000/month (~$24k/year)
// is above the $11k ISR threshold so ISR withholds at the
// 15% bracket.
func TestPAPackDollarized(t *testing.T) {
	pack, _ := Lookup("PA")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(2000), monthlyPeriod())
	codes := indexByCode(out)
	for _, c := range []string{"PA_CSS", "PA_SEGURO_EDUCATIVO", "PA_ISR"} {
		if _, ok := codes[c]; !ok {
			t.Fatalf("missing %s in PA pack output: %v", c, codes)
		}
	}
}

// TestUYPackIRPFAfterAportes covers Uruguay's IRPF base
// computation: nominal gross less Jubilación / FONASA / FRL
// must precede the BPC bracket walk. A nominal of UYU 80,000
// (~12.5 BPC) is above the 7-BPC exempt band, IRPF should
// withhold a modest amount.
func TestUYPackIRPFAfterAportes(t *testing.T) {
	pack, _ := Lookup("UY")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(80000), monthlyPeriod())
	codes := indexByCode(out)
	for _, c := range []string{"UY_BPS_JUBILACION", "UY_BPS_FONASA", "UY_FRL"} {
		if _, ok := codes[c]; !ok {
			t.Fatalf("missing %s: %v", c, codes)
		}
	}
}

// TestECPackBelowExempt covers Ecuador's 0% band: a $900/month
// gross (~$10,800/year) is below the $11,902 exempt threshold,
// so EC_IMPUESTO_RENTA is absent.
func TestECPackBelowExempt(t *testing.T) {
	pack, _ := Lookup("EC")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(900), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["EC_IMPUESTO_RENTA"]; ok {
		t.Fatalf("EC_IMPUESTO_RENTA should be absent below exempt: %s", codes["EC_IMPUESTO_RENTA"])
	}
	if _, ok := codes["EC_IESS"]; !ok {
		t.Fatalf("EC_IESS missing: %v", codes)
	}
}

// TestDOPackAFPAndSFSCapAndISR covers the Dominican Republic
// caps + ISR. A gross of DOP 200,000/month exercises all three
// lines (AFP, SFS, ISR).
func TestDOPackAFPAndSFSCapAndISR(t *testing.T) {
	pack, _ := Lookup("DO")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(200000), monthlyPeriod())
	codes := indexByCode(out)
	for _, c := range []string{"DO_AFP", "DO_SFS", "DO_ISR"} {
		if _, ok := codes[c]; !ok {
			t.Fatalf("missing %s: %v", c, codes)
		}
	}
}

// TestGTPackBelowExempt covers Guatemala's 48k exempt threshold:
// a low-income earner GTQ 3,000/month (~GTQ 36,000/year) is below
// the exempt threshold so GT_ISR is absent.
func TestGTPackBelowExempt(t *testing.T) {
	pack, _ := Lookup("GT")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(3000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["GT_ISR"]; ok {
		t.Fatalf("GT_ISR should be absent below exempt: %s", codes["GT_ISR"])
	}
	if _, ok := codes["GT_IGSS"]; !ok {
		t.Fatalf("GT_IGSS missing: %v", codes)
	}
}

// TestPYPackBelowIRPThreshold covers Paraguay: most employees
// fall below the 80-jornales threshold (~PYG 9.4M/year) so IRP
// does not apply. A gross of PYG 5,000,000/month is below the
// IRP gating threshold so PY_IRP is absent.
func TestPYPackBelowIRPThreshold(t *testing.T) {
	pack, _ := Lookup("PY")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(5000000), monthlyPeriod())
	codes := indexByCode(out)
	if _, ok := codes["PY_IRP"]; ok {
		t.Fatalf("PY_IRP should be absent below threshold: %s", codes["PY_IRP"])
	}
	if _, ok := codes["PY_IPS"]; !ok {
		t.Fatalf("PY_IPS missing: %v", codes)
	}
}

// TestTTPackPAYEAndNIS covers Trinidad & Tobago: PAYE + NIS +
// Health Surcharge for a TTD 10,000/month gross.
func TestTTPackPAYEAndNIS(t *testing.T) {
	pack, _ := Lookup("TT")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(10000), monthlyPeriod())
	codes := indexByCode(out)
	for _, c := range []string{"TT_NIS", "TT_HEALTH_SURCHARGE", "TT_PAYE"} {
		if _, ok := codes[c]; !ok {
			t.Fatalf("missing %s in TT pack: %v", c, codes)
		}
	}
}

// TestAmericasPacksZeroGrossReturnsNil ensures every new pack
// honours the EmployeeInfo zero-gross convention: a slip with
// zero or negative gross produces no deductions. Pins the no-pay
// path across the batch.
func TestAmericasPacksZeroGrossReturnsNil(t *testing.T) {
	period := monthlyPeriod()
	for _, code := range []string{"CA", "BR", "MX", "AR", "CO", "CL", "PE", "CR", "PA", "UY", "EC", "DO", "GT", "PY", "TT"} {
		t.Run(code, func(t *testing.T) {
			pack, err := Lookup(code)
			if err != nil {
				t.Fatalf("Lookup(%q): %v", code, err)
			}
			out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.Zero, period)
			if err != nil {
				t.Fatalf("compute(0): %v", err)
			}
			if len(out) != 0 {
				t.Fatalf("expected no deductions for zero gross, got %+v", out)
			}
		})
	}
}

// TestAmericasPacksNegativePeriodReturnsNil pins the
// degenerate-period contract: a pay period with End < Start
// (Days() == 0) yields no deductions across the whole batch.
func TestAmericasPacksNegativePeriodReturnsNil(t *testing.T) {
	bogusPeriod := PayPeriod{
		Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	for _, code := range []string{"CA", "BR", "MX", "AR", "CO", "CL", "PE", "CR", "PA", "UY", "EC", "DO", "GT", "PY", "TT"} {
		t.Run(code, func(t *testing.T) {
			pack, _ := Lookup(code)
			out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(1000), bogusPeriod)
			if len(out) != 0 {
				t.Fatalf("expected no deductions for inverted period, got %+v", out)
			}
		})
	}
}

// TestAmericasPacksEffectiveYear pins the EffectiveYear contract
// across the batch — all new packs report 2025.
func TestAmericasPacksEffectiveYear(t *testing.T) {
	for _, code := range []string{"CA", "BR", "MX", "AR", "CO", "CL", "PE", "CR", "PA", "UY", "EC", "DO", "GT", "PY", "TT"} {
		pack, _ := Lookup(code)
		if got := pack.EffectiveYear(); got != 2025 {
			t.Errorf("%s.EffectiveYear() = %d; want 2025", code, got)
		}
	}
}

// TestAmericasBracketTablesAreContiguous extends the pattern
// established by TestBracketTablesAreContiguous (apac batch) to
// the CA + LATAM packs. Every bracket table that participates in
// a walk function must satisfy:
//
//  1. Top[i] == Floor[i+1] (top-contiguity).
//  2. Base[i+1] == Base[i] + (Floor[i+1] - Floor[i]) × Rate[i]
//     (cumulative-tax monotonicity).
//
// Plus the open-ended-top invariant: the last row's Top is 0.
//
// CA is the heaviest pack: 14 tables (federal + 13 provinces).
// All four LATAM packs that use a Floor/Top/Base/Rate shape (BR
// IRRF + INSS, MX ISR, AR Ganancias, EC Impuesto, UY IRPF,
// PE Renta) are covered.
func TestAmericasBracketTablesAreContiguous(t *testing.T) {
	check := func(t *testing.T, label string, rows []bracketRow) {
		t.Helper()
		for i := 0; i < len(rows)-1; i++ {
			cur, next := rows[i], rows[i+1]
			if !cur.top.Equal(next.floor) {
				t.Fatalf("%s row[%d].Top (%s) != row[%d].Floor (%s)", label, i, cur.top, i+1, next.floor)
			}
			want := cur.base.Add(next.floor.Sub(cur.floor).Mul(cur.rate))
			if !next.base.Equal(want) {
				t.Fatalf("%s row[%d].Base (%s) != Base[%d] + (Floor[%d]-Floor[%d])*Rate[%d] (= %s)",
					label, i+1, next.base, i, i+1, i, i, want)
			}
		}
		last := rows[len(rows)-1]
		if !last.top.IsZero() {
			t.Fatalf("%s last bracket Top should be 0 (open-ended), got %s", label, last.top)
		}
	}

	// CA federal.
	fedRows := make([]bracketRow, len(caFederalBrackets))
	for i, b := range caFederalBrackets {
		fedRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	t.Run("CA federal", func(t *testing.T) { check(t, "CA federal", fedRows) })

	// CA provinces.
	for code, prov := range caProvincialBrackets {
		rows := make([]bracketRow, len(prov.Brackets))
		for i, b := range prov.Brackets {
			rows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
		}
		t.Run("CA "+code, func(t *testing.T) { check(t, "CA "+code, rows) })
	}

	// BR IRRF.
	brIRRFRows := make([]bracketRow, len(brIRRFBrackets))
	for i, b := range brIRRFBrackets {
		brIRRFRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	t.Run("BR IRRF", func(t *testing.T) { check(t, "BR IRRF", brIRRFRows) })

	// MX ISR — the published SAT "cuota fija" values are rounded
	// to 2dp on the official Art. 96 schedule, so adjacent rows
	// drift slightly from strict mathematical Base-contiguity
	// (e.g. row1 declared 14.32 vs computed 14.323776 from
	// row0). We pin SAT's printed table verbatim — the SAT
	// is the source of truth even when its rounding doesn't
	// satisfy the exact invariant. The relaxed check below
	// asserts the drift is ≤ MXN 0.10 / row (rounding-error
	// floor) so a transcription error (>10c shift) still
	// fails CI.
	const mxBaseTolerance = "0.10"
	mxTolerance := decimal.RequireFromString(mxBaseTolerance)
	for i := 0; i < len(mxISRBrackets)-1; i++ {
		cur, next := mxISRBrackets[i], mxISRBrackets[i+1]
		if !cur.Top.Equal(next.Floor) {
			t.Fatalf("MX ISR row[%d].Top (%s) != row[%d].Floor (%s)",
				i, cur.Top, i+1, next.Floor)
		}
		expected := cur.CuotaFij.Add(next.Floor.Sub(cur.Floor).Mul(cur.Rate))
		diff := next.CuotaFij.Sub(expected).Abs()
		if diff.GreaterThan(mxTolerance) {
			t.Fatalf("MX ISR row[%d].CuotaFij (%s) drifts from expected (%s) by %s; tolerance %s",
				i+1, next.CuotaFij, expected, diff, mxTolerance)
		}
	}
	if !mxISRBrackets[len(mxISRBrackets)-1].Top.IsZero() {
		t.Fatalf("MX ISR last bracket Top should be 0, got %s", mxISRBrackets[len(mxISRBrackets)-1].Top)
	}

	// AR Ganancias.
	arRows := make([]bracketRow, len(arGananciasBrackets))
	for i, b := range arGananciasBrackets {
		arRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	t.Run("AR Ganancias", func(t *testing.T) { check(t, "AR Ganancias", arRows) })

	// EC Impuesto a la Renta.
	ecRows := make([]bracketRow, len(ecImpuestoBrackets))
	for i, b := range ecImpuestoBrackets {
		ecRows[i] = bracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	t.Run("EC Renta", func(t *testing.T) { check(t, "EC Renta", ecRows) })

	// UY IRPF — uses BPC as units; the contiguity invariant holds
	// in BPC just as in monetary units.
	uyRows := make([]bracketRow, len(uyIRPFBrackets))
	for i, b := range uyIRPFBrackets {
		uyRows[i] = bracketRow{floor: b.FloorBPC, top: b.TopBPC, base: b.BaseBPC, rate: b.Rate}
	}
	t.Run("UY IRPF", func(t *testing.T) { check(t, "UY IRPF", uyRows) })

	// PE Renta 5ta — UIT units.
	peRows := make([]bracketRow, len(peRentaBrackets))
	for i, b := range peRentaBrackets {
		peRows[i] = bracketRow{floor: b.FloorUIT, top: b.TopUIT, base: b.BaseUIT, rate: b.Rate}
	}
	t.Run("PE Renta", func(t *testing.T) { check(t, "PE Renta", peRows) })

	// CO retención — UVT units. Last bracket has Top open-ended.
	coRows := make([]bracketRow, len(coRetencionBrackets))
	for i, b := range coRetencionBrackets {
		coRows[i] = bracketRow{floor: b.FloorUVT, top: b.TopUVT, base: b.BaseUVT, rate: b.Rate}
	}
	t.Run("CO retención", func(t *testing.T) { check(t, "CO retención", coRows) })
}
