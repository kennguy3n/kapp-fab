package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// egPack implements Egypt's monthly payroll-side statutory
// withholdings:
//
//   - Personal Income Tax per Income Tax Law 91/2005 (Article 8
//     bracket schedule, as amended by Law 175/2023 effective 1
//     January 2024 to raise the personal exemption from EGP
//     15,000 to EGP 20,000 and the first tax-exempt band from
//     EGP 30,000 to EGP 40,000). Seven progressive bands applied
//     to annualised taxable income (after the personal exemption
//     and statutory social-insurance deductions).
//
//   - Social Insurance Law 148/2019: 11% employee share applied
//     to the insured monthly wage between the lower limit (EGP
//     2,000 / month for 2025) and the upper limit (EGP 14,500 /
//     month for 2025; raised annually by 15% per the law's
//     escalator). Slips below the floor are not exempt — the
//     contribution is computed off the floor for "below-floor"
//     wages (Article 10(c)).
//
// References:
//
//	Income Tax Law 91/2005 + Law 175/2023:
//	  https://www.eta.gov.eg/en/legislation
//	Social Insurance Law 148/2019:
//	  https://www.nosi.gov.eg/en/social-insurance-law
type egPack struct{}

func init() { Register(&egPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services. Used by the registry to key lookups from
// tenants.country.
func (egPack) Country() string { return "EG" }

// EffectiveYear returns the fiscal year the EG tables are
// calibrated for: 2024 (post-Law-175/2023 bands + 2025 social-
// insurance ceilings).
func (egPack) EffectiveYear() int { return 2024 }

type egBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// ITL 91/2005 Article 8 schedule, as amended by Law
	// 175/2023 effective 1 Jan 2024. Annual taxable income in
	// EGP.
	egBrackets = []egBracket{
		{Floor: dec("0"), Top: dec("40000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("40000"), Top: dec("55000"), Base: dec("0"), Rate: dec("0.10")},
		{Floor: dec("55000"), Top: dec("70000"), Base: dec("1500"), Rate: dec("0.15")},
		{Floor: dec("70000"), Top: dec("200000"), Base: dec("3750"), Rate: dec("0.20")},
		{Floor: dec("200000"), Top: dec("400000"), Base: dec("29750"), Rate: dec("0.225")},
		{Floor: dec("400000"), Top: dec("1200000"), Base: dec("74750"), Rate: dec("0.25")},
		{Floor: dec("1200000"), Top: decimal.Zero, Base: dec("274750"), Rate: dec("0.275")},
	}

	// Personal exemption EGP 20,000 annual per Law 175/2023.
	egPersonalExemptionAnnual = dec("20000")

	// Social insurance: 11% employee, lower limit EGP 2,000 /
	// month, upper limit EGP 14,500 / month for 2025.
	egSIRate         = dec("0.11")
	egSILowerMonthly = dec("2000")
	egSIUpperMonthly = dec("14500")

	egAnnualPeriodFraction = decimal.NewFromFloat(365.25)
	egMonthlyDaysApprox    = decimal.NewFromFloat(30.4375)
)

// ComputeWithholding emits EG_SOCIAL_INSURANCE and EG_PIT.
// Social insurance is computed off the floored/capped monthly
// equivalent; PIT is annualised against the slip's gross minus
// the social-insurance line, the personal exemption is then
// subtracted, and the result is walked against the brackets.
// Zero-amount lines are omitted.
func (egPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	monthlyEquiv := gross.Mul(egMonthlyDaysApprox).Div(decimal.NewFromInt(int64(days)))

	// Social insurance: clamp monthly equivalent to [floor, ceiling].
	siBase := monthlyEquiv
	if siBase.LessThan(egSILowerMonthly) {
		siBase = egSILowerMonthly
	}
	if siBase.GreaterThan(egSIUpperMonthly) {
		siBase = egSIUpperMonthly
	}
	siMonthly := siBase.Mul(egSIRate)
	siPeriod := siMonthly.Mul(decimal.NewFromInt(int64(days))).Div(egMonthlyDaysApprox).Round(2)
	if siPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "EG_SOCIAL_INSURANCE",
			Name:   "Social Insurance (employee, EG)",
			Amount: siPeriod,
		})
	}

	// PIT: annualise (gross - SI) and subtract personal exemption.
	periodFraction := decimal.NewFromInt(int64(days)).Div(egAnnualPeriodFraction)
	netForPIT := gross.Sub(siPeriod)
	if netForPIT.LessThan(decimal.Zero) {
		netForPIT = decimal.Zero
	}
	annualGross := netForPIT.Div(periodFraction)
	taxableAnnual := annualGross.Sub(egPersonalExemptionAnnual)
	if taxableAnnual.LessThan(decimal.Zero) {
		taxableAnnual = decimal.Zero
	}
	annualTax := walkEGBrackets(taxableAnnual)
	periodPIT := annualTax.Mul(periodFraction).Round(2)
	if periodPIT.IsPositive() {
		out = append(out, Deduction{
			Code:   "EG_PIT",
			Name:   "Personal Income Tax (EG)",
			Amount: periodPIT,
		})
	}

	return out, nil
}

func walkEGBrackets(annual decimal.Decimal) decimal.Decimal {
	var match egBracket
	matched := false
	for _, b := range egBrackets {
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
