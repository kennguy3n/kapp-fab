package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// mmPack implements Myanmar's payroll-side statutory withholdings:
//
//   - Income tax on salary per the Union Tax Law (2023/2024) and
//     the Income Tax Law. The pack annualises the slip's gross
//     (days / 365.25), applies the basic personal relief of 20% of
//     total salary income (capped at MMK 10,000,000/year), the
//     spouse relief and per-child relief, walks the six-band
//     progressive schedule (MMK) and prorates the annual tax back
//     to the slip period.
//
//   - Social Security Board (SSB) employee contribution per the
//     Social Security Law 2012: 2% of the monthly wage, capped at a
//     contributory wage of MMK 300,000/month (so a maximum employee
//     contribution of MMK 6,000/month). The employer's 3% share is
//     not a slip deduction.
//
// Non-residents (foreigners in Myanmar < 183 days) are taxed on the
// same progressive schedule with no personal relief (Union Tax Law);
// the pack applies the bracket walk to the full annualised gross for
// non-residents and emits no SSB.
//
// References:
//
//	Union Tax Law (annual; income-tax bands + reliefs):
//	  https://www.ird.gov.mm/en
//	Income Tax Law (Pyidaungsu Hluttaw Law, reliefs Art. 6):
//	  https://www.ird.gov.mm/en
//	Social Security Law 2012 (SSB 2% employee / 3% employer):
//	  https://www.ssb.gov.mm/
type mmPack struct{}

func init() { Register(&mmPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (mmPack) Country() string { return "MM" }

// EffectiveYear returns the calendar year the MM tables are
// calibrated for: 2024 (Union Tax Law 2023 income-tax bands for
// the 2024-2025 assessment year; SSB 2% employee schedule).
func (mmPack) EffectiveYear() int { return 2024 }

type mmBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Union Tax Law progressive schedule (annual taxable income,
	// MMK). Base is cumulative tax at each Floor.
	mmBracketsResident = []mmBracket{
		{Floor: dec("0"), Top: dec("2000000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("2000000"), Top: dec("5000000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("5000000"), Top: dec("10000000"), Base: dec("150000"), Rate: dec("0.10")},
		{Floor: dec("10000000"), Top: dec("20000000"), Base: dec("650000"), Rate: dec("0.15")},
		{Floor: dec("20000000"), Top: dec("30000000"), Base: dec("2150000"), Rate: dec("0.20")},
		{Floor: dec("30000000"), Top: decimal.Zero, Base: dec("4150000"), Rate: dec("0.25")},
	}

	// Basic personal relief: 20% of total salary income, capped.
	mmPersonalReliefRate = dec("0.20")
	mmPersonalReliefCap  = dec("10000000")

	// Spouse relief (when the employee has dependents) and per-child
	// relief (Income Tax Law Art. 6).
	mmSpouseRelief = dec("1000000")
	mmChildRelief  = dec("500000")

	// SSB employee contribution: 2% of the monthly wage, capped at a
	// contributory wage of MMK 300,000/month.
	mmSSBEmployeeRate    = dec("0.02")
	mmSSBContributoryCap = dec("300000")

	mmMaxDependents = 15

	mmAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits MM_INCOME_TAX (annualised progressive)
// and MM_SSB (Social Security Board employee contribution) for
// residents. Non-residents get the bracket walk with no reliefs and
// no SSB.
func (mmPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(mmAnnualPeriodFraction)
	annualGross := gross.Div(periodFraction)

	var taxable decimal.Decimal
	if e.Resident {
		relief := annualGross.Mul(mmPersonalReliefRate)
		if relief.GreaterThan(mmPersonalReliefCap) {
			relief = mmPersonalReliefCap
		}
		deps := e.NumDependents
		if deps < 0 {
			deps = 0
		}
		if deps > mmMaxDependents {
			deps = mmMaxDependents
		}
		// When the employee supports dependents, the spouse relief
		// applies once and the child relief per dependent.
		if deps > 0 {
			relief = relief.Add(mmSpouseRelief)
			relief = relief.Add(mmChildRelief.Mul(decimal.NewFromInt(int64(deps))))
		}
		taxable = annualGross.Sub(relief)
	} else {
		// Non-residents: progressive schedule on full income, no
		// relief.
		taxable = annualGross
	}
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}

	annualTax := walkMMBrackets(taxable, mmBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "MM_INCOME_TAX",
			Name:   "Income tax on salary (MM)",
			Amount: periodTax,
		})
	}

	if !e.Resident {
		return out, nil
	}

	// SSB employee contribution: 2% of monthly wage, capped.
	ssbBase := gross
	if ssbBase.GreaterThan(mmSSBContributoryCap) {
		ssbBase = mmSSBContributoryCap
	}
	ssb := ssbBase.Mul(mmSSBEmployeeRate).Round(2)
	if ssb.IsPositive() {
		out = append(out, Deduction{
			Code:   "MM_SSB",
			Name:   "Social Security Board contribution (employee, MM)",
			Amount: ssb,
		})
	}

	return out, nil
}

func walkMMBrackets(annual decimal.Decimal, scale []mmBracket) decimal.Decimal {
	var match mmBracket
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
