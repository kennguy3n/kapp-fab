package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// joPack implements Jordan's payroll-side statutory withholdings:
//
//   - Income tax on employment income per Income Tax Law No. 34 of
//     2014 (as amended by Law No. 38 of 2018). The pack annualises
//     the slip's gross (days / 365.25), applies the personal
//     exemption (JOD 9,000) plus a family exemption (a further
//     JOD 9,000 when the employee supports dependents), walks the
//     progressive schedule (JOD), adds the 1% national-contribution
//     surtax on taxable income, and prorates the annual liability
//     back to the slip period.
//
//   - Social Security Corporation (SSC) employee contribution per
//     the Social Security Law: 7.5% of the subscription wage, capped
//     at the SSC maximum monthly subscription wage (JOD 3,484). The
//     employer's 14.25% share is not a slip deduction.
//
// References:
//
//	Income Tax Law No. 34/2014 (as amended by 38/2018) + ISTD:
//	  https://www.istd.gov.jo/
//	Social Security Corporation contribution rates:
//	  https://www.ssc.gov.jo/
type joPack struct{}

func init() { Register(&joPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (joPack) Country() string { return "JO" }

// EffectiveYear returns the calendar year the JO rates are
// calibrated for: 2025 (income-tax bands + 1% national contribution
// per Law 38/2018; SSC subscription-wage ceiling JOD 3,484).
func (joPack) EffectiveYear() int { return 2025 }

type joBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Income Tax Law progressive schedule (annual taxable income
	// after exemptions, JOD). Base is cumulative tax at each Floor.
	joBracketsResident = []joBracket{
		{Floor: dec("0"), Top: dec("5000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("5000"), Top: dec("10000"), Base: dec("250"), Rate: dec("0.10")},
		{Floor: dec("10000"), Top: dec("15000"), Base: dec("750"), Rate: dec("0.15")},
		{Floor: dec("15000"), Top: dec("20000"), Base: dec("1500"), Rate: dec("0.20")},
		{Floor: dec("20000"), Top: dec("1000000"), Base: dec("2500"), Rate: dec("0.25")},
		{Floor: dec("1000000"), Top: decimal.Zero, Base: dec("247500"), Rate: dec("0.30")},
	}

	// Personal and family exemptions (annual, JOD).
	joPersonalExemption = dec("9000")
	joFamilyExemption   = dec("9000")

	// National contribution surtax on taxable income (Law 38/2018).
	joNationalContributionRate = dec("0.01")

	// SSC employee contribution: 7.5% of subscription wage, capped.
	joSSCEmployeeRate = dec("0.075")
	joSSCWageCeiling  = dec("3484")

	joAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits JO_INCOME_TAX (annualised progressive tax
// plus the 1% national contribution) and JO_SSC (Social Security
// Corporation employee contribution).
func (joPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(joAnnualPeriodFraction)
	annualGross := gross.Div(periodFraction)

	exemption := joPersonalExemption
	if e.NumDependents > 0 {
		exemption = exemption.Add(joFamilyExemption)
	}
	taxable := annualGross.Sub(exemption)
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	annualTax := walkJOBrackets(taxable, joBracketsResident)
	// 1% national contribution on taxable income.
	annualTax = annualTax.Add(taxable.Mul(joNationalContributionRate))
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "JO_INCOME_TAX",
			Name:   "Income tax + national contribution (JO)",
			Amount: periodTax,
		})
	}

	// SSC employee contribution on the capped subscription wage.
	sscBase := gross
	if sscBase.GreaterThan(joSSCWageCeiling) {
		sscBase = joSSCWageCeiling
	}
	ssc := sscBase.Mul(joSSCEmployeeRate).Round(2)
	if ssc.IsPositive() {
		out = append(out, Deduction{
			Code:   "JO_SSC",
			Name:   "Social Security contribution (employee, JO)",
			Amount: ssc,
		})
	}

	return out, nil
}

func walkJOBrackets(annual decimal.Decimal, scale []joBracket) decimal.Decimal {
	var match joBracket
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
