package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// roPack implements Romania's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - CAS (Contribuția de asigurări sociale, pension): 25% on
//     gross earnings. Employee share; the employer pays an
//     additional 2.25% / 4% / 8% asigurări pentru muncă
//     (varies by working conditions) which is out of scope.
//
//   - CASS (Contribuția de asigurări sociale de sănătate,
//     health): 10% on gross earnings.
//
//   - Impozit pe venit (income tax): flat 10% on the income
//     base = gross − CAS − CASS − personal deduction
//     (deducere personală). 2025 deducere personală for
//     income below RON 4,500 / mo is RON 600 / mo (single
//     filer, no dependants); this pack applies the flat
//     RON 600 deduction for all employees as a representative
//     baseline. Real payroll uses scaling tables keyed by
//     income band + dependants count.
//
// Period-fraction handling: CAS and CASS are flat percentages of
// the period gross, so they're emitted period-local with no
// annualization. The personal deduction, however, is defined as a
// *monthly* amount and must scale with the actual pay-period length
// — otherwise a 14-day slip over-deducts the deduction (full 600
// RON applied to a half-month base) and under-withholds impozit
// versus a monthly slip at the same daily rate. We therefore
// annualize the impozit base (gross / period-fraction), apply the
// annual deduction (600 × 12 = 7,200 RON/yr), then prorate the
// resulting annual tax back by the same period-fraction. This
// matches the annualize→compute→prorate pattern used by every
// other Phase-N2 pack with a fixed deduction or credit (PL, CZ,
// DK, SE, NO, FI).
//
// References:
//
//	ANAF — venituri din salarii 2025:
//	  https://static.anaf.ro/static/10/Anaf/AsisContrib/Contributii_salarii.htm
//	Codul fiscal 2025, art. 78 (deduceri personale):
//	  https://static.anaf.ro/static/10/Anaf/legislatie/Cod_fiscal.htm
type roPack struct{}

func init() { Register(&roPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (roPack) Country() string { return "RO" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (ANAF + CAS + CASS 2025 cote).
func (roPack) EffectiveYear() int { return 2025 }

var (
	roCASRate           = dec("0.25")
	roCASSRate          = dec("0.10")
	roImpozitRate       = dec("0.10")
	roDeducerePersonala = dec("600") // monthly, single, no dependants
	roAnnualDays        = decimal.NewFromFloat(365.25)
)

// roAnnualDeducerePersonala is the annual equivalent of the monthly
// 600 RON personal deduction used by the annualize/prorate path.
var roAnnualDeducerePersonala = roDeducerePersonala.Mul(decimal.NewFromInt(12))

// ComputeWithholding emits up to three lines:
//
//   - RO_CAS     (pension 25% on gross)
//   - RO_CASS    (health 10% on gross)
//   - RO_IMPOZIT (income tax 10% on (gross - CAS - CASS - personal deduction))
//
// Negative or zero gross returns nil.
func (roPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(roAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}

	out := []Deduction{}

	cas := gross.Mul(roCASRate).Round(2)
	if cas.IsPositive() {
		out = append(out, Deduction{
			Code:   "RO_CAS",
			Name:   "CAS pension contribution (RO)",
			Amount: cas,
		})
	}

	cass := gross.Mul(roCASSRate).Round(2)
	if cass.IsPositive() {
		out = append(out, Deduction{
			Code:   "RO_CASS",
			Name:   "CASS health contribution (RO)",
			Amount: cass,
		})
	}

	// Annualize gross + the per-period CAS/CASS to compute an
	// annual tax base, then prorate the resulting annual impozit
	// back by periodFraction. The annual personal deduction
	// (7,200 RON/yr) is applied at the annual layer so a 14-day
	// slip only gets ~276 RON of the deduction, not the full 600.
	annualGross := gross.Div(periodFraction)
	annualCAS := cas.Div(periodFraction)
	annualCASS := cass.Div(periodFraction)
	annualTaxBase := annualGross.Sub(annualCAS).Sub(annualCASS).Sub(roAnnualDeducerePersonala)
	if annualTaxBase.LessThan(decimal.Zero) {
		annualTaxBase = decimal.Zero
	}
	annualImpozit := annualTaxBase.Mul(roImpozitRate)
	impozit := annualImpozit.Mul(periodFraction).Round(2)
	if impozit.IsPositive() {
		out = append(out, Deduction{
			Code:   "RO_IMPOZIT",
			Name:   "Impozit pe venit (RO)",
			Amount: impozit,
		})
	}

	return out, nil
}
