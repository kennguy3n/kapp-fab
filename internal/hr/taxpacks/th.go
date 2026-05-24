package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// thPack implements Thailand's PND1 monthly payroll-side
// statutory withholdings:
//
//   - PIT (Personal Income Tax) progressive schedule per Revenue
//     Code s.48 + Royal Decree (No. 750) BE 2565 / 2566, the
//     amount carried forward into the 2024 / 2025 tax years. The
//     pack annualises gross, applies the standard deduction of
//     50% of income (capped at THB 100,000), the personal
//     allowance of THB 60,000, and an additional THB 30,000 per
//     dependent (child / parent), then walks the 8-bracket
//     schedule. PND1 (Form Por.Ngor.Dor.1) is the monthly
//     withholding return; the slip amount is annual tax / 12 for
//     a standard monthly period, otherwise pro-rated by
//     period.Days() / 365.25.
//
//   - SSF (Social Security Fund) employee contribution at 5% of
//     monthly wage capped at THB 15,000 / month (max THB 750 /
//     month). Per the Social Security Act BE 2533 s.46 + 2024
//     Royal Decree extension keeping the 5% rate after the
//     COVID-era temporary reduction.
//
// Reference:
//
//	Revenue Department PIT schedule:
//	  https://www.rd.go.th/english/6045.html
//	Social Security Office contribution rates:
//	  https://www.sso.go.th
type thPack struct{}

func init() { Register(&thPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (thPack) Country() string { return "TH" }

// EffectiveYear returns the fiscal year the TH tables are
// calibrated for: 2024 brackets (carried forward from BE 2566).
// The social security rate is the standard 5% — operators should
// re-check the SSO annual notice in Jan of each new year as the
// SSF ceiling tracks the minimum wage indirectly.
func (thPack) EffectiveYear() int { return 2024 }

// thBracket mirrors the Revenue Department PIT schedule. Floor /
// Top are taxable annual income in THB; Base is cumulative tax at
// Floor; Rate applies to the marginal portion above Floor.
type thBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	thBracketsResident = []thBracket{
		{Floor: dec("0"), Top: dec("150000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("150000"), Top: dec("300000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("300000"), Top: dec("500000"), Base: dec("7500"), Rate: dec("0.10")},
		{Floor: dec("500000"), Top: dec("750000"), Base: dec("27500"), Rate: dec("0.15")},
		{Floor: dec("750000"), Top: dec("1000000"), Base: dec("65000"), Rate: dec("0.20")},
		{Floor: dec("1000000"), Top: dec("2000000"), Base: dec("115000"), Rate: dec("0.25")},
		{Floor: dec("2000000"), Top: dec("5000000"), Base: dec("365000"), Rate: dec("0.30")},
		{Floor: dec("5000000"), Top: decimal.Zero, Base: dec("1265000"), Rate: dec("0.35")},
	}

	// Revenue Department allowances applied before the bracket
	// walk. The standard deduction is 50% of income up to a
	// THB 100,000 cap; the personal allowance is the THB 60,000
	// flat for the employee themselves; dependent allowances run
	// THB 30,000 each (children + supported parents).
	thStandardDeductionRate = dec("0.5")
	thStandardDeductionCap  = dec("100000")
	thPersonalAllowance     = dec("60000")
	thDependentAllowance    = dec("30000")

	// Sanity cap on declared dependents. Not a Revenue Code
	// figure — Thai law applies the per-dependent allowance to
	// every qualifying dependent with no fixed numeric ceiling
	// — but a defense-in-depth guard against a wizard /
	// payroll-import bug sending an absurd NumDependents value
	// that would otherwise drive taxable income to zero.
	thMaxDependents = 20

	// SSF parameters (Social Security Act).
	thSSFEmployeeRate = dec("0.05")
	thSSFCeiling      = dec("15000") // per-month insurable wage cap.

	thPeriodsPerYear = decimal.NewFromFloat(365.25)

	// Revenue Code s.50(1) + s.41 + Ministerial Reg. 126 on
	// non-resident employment income: 15% flat withholding for
	// non-residents whose Thai-sourced employment income is
	// taxed at source. Applies when the employee is in Thailand
	// < 180 days in the tax year and has no permanent
	// establishment.
	thNonResidentRate = dec("0.15")
)

// ComputeWithholding emits TH_PIT_WITHHOLDING (PND1 progressive
// monthly tax after standard deduction and personal/dependent
// allowances) and TH_SSF (5% capped at the THB 15,000 / month
// insurable wage) for residents. Non-residents (Revenue Code
// s.50(1) + s.41) get TH_NONRESIDENT_TAX at the flat 15% rate
// and no SSF (SSF eligibility under SSA s.33 is restricted to
// employees with permanent Thai employment). Zero-amount lines
// are omitted.
func (thPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Non-resident: flat 15% on gross, no SSF.
	if !e.Resident {
		nr := gross.Mul(thNonResidentRate).Round(2)
		if nr.IsPositive() {
			out = append(out, Deduction{
				Code:   "TH_NONRESIDENT_TAX",
				Name:   "Non-resident PIT flat 15% (TH)",
				Amount: nr,
			})
		}
		return out, nil
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(thPeriodsPerYear)
	annualGross := gross.Div(periodFraction)

	// Standard deduction: 50% of income capped at THB 100,000.
	stdDed := annualGross.Mul(thStandardDeductionRate)
	if stdDed.GreaterThan(thStandardDeductionCap) {
		stdDed = thStandardDeductionCap
	}

	// Personal + dependent allowances. Negative or unknown
	// dependent counts are clamped to zero so a missing
	// EmployeeInfo.NumDependents doesn't wrongly subtract.
	// Upper-bounded at thMaxDependents (20) as defense in depth
	// against a data-entry error driving taxable income to zero.
	// The Thai Revenue Code does not impose a hard statutory cap
	// the way Indonesia does (UU PPh art. 7(3) → max 3
	// dependents) — Thai allowances cover an employee's children,
	// supported parents, and (under TRD interpretation) some
	// supported in-laws, with no published numeric ceiling — but
	// 20 comfortably covers every reasonable household
	// composition and bounds the blast radius of a wizard /
	// payroll-import bug that sends NumDependents=10_000.
	deps := e.NumDependents
	if deps < 0 {
		deps = 0
	}
	if deps > thMaxDependents {
		deps = thMaxDependents
	}
	allowances := thPersonalAllowance.Add(
		thDependentAllowance.Mul(decimal.NewFromInt(int64(deps))),
	)

	taxable := annualGross.Sub(stdDed).Sub(allowances)
	if taxable.LessThan(decimal.Zero) {
		taxable = decimal.Zero
	}
	annualTax := walkTHBrackets(taxable, thBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "TH_PIT_WITHHOLDING",
			Name:   "PND1 personal income tax withholding (TH)",
			Amount: periodTax,
		})
	}

	// SSF — flat 5% of monthly wage capped at THB 750. The cap
	// is implicit in capping the wage base at THB 15,000.
	ssfBase := gross
	if ssfBase.GreaterThan(thSSFCeiling) {
		ssfBase = thSSFCeiling
	}
	ssf := ssfBase.Mul(thSSFEmployeeRate).Round(2)
	if ssf.IsPositive() {
		out = append(out, Deduction{
			Code:   "TH_SSF",
			Name:   "Social Security Fund (employee share, TH)",
			Amount: ssf,
		})
	}

	return out, nil
}

func walkTHBrackets(annual decimal.Decimal, scale []thBracket) decimal.Decimal {
	var match thBracket
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
