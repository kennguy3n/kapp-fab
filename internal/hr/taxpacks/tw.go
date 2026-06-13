package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// twPack implements Taiwan's monthly payroll-side statutory
// withholdings:
//
//   - Salary income tax withholding (薪資所得扣繳) per the Income
//     Tax Act (所得稅法) Article 14 / 17. The pack annualises the
//     slip's gross (days / 365.25), subtracts the resident's
//     standard deduction, personal exemption(s) and the salary-
//     income special deduction, walks the five-band progressive
//     schedule (Article 5), and prorates the annual tax back to the
//     slip period. The employer's monthly withholding is a
//     provisional figure reconciled on the taxpayer's annual
//     settlement return (結算申報) the following May.
//
//   - Labor Insurance + Employment Insurance (勞工保險 + 就業保險,
//     LI/EI) employee share. The 2025 combined ordinary premium
//     rate is 12% (11% labour-insurance ordinary-risk + 1%
//     employment-insurance); the employee bears 20% of the premium
//     → 2.4% of the insured monthly salary, capped at the LI
//     maximum insured-salary grade (NT$45,800/month).
//
//   - National Health Insurance (全民健康保險, NHI) employee share.
//     The 2025 premium rate is 5.17%; the employee bears 30% →
//     1.551% of the insured monthly salary, capped at the NHI
//     maximum insured-salary grade (NT$219,500/month). The 2.11%
//     supplementary premium on irregular bonuses is billed
//     separately and is not a regular-slip deduction.
//
// The graded insured-salary tables (投保薪資分級表) round the
// employee's salary up to the next published grade; the pack uses
// min(gross, ceiling) as the insured-salary proxy, which matches
// the published grade for salaries at a grade boundary and is the
// same conservative simplification the JP / KR packs use for their
// standard-remuneration proxies.
//
// Non-residents (in Taiwan < 183 days in the tax year) are withheld
// at a flat rate under the Standards of Withholding Rates for
// Various Incomes Article 3: 6% when monthly salary does not exceed
// 1.5× the minimum monthly wage, otherwise 18%. Non-resident slips
// emit only the flat withholding line (LI / NHI obligations for
// short-term foreign workers are handled by the engine's foreign-
// worker slip type, not this pack).
//
// References:
//
//	Income Tax Act (所得稅法) Articles 5 / 14 / 17:
//	  https://law.moj.gov.tw/ENG/LawClass/LawAll.aspx?pcode=G0340003
//	Bureau of Labor Insurance premium rates (2025):
//	  https://www.bli.gov.tw/en/
//	NHIA premium rate + insured-salary grades (2025):
//	  https://www.nhi.gov.tw/en/
//	Standards of Withholding Rates for Various Incomes Article 3:
//	  https://law.moj.gov.tw/ENG/LawClass/LawAll.aspx?pcode=G0340072
type twPack struct{}

func init() { Register(&twPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (twPack) Country() string { return "TW" }

// EffectiveYear returns the calendar year the TW rates and brackets
// are calibrated for: 2025 (income-tax bands per the 2024-income /
// 2025-filing schedule, LI 12% combined premium, NHI 5.17%).
func (twPack) EffectiveYear() int { return 2025 }

type twBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Income Tax Act Article 5 progressive schedule (annual
	// taxable income, NT$). Base is cumulative tax at each Floor.
	twBracketsResident = []twBracket{
		{Floor: dec("0"), Top: dec("590000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("590000"), Top: dec("1330000"), Base: dec("29500"), Rate: dec("0.12")},
		{Floor: dec("1330000"), Top: dec("2660000"), Base: dec("118300"), Rate: dec("0.20")},
		{Floor: dec("2660000"), Top: dec("4980000"), Base: dec("384300"), Rate: dec("0.30")},
		{Floor: dec("4980000"), Top: decimal.Zero, Base: dec("1080300"), Rate: dec("0.40")},
	}

	// Resident annual deductions (2024-income / 2025-filing).
	twPersonalExemption      = dec("97000")  // per person (self + each dependent)
	twStandardDeduction      = dec("131000") // single filer
	twSalarySpecialDeduction = dec("218000") // salary-income special deduction

	// Labor + Employment Insurance employee share: 12% combined
	// ordinary premium × 20% employee burden = 2.4%, capped at the
	// LI maximum insured-salary grade.
	twLaborInsuranceRate    = dec("0.024")
	twLaborInsuranceCeiling = dec("45800")

	// National Health Insurance employee share: 5.17% × 30% =
	// 1.551%, capped at the NHI maximum insured-salary grade.
	twNHIRate    = dec("0.01551")
	twNHICeiling = dec("219500")

	// Non-resident flat withholding (Standards Article 3): 6% up
	// to 1.5× the minimum monthly wage (NT$28,590 in 2025 →
	// NT$42,885), 18% above.
	twNonResidentLowRate   = dec("0.06")
	twNonResidentHighRate  = dec("0.18")
	twNonResidentThreshold = dec("42885")

	// Dependents above this count are almost always a data-entry
	// error; clamp so a fat-fingered field cannot zero out the tax
	// base via unbounded personal exemptions.
	twMaxDependents = 15

	twAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits TW_INCOME_TAX (annualised progressive
// withholding), TW_LABOR_INSURANCE (LI + EI employee share), and
// TW_NHI (health-insurance employee share) for residents.
// Non-residents get a single TW_NONRESIDENT_TAX flat-rate line and
// no social-insurance contributions.
func (twPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Non-resident flat withholding (Standards Article 3).
	if !e.Resident {
		itGross := e.IncomeTaxBase(gross)
		rate := twNonResidentHighRate
		if itGross.LessThanOrEqual(twNonResidentThreshold) {
			rate = twNonResidentLowRate
		}
		nr := itGross.Mul(rate).Round(2)
		if nr.IsPositive() {
			out = append(out, Deduction{
				Code:   "TW_NONRESIDENT_TAX",
				Name:   "Non-resident withholding tax (TW)",
				Amount: nr,
			})
		}
		return out, nil
	}

	// Income tax: annualise, subtract deductions, walk brackets,
	// prorate back to the slip period.
	periodFraction := decimal.NewFromInt(int64(days)).Div(twAnnualPeriodFraction)
	// Income tax runs on the post-pre-tax base; Labor/Employment/Health
	// insurance keep the full gross.
	annualGross := e.IncomeTaxBase(gross).Div(periodFraction)

	deps := e.NumDependents
	if deps < 0 {
		deps = 0
	}
	if deps > twMaxDependents {
		deps = twMaxDependents
	}
	// Personal exemption covers the employee plus each dependent.
	exemptions := twPersonalExemption.Mul(decimal.NewFromInt(int64(deps + 1)))
	taxable := annualGross.Sub(exemptions).Sub(twStandardDeduction).Sub(twSalarySpecialDeduction)
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	annualTax := walkTWBrackets(taxable, twBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "TW_INCOME_TAX",
			Name:   "Salary income tax withholding (TW)",
			Amount: periodTax,
		})
	}

	// Labor + Employment + Health Insurance run on the contribution base
	// (full gross by default).
	contribGross := e.ContributionBase(gross)

	// Labor + Employment Insurance employee share. The slip's gross
	// is the insured-salary proxy; cap at the LI maximum grade.
	liBase := contribGross
	if liBase.GreaterThan(twLaborInsuranceCeiling) {
		liBase = twLaborInsuranceCeiling
	}
	li := liBase.Mul(twLaborInsuranceRate).Round(2)
	if li.IsPositive() {
		out = append(out, Deduction{
			Code:   "TW_LABOR_INSURANCE",
			Name:   "Labor + Employment Insurance (employee, TW)",
			Amount: li,
		})
	}

	// National Health Insurance employee share; cap at the NHI
	// maximum grade.
	nhiBase := contribGross
	if nhiBase.GreaterThan(twNHICeiling) {
		nhiBase = twNHICeiling
	}
	nhi := nhiBase.Mul(twNHIRate).Round(2)
	if nhi.IsPositive() {
		out = append(out, Deduction{
			Code:   "TW_NHI",
			Name:   "National Health Insurance (employee, TW)",
			Amount: nhi,
		})
	}

	return out, nil
}

func walkTWBrackets(annual decimal.Decimal, scale []twBracket) decimal.Decimal {
	var match twBracket
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
