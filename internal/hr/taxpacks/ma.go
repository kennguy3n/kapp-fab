package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// maPack implements Morocco's payroll-side statutory withholdings:
//
//   - Income tax (Impôt sur le Revenu, IR) on salary per the Code
//     Général des Impôts, using the schedule re-scaled by the
//     Finance Law 2025 (Loi de Finances 2025). The pack annualises
//     the slip's gross (days / 365.25), applies the standard
//     professional-expenses deduction (frais professionnels, 20%
//     capped at MAD 30,000/year), walks the progressive schedule
//     (MAD), subtracts the per-dependent family tax reduction
//     (MAD 500/year each, capped at 6 dependents), and prorates the
//     annual tax back to the slip period.
//
//   - CNSS (Caisse Nationale de Sécurité Sociale) employee
//     contribution: the short- and long-term social-benefit share
//     of 4.48% on the contributory wage capped at MAD 6,000/month.
//
//   - AMO (Assurance Maladie Obligatoire) employee contribution:
//     2.26% of gross salary with no ceiling.
//
// References:
//
//	Code Général des Impôts + Loi de Finances 2025 (IR barème):
//	  https://www.tax.gov.ma/
//	Direction Générale des Impôts — barème IR:
//	  https://www.tax.gov.ma/wps/portal/DGI/
//	CNSS contribution rates (CNSS + AMO):
//	  https://www.cnss.ma/
type maPack struct{}

func init() { Register(&maPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (maPack) Country() string { return "MA" }

// EffectiveYear returns the calendar year the MA schedule is
// calibrated for: 2025 (Loi de Finances 2025 IR barème; CNSS/AMO
// 2025 rates and ceiling).
func (maPack) EffectiveYear() int { return 2025 }

type maBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Loi de Finances 2025 IR progressive schedule (annual net
	// taxable income, MAD). Base is cumulative tax at each Floor.
	maBracketsResident = []maBracket{
		{Floor: dec("0"), Top: dec("40000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("40000"), Top: dec("60000"), Base: dec("0"), Rate: dec("0.10")},
		{Floor: dec("60000"), Top: dec("80000"), Base: dec("2000"), Rate: dec("0.20")},
		{Floor: dec("80000"), Top: dec("100000"), Base: dec("6000"), Rate: dec("0.30")},
		{Floor: dec("100000"), Top: dec("180000"), Base: dec("12000"), Rate: dec("0.34")},
		{Floor: dec("180000"), Top: decimal.Zero, Base: dec("39200"), Rate: dec("0.37")},
	}

	// Standard professional-expenses deduction: 20% of gross,
	// capped at MAD 30,000/year.
	maProfessionalDeductionRate = dec("0.20")
	maProfessionalDeductionCap  = dec("30000")

	// Family tax reduction: MAD 500/year per dependent, capped at
	// six dependents.
	maFamilyReductionPerDependent = dec("500")
	maMaxFamilyDependents         = 6

	// CNSS employee social share: 4.48% capped at MAD 6,000/month.
	maCNSSEmployeeRate = dec("0.0448")
	maCNSSWageCeiling  = dec("6000")

	// AMO employee health share: 2.26%, no ceiling.
	maAMOEmployeeRate = dec("0.0226")

	maAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits MA_IR (annualised progressive income
// tax, net of professional and family deductions), MA_CNSS (capped
// social-security employee share) and MA_AMO (health-insurance
// employee share).
func (maPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(maAnnualPeriodFraction)
	// IR runs on the post-pre-tax base; CNSS / AMO keep the full gross.
	annualGross := e.IncomeTaxBase(gross).Div(periodFraction)

	profDeduction := annualGross.Mul(maProfessionalDeductionRate)
	if profDeduction.GreaterThan(maProfessionalDeductionCap) {
		profDeduction = maProfessionalDeductionCap
	}
	taxable := annualGross.Sub(profDeduction)
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	annualTax := walkMABrackets(taxable, maBracketsResident)

	// Family tax reduction (applied after the bracket walk).
	deps := e.NumDependents
	if deps < 0 {
		deps = 0
	}
	if deps > maMaxFamilyDependents {
		deps = maMaxFamilyDependents
	}
	annualTax = annualTax.Sub(maFamilyReductionPerDependent.Mul(decimal.NewFromInt(int64(deps))))
	if annualTax.IsNegative() {
		annualTax = decimal.Zero
	}
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "MA_IR",
			Name:   "Income tax IR (MA)",
			Amount: periodTax,
		})
	}

	// CNSS + AMO run on the contribution base (full gross by default).
	contribGross := e.ContributionBase(gross)
	cnssBase := contribGross
	if cnssBase.GreaterThan(maCNSSWageCeiling) {
		cnssBase = maCNSSWageCeiling
	}
	cnss := cnssBase.Mul(maCNSSEmployeeRate).Round(2)
	if cnss.IsPositive() {
		out = append(out, Deduction{
			Code:   "MA_CNSS",
			Name:   "CNSS social security (employee, MA)",
			Amount: cnss,
		})
	}

	amo := contribGross.Mul(maAMOEmployeeRate).Round(2)
	if amo.IsPositive() {
		out = append(out, Deduction{
			Code:   "MA_AMO",
			Name:   "AMO compulsory health insurance (employee, MA)",
			Amount: amo,
		})
	}

	return out, nil
}

func walkMABrackets(annual decimal.Decimal, scale []maBracket) decimal.Decimal {
	var match maBracket
	matched := false
	for _, b := range scale {
		if annual.LessThanOrEqual(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.Base.Add(annual.Sub(match.Floor).Mul(match.Rate))
}
