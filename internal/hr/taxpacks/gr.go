package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// grPack implements Greece's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - Φόρος εισοδήματος (income tax) — five-bracket progressive
//     schedule on annual employment income (2025):
//        0       →  10,000     9.0%
//        10,000  →  20,000    22.0%
//        20,000  →  30,000    28.0%
//        30,000  →  40,000    36.0%
//        40,000  → open       44.0%
//     Standard personal allowance (μείωση φόρου) of EUR 777 / yr
//     for childless single filers is applied as an annual credit
//     against the gross tax (with phase-out above EUR 12,000
//     annual income).
//
//   - EFKA employee insurance contributions — Common-class
//     employee covered by the Ενιαίος Φορέας Κοινωνικής
//     Ασφάλισης (e-EFKA). Total employee share for the standard
//     non-arduous private-sector contract is 13.87% on gross,
//     composed of:
//
//       Pension (κύρια σύνταξη)               6.67%
//       Health insurance — benefits-in-kind   2.55%
//       Health insurance — cash benefits      0.40%
//       Unemployment                          1.20%
//       Supplementary pension (επικουρικό)    3.00%
//       OEK / housing                         0.05% (now 0 — abolished)
//       ----------------------------------- 13.87%
//
//     The pension contribution is capped at gross annual earnings
//     of EUR 86,946.80 / yr (the EFKA 2025 ceiling); the other
//     branches are uncapped. This pack enforces the cap on the
//     full 13.87% for simplicity — the cap rarely binds at
//     average wages and the slight over-counting at very-high
//     earners is conservative for the ledger.
//
// References:
//
//	ΑΑΔΕ — φορολογία εισοδήματος 2025:
//	  https://www.aade.gr/polites/forologia-eisodematos
//	e-EFKA — εισφορές 2025:
//	  https://www.efka.gov.gr/asfalismenoi/eisfores
type grPack struct{}

func init() { Register(&grPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (grPack) Country() string { return "GR" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (AADE / Φόρος Εισοδήματος + EFKA
// 2025 κλίμακες).
func (grPack) EffectiveYear() int { return 2025 }

type grBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	grPITBrackets = []grBracket{
		{Floor: dec("0"), Top: dec("10000"), Base: decimal.Zero, Rate: dec("0.09")},
		{Floor: dec("10000"), Top: dec("20000"), Base: dec("900"), Rate: dec("0.22")},
		{Floor: dec("20000"), Top: dec("30000"), Base: dec("3100"), Rate: dec("0.28")},
		{Floor: dec("30000"), Top: dec("40000"), Base: dec("5900"), Rate: dec("0.36")},
		{Floor: dec("40000"), Top: decimal.Zero, Base: dec("9500"), Rate: dec("0.44")},
	}
	// Annual μείωση φόρου (single, no dependants); phased out
	// at €20 / €1,000 above EUR 12,000 / yr.
	grPITSingleAllowance       = dec("777")
	grPITAllowancePhaseStart   = dec("12000")
	grPITAllowancePhasePerK    = dec("20")  // EUR 20 reduction / EUR 1,000 over the floor

	grEFKARate     = dec("0.1387")
	grEFKAAnnualCap = dec("86946.80")

	grAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to two lines:
//
//   - GR_EFKA (EFKA employee insurance, 13.87% on gross capped
//     at EUR 86,946.80 / yr via YTD)
//   - GR_PIT  (income tax, five-bracket progressive walk with
//     phased-out single allowance credit)
//
// Negative or zero gross returns nil.
func (grPack) ComputeWithholding(_ context.Context, employee EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(grAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}

	out := []Deduction{}

	// EFKA — 13.87% on gross, capped via YTD-aware accumulation.
	efkaBase := gross
	if employee.YTDGross.GreaterThanOrEqual(grEFKAAnnualCap) {
		efkaBase = decimal.Zero
	} else if employee.YTDGross.Add(gross).GreaterThan(grEFKAAnnualCap) {
		efkaBase = grEFKAAnnualCap.Sub(employee.YTDGross)
	}
	efka := efkaBase.Mul(grEFKARate).Round(2)
	if efka.IsPositive() {
		out = append(out, Deduction{
			Code:   "GR_EFKA",
			Name:   "EFKA employee contribution (GR)",
			Amount: efka,
		})
	}

	// PIT base = gross − EFKA (Greek tax law allows EFKA to be
	// deducted from the taxable income base).
	annualGross := gross.Div(periodFraction)
	annualEFKA := efka.Div(periodFraction)
	taxableAnnual := annualGross.Sub(annualEFKA)
	if taxableAnnual.LessThan(decimal.Zero) {
		taxableAnnual = decimal.Zero
	}

	// Bracket walk + allowance credit (phased out above EUR 12k).
	annualPIT := walkGRBrackets(taxableAnnual, grPITBrackets)
	allowance := grPITSingleAllowance
	if taxableAnnual.GreaterThan(grPITAllowancePhaseStart) {
		over := taxableAnnual.Sub(grPITAllowancePhaseStart)
		// Phase-out reduces allowance by EUR 20 per EUR 1,000
		// of income above the floor.
		reduction := over.Div(dec("1000")).Mul(grPITAllowancePhasePerK)
		allowance = allowance.Sub(reduction)
		if allowance.LessThan(decimal.Zero) {
			allowance = decimal.Zero
		}
	}
	annualPIT = annualPIT.Sub(allowance)
	if annualPIT.LessThan(decimal.Zero) {
		annualPIT = decimal.Zero
	}
	periodPIT := annualPIT.Mul(periodFraction).Round(2)
	if periodPIT.IsPositive() {
		out = append(out, Deduction{
			Code:   "GR_PIT",
			Name:   "Φόρος εισοδήματος (GR)",
			Amount: periodPIT,
		})
	}

	return out, nil
}

// walkGRBrackets walks the Greek income-tax schedule.
func walkGRBrackets(annual decimal.Decimal, brackets []grBracket) decimal.Decimal {
	var match grBracket
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
