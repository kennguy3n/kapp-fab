package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// nlPack implements the Netherlands' payroll-side statutory
// withholdings for the 2025 fiscal year:
//
//   - Loonheffing (wage withholding) — combines income tax
//     (loonbelasting) and the national insurance premium
//     volksverzekeringen (premie AOW/Anw/Wlz). The Belastingdienst
//     publishes the "witte tabel" (regular employment) for
//     monthly payroll. For 2025 the combined Box 1 schedule
//     under age AOW (most working-age employees) has three
//     bands:
//     0      → 38,441     35.82% (income tax 8.17% + premie 27.65%)
//     38,441 → 76,817     37.48% (income tax 37.48%; premie ceiling reached)
//     76,817 → open       49.50% (top rate)
//     This pack uses these combined rates from the Belasting-
//     dienst's 2025 "loonbelastingtabellen" published in
//     Staatscourant 2024-37411.
//
//   - Heffingskorting (general tax credit) and arbeidskorting
//     (labour discount): the loonheffingskorting on the
//     standard tabel applies these credits automatically. The
//     algemene heffingskorting starts at €3,068 for incomes
//     ≤ €24,813, then tapers; the arbeidskorting peaks at
//     €5,599 around €40,000. For implementation simplicity (and
//     matching how Belastingdienst publishes monthly tables),
//     this pack applies an approximate net effect: subtract a
//     base credit of €3,000 / yr from gross tax for incomes
//     ≤ €76,817, tapering to zero above €124,934. Tenants
//     wanting strict per-bracket credit treatment should
//     override per-employee.
//
//   - ZVW (Zorgverzekeringswet) — health insurance contribution.
//     Employees pay the income-dependent portion via their
//     employer at 5.32% for 2025, capped at the maximum
//     contribution income of €75,864 / year. Note: this is the
//     "lage" rate (employee-borne via inkomensafhankelijke bijdrage
//     ZVW). The "hoge" rate (employer-borne, 6.51%) is not an
//     employee deduction.
//
// References:
//
//	Belastingdienst loonbelastingtabellen 2025:
//	  https://www.belastingdienst.nl/wps/wcm/connect/bldcontentnl/themaoverstijgend/programmas_en_formulieren/loonbelastingtabellen-2025
//	Box 1 tarief 2025:
//	  https://www.belastingdienst.nl/wps/wcm/connect/nl/inkomstenbelasting/content/hoe-bereken-ik-mijn-belasting-box-1
//	ZVW premie 2025 (CAK):
//	  https://www.rijksoverheid.nl/onderwerpen/zorgverzekering/inkomensafhankelijke-bijdrage-zorgverzekeringswet
type nlPack struct{}

func init() { Register(&nlPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (nlPack) Country() string { return "NL" }

// EffectiveYear returns the fiscal year the NL tables are
// calibrated for: 2025 (Belastingdienst loonbelastingtabellen
// 2025).
func (nlPack) EffectiveYear() int { return 2025 }

type nlBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Box 1 (loonheffing onder AOW-leeftijd) 2025 — combined IB
	// + premie volksverzekeringen.
	nlLoonheffingBrackets = []nlBracket{
		{Floor: dec("0"), Top: dec("38441"), Base: dec("0"), Rate: dec("0.3582")},
		{Floor: dec("38441"), Top: dec("76817"), Base: dec("13770.36"), Rate: dec("0.3748")},
		{Floor: dec("76817"), Top: decimal.Zero, Base: dec("28157.85"), Rate: dec("0.4950")},
	}

	// Credit approximation (heffingskorting + arbeidskorting net
	// effect on annual tax).
	nlCreditBase        = dec("3000")
	nlCreditFullIncome  = dec("76817")
	nlCreditPhaseOutTop = dec("124934")

	// ZVW: 5.32% employee inkomensafhankelijke bijdrage, capped
	// at €75,864 / yr (2025 maximum bijdrage-inkomen).
	nlZVWRate      = dec("0.0532")
	nlZVWMaxIncome = dec("75864")

	nlAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to two lines:
//
//   - NL_LOONHEFFING  (Loonheffing per Belastingdienst witte tabel
//     2025, two progressive bands, with the standard
//     heffingskorting applied)
//   - NL_ZVW          (ZVW-bijdrage at 5.32% employee share, capped
//     at the annual maximumbijdrageloon)
//
// Negative or zero gross / period return nil.
func (nlPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(nlAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// Loonheffing — bracket walk on annual gross, minus
	// heffingskortingen, prorated back to the slip.
	annualGrossTax := walkNLBrackets(annualGross, nlLoonheffingBrackets)
	credit := nlComputeCredit(annualGross)
	annualNetTax := annualGrossTax.Sub(credit)
	if annualNetTax.LessThan(decimal.Zero) {
		annualNetTax = decimal.Zero
	}
	periodTax := annualNetTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "NL_LOONHEFFING",
			Name:   "Loonheffing (NL)",
			Amount: periodTax,
		})
	}

	// ZVW — 5.32% on gross up to the annual ceiling, YTD-aware.
	if zvw := nlComputeZVW(gross, e.YTDGross); zvw.IsPositive() {
		out = append(out, Deduction{
			Code:   "NL_ZVW",
			Name:   "ZVW (Zorgverzekering, employee, NL)",
			Amount: zvw,
		})
	}

	return out, nil
}

// nlComputeCredit returns the combined heffingskorting +
// arbeidskorting credit for the annual income. Within the full-
// credit range (≤ €76,817) the credit is constant at €3,000;
// above that it phases out linearly to zero at €124,934.
func nlComputeCredit(annual decimal.Decimal) decimal.Decimal {
	if annual.LessThanOrEqual(nlCreditFullIncome) {
		return nlCreditBase
	}
	if annual.GreaterThanOrEqual(nlCreditPhaseOutTop) {
		return decimal.Zero
	}
	// Linear taper from (76817, 3000) to (124934, 0).
	span := nlCreditPhaseOutTop.Sub(nlCreditFullIncome)
	pos := annual.Sub(nlCreditFullIncome)
	remaining := decimal.NewFromInt(1).Sub(pos.Div(span))
	return nlCreditBase.Mul(remaining)
}

// nlComputeZVW returns the ZVW employee contribution, YTD-aware.
func nlComputeZVW(gross, ytd decimal.Decimal) decimal.Decimal {
	if ytd.GreaterThanOrEqual(nlZVWMaxIncome) {
		return decimal.Zero
	}
	base := gross
	if ytd.Add(gross).GreaterThan(nlZVWMaxIncome) {
		base = nlZVWMaxIncome.Sub(ytd)
	}
	if base.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return base.Mul(nlZVWRate).Round(2)
}

// walkNLBrackets walks the Loonheffing schedule.
func walkNLBrackets(annual decimal.Decimal, brackets []nlBracket) decimal.Decimal {
	var match nlBracket
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
