package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// iePack implements Ireland's payroll-side statutory
// withholdings for the 2025 fiscal year:
//
//   - PAYE (Pay As You Earn) income tax. Single-person Standard
//     Rate Cut-Off Point (SRCOP) for 2025 is €44,000; income
//     within the SRCOP is taxed at 20%, income above at 40%.
//     The personal tax credit (single) is €2,000 / year and the
//     PAYE (Employee) tax credit is €2,000 / year, for €4,000
//     of credits applied against gross tax.
//
//   - USC (Universal Social Charge). 2025 schedule:
//     0       → 12,012   0.5%
//     12,012  → 27,382   2.0%
//     27,382  → 70,044   4.0%
//     70,044  → open     8.0%
//     The first €13,000 of income is fully exempt — employees
//     earning under that threshold pay no USC at all.
//
//   - PRSI (Pay-Related Social Insurance) Class A — the standard
//     class for private-sector employees. The 2025 employee
//     rate is 4.1% (raised from 4.0% on 1 October 2024). No
//     ceiling. The PRSI credit phases out €12 per week of pay
//     between €352.01 and €424; this pack uses a uniform 4.1%
//     for simplicity, matching the Revenue Commissioners'
//     weekly cycle-A tables for the bulk of employees.
//
// References:
//
//	Revenue PAYE Manual 2025:
//	  https://www.revenue.ie/en/employing-people/paye-employers/employee-pay-and-tax-credits/index.aspx
//	USC rates and bands 2025:
//	  https://www.revenue.ie/en/jobs-and-pensions/usc/usc-rates-and-bands.aspx
//	PRSI Class A 2025:
//	  https://www.gov.ie/en/publication/da7a0-prsi-class-a-rates-of-contribution/
type iePack struct{}

func init() { Register(&iePack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (iePack) Country() string { return "IE" }

// EffectiveYear returns the fiscal year the IE tables are
// calibrated for: 2025 (Revenue PAYE 2025 + Finance Act 2024
// USC + DSP PRSI 2025).
func (iePack) EffectiveYear() int { return 2025 }

type ieBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// PAYE Standard Rate Cut-Off Point (single, 2025).
	ieSRCOP        = dec("44000")
	iePAYELowRate  = dec("0.20")
	iePAYEHighRate = dec("0.40")
	// Tax credits applied annually against gross PAYE liability.
	iePersonalCredit = dec("2000") // Personal Tax Credit (single)
	iePAYECredit     = dec("2000") // Employee (PAYE) Tax Credit
	ieTotalCredit    = iePersonalCredit.Add(iePAYECredit)

	ieUSCBrackets = []ieBracket{
		{Floor: dec("0"), Top: dec("12012"), Base: dec("0"), Rate: dec("0.005")},
		{Floor: dec("12012"), Top: dec("27382"), Base: dec("60.06"), Rate: dec("0.02")},
		{Floor: dec("27382"), Top: dec("70044"), Base: dec("367.46"), Rate: dec("0.04")},
		{Floor: dec("70044"), Top: decimal.Zero, Base: dec("2073.94"), Rate: dec("0.08")},
	}
	ieUSCExemptionThreshold = dec("13000")

	iePRSIRate = dec("0.041")

	ieAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to three lines:
//
//   - IE_PAYE  (Income Tax at 20% under the SRCOP / 40% above)
//   - IE_USC   (Universal Social Charge progressive bands, with
//     full exemption under €13,000 of annual gross)
//   - IE_PRSI  (Pay-Related Social Insurance Class A1, 4.1% employee
//     share)
//
// Negative or zero gross / period return nil.
func (iePack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(ieAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// PAYE: 20% to SRCOP, 40% above; credit applied against
	// annual gross tax (not refundable).
	var annualPAYE decimal.Decimal
	if annualGross.LessThanOrEqual(ieSRCOP) {
		annualPAYE = annualGross.Mul(iePAYELowRate)
	} else {
		annualPAYE = ieSRCOP.Mul(iePAYELowRate).Add(
			annualGross.Sub(ieSRCOP).Mul(iePAYEHighRate),
		)
	}
	annualPAYE = annualPAYE.Sub(ieTotalCredit)
	if annualPAYE.LessThan(decimal.Zero) {
		annualPAYE = decimal.Zero
	}
	periodPAYE := annualPAYE.Mul(periodFraction).Round(2)
	if periodPAYE.IsPositive() {
		out = append(out, Deduction{
			Code:   "IE_PAYE",
			Name:   "PAYE (IE)",
			Amount: periodPAYE,
		})
	}

	// USC — exempt below €13,000 / yr; otherwise bracket walk.
	if annualGross.GreaterThan(ieUSCExemptionThreshold) {
		annualUSC := walkIEBrackets(annualGross, ieUSCBrackets)
		periodUSC := annualUSC.Mul(periodFraction).Round(2)
		if periodUSC.IsPositive() {
			out = append(out, Deduction{
				Code:   "IE_USC",
				Name:   "USC (Universal Social Charge, IE)",
				Amount: periodUSC,
			})
		}
	}

	// PRSI Class A — flat 4.1% on slip gross, no cap.
	if prsi := gross.Mul(iePRSIRate).Round(2); prsi.IsPositive() {
		out = append(out, Deduction{
			Code:   "IE_PRSI",
			Name:   "PRSI Class A (employee, IE)",
			Amount: prsi,
		})
	}

	return out, nil
}

// walkIEBrackets walks the USC schedule.
func walkIEBrackets(annual decimal.Decimal, brackets []ieBracket) decimal.Decimal {
	var match ieBracket
	matched := false
	for _, b := range brackets {
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
