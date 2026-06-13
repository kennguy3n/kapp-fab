package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// lkPack implements Sri Lanka's payroll-side statutory withholdings:
//
//   - Advance Personal Income Tax (APIT) on employment income per
//     the Inland Revenue Act No. 24 of 2017 (as amended from
//     1 Jan 2023). The pack annualises the slip's gross
//     (days / 365.25), applies the personal relief of
//     LKR 1,200,000/year (the tax-free first band), walks the
//     progressive schedule (LKR) and prorates the annual tax back
//     to the slip period.
//
//   - Employees' Provident Fund (EPF) employee contribution per the
//     EPF Act No. 15 of 1958: 8% of total monthly earnings (no
//     ceiling). The employer's 12% EPF share and the 3% ETF
//     (Employees' Trust Fund) contribution are employer costs, not
//     slip deductions.
//
// References:
//
//	Inland Revenue Act No. 24 of 2017 + APIT tables (2023+):
//	  https://www.ird.gov.lk/
//	IRD APIT tax tables:
//	  https://www.ird.gov.lk/en/publications/sitepages/APIT_Tax_Tables.aspx
//	EPF Act No. 15 of 1958 (8% employee / 12% employer):
//	  https://epf.lk/
type lkPack struct{}

func init() { Register(&lkPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (lkPack) Country() string { return "LK" }

// EffectiveYear returns the calendar year the LK tables are
// calibrated for: 2024 (APIT schedule effective from the
// 2023/2024 year of assessment; EPF 8% employee).
func (lkPack) EffectiveYear() int { return 2024 }

type lkBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// APIT progressive schedule (annual taxable income, LKR). Base
	// is cumulative tax at each Floor. The first band is the
	// LKR 1,200,000 personal relief.
	lkBracketsResident = []lkBracket{
		{Floor: dec("0"), Top: dec("1200000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("1200000"), Top: dec("1700000"), Base: dec("0"), Rate: dec("0.06")},
		{Floor: dec("1700000"), Top: dec("2200000"), Base: dec("30000"), Rate: dec("0.12")},
		{Floor: dec("2200000"), Top: dec("2700000"), Base: dec("90000"), Rate: dec("0.18")},
		{Floor: dec("2700000"), Top: dec("3200000"), Base: dec("180000"), Rate: dec("0.24")},
		{Floor: dec("3200000"), Top: dec("3700000"), Base: dec("300000"), Rate: dec("0.30")},
		{Floor: dec("3700000"), Top: decimal.Zero, Base: dec("450000"), Rate: dec("0.36")},
	}

	// EPF employee contribution: 8% of total monthly earnings.
	lkEPFEmployeeRate = dec("0.08")

	lkAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits LK_APIT (annualised progressive advance
// personal income tax) and LK_EPF_EMPLOYEE (8% provident-fund
// contribution).
func (lkPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(lkAnnualPeriodFraction)
	annualGross := e.IncomeTaxBase(gross).Div(periodFraction)
	annualTax := walkLKBrackets(annualGross, lkBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "LK_APIT",
			Name:   "Advance Personal Income Tax (LK)",
			Amount: periodTax,
		})
	}

	// EPF runs on the contribution base (full gross by default).
	epf := e.ContributionBase(gross).Mul(lkEPFEmployeeRate).Round(2)
	if epf.IsPositive() {
		out = append(out, Deduction{
			Code:   "LK_EPF_EMPLOYEE",
			Name:   "EPF contribution (employee, LK)",
			Amount: epf,
		})
	}

	return out, nil
}

func walkLKBrackets(annual decimal.Decimal, scale []lkBracket) decimal.Decimal {
	var match lkBracket
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
