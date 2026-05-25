package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// kePack implements Kenya's monthly payroll-side statutory
// withholdings:
//
//   - PAYE per the Kenya Revenue Authority (KRA) Income Tax Act
//     Cap 470, Third Schedule (as amended by the Finance Act 2023
//     to introduce the 32.5% and 35% bands effective 1 July 2023).
//     Five-band monthly schedule: 10% / 25% / 30% / 32.5% / 35%.
//     The pack applies the published monthly personal relief
//     (KES 2,400) after walking the bands — relief reduces tax
//     payable but never below zero, per s.30(1).
//
//   - NSSF Act 2013 (Tier I + II): 6% employee share on pensionable
//     earnings up to the Upper Earnings Limit (KES 72,000 from
//     February 2025, with Lower Earnings Limit KES 8,000). Tier
//     totals: maximum employee contribution KES 4,320 / month
//     (6% × 72,000). The pack applies the post-Feb-2025
//     Tier I + II merged schedule because the Court of Appeal
//     upheld the staged increase in Feb 2025.
//
//   - SHIF (Social Health Insurance Fund), the NHIF successor
//     effective 1 October 2024 under the Social Health Insurance
//     Act 2023: 2.75% of gross monthly salary, no ceiling, minimum
//     KES 300 / month. This replaces the former NHIF graduated
//     bands.
//
//   - Affordable Housing Levy (Affordable Housing Act 2024 s.5):
//     1.5% employee + 1.5% employer of gross monthly salary, no
//     ceiling. Effective from 19 March 2024.
//
// References:
//
//	KRA PAYE bands (Third Schedule + Finance Act 2023):
//	  https://www.kra.go.ke/individual/calculate-tax/calculating-tax/paye
//	NSSF Act 2013 + Feb 2025 implementation:
//	  https://www.nssf.or.ke/
//	Social Health Insurance Act 2023 (SHIF):
//	  https://shif.go.ke/
//	Affordable Housing Act 2024 (Housing Levy):
//	  https://housing.go.ke/
type kePack struct{}

func init() { Register(&kePack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services. Used by the registry to key lookups from
// tenants.country.
func (kePack) Country() string { return "KE" }

// EffectiveYear returns the fiscal year the KE tables are
// calibrated for: 2025 (post-Feb-2025 NSSF + post-Oct-2024 SHIF
// + post-Mar-2024 Housing Levy + Finance Act 2023 PAYE bands).
func (kePack) EffectiveYear() int { return 2025 }

type keBracket struct {
	Floor decimal.Decimal // monthly KES
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// KRA Third Schedule + Finance Act 2023 monthly bands.
	keBrackets = []keBracket{
		{Floor: dec("0"), Top: dec("24000"), Base: dec("0"), Rate: dec("0.10")},
		{Floor: dec("24000"), Top: dec("32333"), Base: dec("2400"), Rate: dec("0.25")},
		{Floor: dec("32333"), Top: dec("500000"), Base: dec("4483.25"), Rate: dec("0.30")},
		{Floor: dec("500000"), Top: dec("800000"), Base: dec("144783.35"), Rate: dec("0.325")},
		{Floor: dec("800000"), Top: decimal.Zero, Base: dec("242283.35"), Rate: dec("0.35")},
	}

	// ITA s.30(1) monthly personal relief.
	kePersonalReliefMonthly = dec("2400")

	// NSSF Act 2013 post-Feb-2025 Tier I + II merged: 6% on
	// pensionable earnings up to UEL = KES 72,000. Max
	// employee contribution = 4,320.
	keNSSFRate        = dec("0.06")
	keNSSFUpperEarn   = dec("72000")
	keNSSFMaxEmployee = dec("4320")

	// SHIF rate (Social Health Insurance Fund, post-Oct-2024).
	keSHIFRate = dec("0.0275")
	keSHIFMin  = dec("300")

	// Affordable Housing Levy: 1.5% gross, no ceiling.
	keHousingLevyRate = dec("0.015")

	keMonthlyDaysApprox = decimal.NewFromFloat(30.4375)
)

// ComputeWithholding emits KE_PAYE, KE_NSSF, KE_SHIF, KE_HOUSING_LEVY.
// All four bases use the slip's monthly-equivalent gross (slip ÷
// period days × 30.4375); zero-amount lines are omitted.
func (kePack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Monthly-equivalent gross.
	monthlyEquiv := gross.Mul(keMonthlyDaysApprox).Div(decimal.NewFromInt(int64(days)))

	// PAYE: walk the monthly bands, subtract personal relief.
	monthlyTax := walkKEBrackets(monthlyEquiv).Sub(kePersonalReliefMonthly)
	if monthlyTax.LessThan(decimal.Zero) {
		monthlyTax = decimal.Zero
	}
	// Scale back to slip-period proportion.
	periodPAYE := monthlyTax.Mul(decimal.NewFromInt(int64(days))).Div(keMonthlyDaysApprox).Round(2)
	if periodPAYE.IsPositive() {
		out = append(out, Deduction{
			Code:   "KE_PAYE",
			Name:   "PAYE (KRA, KE)",
			Amount: periodPAYE,
		})
	}

	// NSSF: 6% on min(monthlyEquiv, 72,000), max 4,320.
	nssfBase := monthlyEquiv
	if nssfBase.GreaterThan(keNSSFUpperEarn) {
		nssfBase = keNSSFUpperEarn
	}
	nssfMonthly := nssfBase.Mul(keNSSFRate)
	if nssfMonthly.GreaterThan(keNSSFMaxEmployee) {
		nssfMonthly = keNSSFMaxEmployee
	}
	nssfPeriod := nssfMonthly.Mul(decimal.NewFromInt(int64(days))).Div(keMonthlyDaysApprox).Round(2)
	if nssfPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "KE_NSSF",
			Name:   "NSSF (employee Tier I+II, KE)",
			Amount: nssfPeriod,
		})
	}

	// SHIF: 2.75% gross, min 300 monthly.
	shifMonthly := monthlyEquiv.Mul(keSHIFRate)
	if shifMonthly.LessThan(keSHIFMin) {
		shifMonthly = keSHIFMin
	}
	shifPeriod := shifMonthly.Mul(decimal.NewFromInt(int64(days))).Div(keMonthlyDaysApprox).Round(2)
	if shifPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "KE_SHIF",
			Name:   "SHIF (Social Health Insurance, KE)",
			Amount: shifPeriod,
		})
	}

	// Housing Levy: 1.5% gross, employee share.
	housingMonthly := monthlyEquiv.Mul(keHousingLevyRate)
	housingPeriod := housingMonthly.Mul(decimal.NewFromInt(int64(days))).Div(keMonthlyDaysApprox).Round(2)
	if housingPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "KE_HOUSING_LEVY",
			Name:   "Affordable Housing Levy (employee, KE)",
			Amount: housingPeriod,
		})
	}

	return out, nil
}

func walkKEBrackets(monthly decimal.Decimal) decimal.Decimal {
	var match keBracket
	matched := false
	for _, b := range keBrackets {
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
