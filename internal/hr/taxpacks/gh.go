package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// ghPack implements Ghana's payroll-side statutory withholdings:
//
//   - Pay-As-You-Earn (PAYE) income tax on employment income per
//     the Income Tax Act 2015 (Act 896), Sixth Schedule. The
//     schedule is a genuinely MONTHLY graduated table (GHS), so the
//     pack applies it directly to the slip's monthly chargeable
//     income without annualisation. The employee's SSNIT first-tier
//     contribution is deductible before PAYE, so the pack nets it
//     off the chargeable-income base.
//
//   - SSNIT (Social Security and National Insurance Trust) first-
//     tier employee contribution per the National Pensions Act 2008
//     (Act 766): 5.5% of basic salary. The employer's 13% share
//     (split 13.5% total less the 5.5% employee) is not a slip
//     deduction.
//
// A non-resident individual is taxed at the flat 25% rate on
// Ghana-source employment income (Act 896 s. 116); the pack emits
// that flat line and no SSNIT for non-residents.
//
// References:
//
//	Income Tax Act 2015 (Act 896) + annual amendment acts:
//	  https://gra.gov.gh/
//	GRA PAYE graduated tax table (monthly):
//	  https://gra.gov.gh/domestic-tax/tax-types/paye/
//	National Pensions Act 2008 (Act 766) — SSNIT 5.5% employee:
//	  https://www.ssnit.org.gh/
type ghPack struct{}

func init() { Register(&ghPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (ghPack) Country() string { return "GH" }

// EffectiveYear returns the calendar year the GH table is
// calibrated for: 2024 (GRA monthly PAYE graduated table; SSNIT
// 5.5% first-tier employee rate).
func (ghPack) EffectiveYear() int { return 2024 }

type ghBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// GRA monthly PAYE graduated table (monthly chargeable income,
	// GHS). Base is cumulative tax at each Floor.
	ghBracketsMonthly = []ghBracket{
		{Floor: dec("0"), Top: dec("490"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("490"), Top: dec("600"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("600"), Top: dec("730"), Base: dec("5.5"), Rate: dec("0.10")},
		{Floor: dec("730"), Top: dec("3896.67"), Base: dec("18.5"), Rate: dec("0.175")},
		{Floor: dec("3896.67"), Top: dec("19896.67"), Base: dec("572.67"), Rate: dec("0.25")},
		{Floor: dec("19896.67"), Top: dec("50416.67"), Base: dec("4572.67"), Rate: dec("0.30")},
		{Floor: dec("50416.67"), Top: decimal.Zero, Base: dec("13728.67"), Rate: dec("0.35")},
	}

	// SSNIT first-tier employee contribution: 5.5% of basic salary.
	ghSSNITEmployeeRate = dec("0.055")

	// Non-resident flat rate on Ghana-source employment income.
	ghNonResidentRate = dec("0.25")
)

// ComputeWithholding emits GH_SSNIT (5.5% first-tier employee
// contribution) and GH_PAYE (monthly graduated income tax on the
// chargeable income net of SSNIT) for residents. Non-residents get
// a single GH_NONRESIDENT_TAX flat-rate line.
func (ghPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) || period.Days() <= 0 {
		return nil, nil
	}

	if !e.Resident {
		nr := e.IncomeTaxBase(gross).Mul(ghNonResidentRate).Round(2)
		if !nr.IsPositive() {
			return nil, nil
		}
		return []Deduction{{
			Code:   "GH_NONRESIDENT_TAX",
			Name:   "Non-resident withholding tax (GH)",
			Amount: nr,
		}}, nil
	}

	out := []Deduction{}

	// SSNIT first-tier employee contribution, deductible before
	// PAYE.
	ssnit := e.ContributionBase(gross).Mul(ghSSNITEmployeeRate).Round(2)
	if ssnit.IsPositive() {
		out = append(out, Deduction{
			Code:   "GH_SSNIT",
			Name:   "SSNIT first-tier contribution (employee, GH)",
			Amount: ssnit,
		})
	}

	chargeable := e.IncomeTaxBase(gross).Sub(ssnit)
	if chargeable.IsNegative() {
		chargeable = decimal.Zero
	}
	paye := walkGHBrackets(chargeable, ghBracketsMonthly).Round(2)
	if paye.IsPositive() {
		out = append(out, Deduction{
			Code:   "GH_PAYE",
			Name:   "PAYE income tax (GH)",
			Amount: paye,
		})
	}

	return out, nil
}

func walkGHBrackets(monthly decimal.Decimal, scale []ghBracket) decimal.Decimal {
	var match ghBracket
	matched := false
	for _, b := range scale {
		if monthly.LessThanOrEqual(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.Base.Add(monthly.Sub(match.Floor).Mul(match.Rate))
}
