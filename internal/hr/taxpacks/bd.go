package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// bdPack implements Bangladesh's payroll-side statutory income-tax
// deduction at source on salary per the Income Tax Act 2023 (আয়কর
// আইন ২০২৩) and the annual Finance Act.
//
// The pack annualises the slip's gross (days / 365.25), applies the
// tax-free threshold via the zero-rate first band, walks the
// progressive schedule (BDT) and prorates the annual tax back to
// the slip period. Bangladesh has no general mandatory social-
// insurance payroll withholding for private-sector employees
// (provident-fund participation is employer-scheme-specific and
// voluntary), so income tax is the only statutory slip deduction.
//
// The default tax-free threshold modelled here is the general
// taxpayer threshold (BDT 350,000). Higher thresholds for women /
// senior citizens (BDT 400,000), persons with disability and
// gazetted freedom fighters are policy-flag driven and are layered
// on by the engine's exemption profile rather than hard-coded per
// slip.
//
// A non-resident individual is taxed at a flat maximum rate of 30%
// on Bangladesh-source income (Income Tax Act s. 2(48) / Part 7);
// the pack emits that flat line for non-residents.
//
// References:
//
//	Income Tax Act 2023 + Finance Act (annual slabs):
//	  https://nbr.gov.bd/
//	National Board of Revenue individual tax rates:
//	  https://nbr.gov.bd/taxtypes/income-tax/eng
type bdPack struct{}

func init() { Register(&bdPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (bdPack) Country() string { return "BD" }

// EffectiveYear returns the calendar year the BD slabs are
// calibrated for: 2024 (Finance Act 2024, assessment year
// 2024-2025).
func (bdPack) EffectiveYear() int { return 2024 }

type bdBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Finance Act progressive schedule (annual taxable income,
	// BDT). Base is cumulative tax at each Floor. The first band is
	// the general tax-free threshold.
	bdBracketsResident = []bdBracket{
		{Floor: dec("0"), Top: dec("350000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("350000"), Top: dec("450000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("450000"), Top: dec("850000"), Base: dec("5000"), Rate: dec("0.10")},
		{Floor: dec("850000"), Top: dec("1350000"), Base: dec("45000"), Rate: dec("0.15")},
		{Floor: dec("1350000"), Top: dec("1850000"), Base: dec("120000"), Rate: dec("0.20")},
		{Floor: dec("1850000"), Top: decimal.Zero, Base: dec("220000"), Rate: dec("0.25")},
	}

	// Non-resident flat maximum rate on Bangladesh-source income.
	bdNonResidentRate = dec("0.30")

	bdAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits BD_INCOME_TAX (annualised progressive
// deduction at source) for residents, or BD_NONRESIDENT_TAX (flat
// 30%) for non-residents.
func (bdPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	if !e.Resident {
		nr := e.IncomeTaxBase(gross).Mul(bdNonResidentRate).Round(2)
		if !nr.IsPositive() {
			return nil, nil
		}
		return []Deduction{{
			Code:   "BD_NONRESIDENT_TAX",
			Name:   "Non-resident withholding tax (BD)",
			Amount: nr,
		}}, nil
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(bdAnnualPeriodFraction)
	annualGross := e.IncomeTaxBase(gross).Div(periodFraction)
	annualTax := walkBDBrackets(annualGross, bdBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if !periodTax.IsPositive() {
		return nil, nil
	}
	return []Deduction{{
		Code:   "BD_INCOME_TAX",
		Name:   "Income tax deducted at source (BD)",
		Amount: periodTax,
	}}, nil
}

func walkBDBrackets(annual decimal.Decimal, scale []bdBracket) decimal.Decimal {
	var match bdBracket
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
