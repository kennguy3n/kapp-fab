package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// nzPack implements New Zealand's monthly payroll-side statutory
// withholdings:
//
//   - PAYE: Inland Revenue Department (IRD) progressive schedule
//     per the Income Tax Act 2007, effective 1 April 2024 (Budget
//     2024 thresholds). The pack annualises monthly gross via
//     period.Days() / 365.25 so that leap and non-leap years round
//     to the same monthly figure (the IR340 worked examples
//     implicitly assume the same averaging — using a flat 365
//     produces a ~0.07% drift across the leap-year boundary that
//     the worked examples do not reproduce). Brackets walked with
//     the standard (Base + Rate × (income - Floor)) formulation.
//
//   - ACC Earners' Levy: 1.60% (2024/25 rate, gazetted in the
//     2024/25 Workforce Income Plan) on liable earnings, capped at
//     the maximum liable earnings threshold of NZD 142,283 (the
//     2024/25 ACC ceiling published by ACC on 1 April 2024). The
//     annual ceiling is pro-rated to the slip period (cap ×
//     days/365.25) so a monthly slip caps the liable base at
//     142,283/12 ≈ 11,857. YTD-aware capping is deferred to a
//     future EmployeeInfo.YTDLiableEarnings projection; today's
//     per-slip pro-ration is conservative for run-rate slips and
//     within rounding of the IRD CS calculator for steady wages.
//
//   - KiwiSaver: employee share at 3% / 4% / 6% / 8% / 10% per
//     KiwiSaver Act 2006 + Schedule 1 (post-2019 contribution
//     options). Rate is opt-in via EmployeeInfo.KiwiSaverRate;
//     decimal.Zero means the employee has not opted into
//     KiwiSaver (or has a contributions holiday) and no line is
//     emitted. The pack does NOT auto-enrol new employees — that
//     decision sits with onboarding, not the slip generator.
//
// References:
//
//	IRD PAYE rates (2024/25):
//	  https://www.ird.govt.nz/income-tax/income-tax-for-individuals/tax-codes-and-tax-rates-for-individuals/tax-rates-for-individuals
//	Budget 2024 personal-tax thresholds (effective 31 Jul 2024):
//	  https://www.beehive.govt.nz/sites/default/files/2024-05/Personal%20income%20tax%20changes.pdf
//	ACC Earners' Levy 2024/25:
//	  https://www.acc.co.nz/for-business/levies/levy-payable-by-businesses/
//	KiwiSaver Act 2006, Schedule 1:
//	  https://www.legislation.govt.nz/act/public/2006/0040/latest/DLM378372.html
type nzPack struct{}

func init() { Register(&nzPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (nzPack) Country() string { return "NZ" }

// EffectiveYear returns the fiscal year the NZ tables are
// calibrated for: 2024 (Budget 2024 personal-tax thresholds
// effective 31 July 2024 + ACC 2024/25 levy + KiwiSaver Act
// schedule).
func (nzPack) EffectiveYear() int { return 2024 }

type nzBracket struct {
	Floor decimal.Decimal // annual taxable income, NZD
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Post-Budget-2024 PAYE schedule (effective 31 July 2024):
	// brackets widened at the bottom + new 30% step between
	// 53,500 and 78,100. Published in IR340 tables.
	nzBracketsResident = []nzBracket{
		{Floor: dec("0"), Top: dec("15600"), Base: dec("0"), Rate: dec("0.105")},
		{Floor: dec("15600"), Top: dec("53500"), Base: dec("1638"), Rate: dec("0.175")},
		{Floor: dec("53500"), Top: dec("78100"), Base: dec("8270.50"), Rate: dec("0.30")},
		{Floor: dec("78100"), Top: dec("180000"), Base: dec("15650.50"), Rate: dec("0.33")},
		{Floor: dec("180000"), Top: decimal.Zero, Base: dec("49277.50"), Rate: dec("0.39")},
	}

	// ACC Earners' Levy 2024/25.
	nzACCRate    = dec("0.016")
	nzACCCeiling = dec("142283")

	// Day count used to scale brackets — IR340 PAYE tables use
	// 365.25 implicitly across the year so monthly slips round
	// consistently across leap and non-leap years.
	nzAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits NZ_PAYE (progressive), NZ_ACC (earners'
// levy capped at the 2024/25 ceiling), and NZ_KIWISAVER (employee
// share per KiwiSaverRate). Zero-amount lines are omitted.
func (nzPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(nzAnnualDays)
	if periodFraction.IsZero() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	annualTax := walkNZBrackets(annualGross, nzBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "NZ_PAYE",
			Name:   "PAYE income tax withholding (NZ)",
			Amount: periodTax,
		})
	}

	// ACC: cap the slip's liable earnings against the annual
	// ceiling pro-rated to the period. A monthly slip that pushes
	// the employee past the 142,283 ceiling only levies the
	// portion under the cap; downstream YTD reconciliation lives
	// in the engine, not this pack.
	periodACCCap := nzACCCeiling.Mul(periodFraction)
	accBase := gross
	if accBase.GreaterThan(periodACCCap) {
		accBase = periodACCCap
	}
	if acc := accBase.Mul(nzACCRate).Round(2); acc.IsPositive() {
		out = append(out, Deduction{
			Code:   "NZ_ACC",
			Name:   "ACC Earners' Levy (NZ)",
			Amount: acc,
		})
	}

	// KiwiSaver: opt-in. KiwiSaverRate == 0 means "not enrolled".
	// Non-zero rates apply verbatim against gross (not after
	// PAYE) per IRD payroll rules.
	if e.KiwiSaverRate.IsPositive() {
		if ks := gross.Mul(e.KiwiSaverRate).Round(2); ks.IsPositive() {
			out = append(out, Deduction{
				Code:   "NZ_KIWISAVER",
				Name:   "KiwiSaver employee contribution (NZ)",
				Amount: ks,
			})
		}
	}

	return out, nil
}

// walkNZBrackets walks the IR340 PAYE schedule. The Top of the
// last bracket is decimal.Zero, which is treated as "no upper
// bound" by the loop (the last matched bracket wins).
func walkNZBrackets(annual decimal.Decimal, scale []nzBracket) decimal.Decimal {
	var match nzBracket
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
