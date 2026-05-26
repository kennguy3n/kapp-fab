package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// zaPack implements South Africa's monthly payroll-side statutory
// withholdings:
//
//   - PAYE (Pay-As-You-Earn) per SARS' progressive personal income
//     tax tables (Income Tax Act 1962 s.5 + the annual Money Bill).
//     The pack walks the seven-bracket 2024/2025 schedule, annualises
//     the slip's gross via the period-to-year ratio, applies the
//     section 6(2) primary rebate (R17,235 for taxpayers under 65),
//     and proportions the resulting annual tax back onto the slip.
//     SARS's EMP201 "monthly equivalent" tables produce the same
//     result by construction; this pack computes it directly off
//     the annual schedule so the math stays correct for any pay
//     period (weekly / fortnightly / monthly) without separate
//     lookup tables.
//
//   - UIF (Unemployment Insurance Fund) employee contribution at
//     1% of remuneration, capped at the UIF Contributions Act 2002
//     monthly ceiling (R17,712 → max R177.12 / month, raised from
//     R14,872 effective 1 June 2021). The matching employer 1% is
//     paid by the employer separately and is not a payroll
//     deduction.
//
// SDL (Skills Development Levy, 1%) and OID (Occupational Injuries
// and Diseases) assessments are *employer* obligations and are not
// emitted by this pack — the slip ledger picks them up on the
// employer side.
//
// References:
//
//	SARS PAYE pocket guide 2024/2025 (Schedule 1 + s.5 + s.6(2)):
//	  https://www.sars.gov.za/types-of-tax/pay-as-you-earn/
//	UIF Contributions Act 2002:
//	  https://www.labour.gov.za/legislation/acts/unemployment-insurance/unemployment-insurance-contributions-act
type zaPack struct{}

func init() { Register(&zaPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services. Used by the registry to key lookups from
// tenants.country.
func (zaPack) Country() string { return "ZA" }

// EffectiveYear returns the fiscal year the ZA tables are
// calibrated for: South African 2024/2025 tax year (1 March 2024
// – 28 February 2025) per the 2024 SARS Pocket Guide.
func (zaPack) EffectiveYear() int { return 2024 }

// zaBracket is one row of SARS Table A (annual taxable income).
// Floor / Top are in ZAR per the published tables; Base is the
// cumulative tax owed at Floor and Rate is the marginal rate
// applied between Floor and Top. Top == decimal.Zero marks the
// open-ended top bracket.
type zaBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// SARS Schedule 1 for tax year 2024/2025 (1 Mar 2024 – 28 Feb 2025).
	zaBrackets = []zaBracket{
		{Floor: dec("0"), Top: dec("237100"), Base: dec("0"), Rate: dec("0.18")},
		{Floor: dec("237100"), Top: dec("370500"), Base: dec("42678"), Rate: dec("0.26")},
		{Floor: dec("370500"), Top: dec("512800"), Base: dec("77362"), Rate: dec("0.31")},
		{Floor: dec("512800"), Top: dec("673000"), Base: dec("121475"), Rate: dec("0.36")},
		{Floor: dec("673000"), Top: dec("857900"), Base: dec("179147"), Rate: dec("0.39")},
		{Floor: dec("857900"), Top: dec("1817000"), Base: dec("251258"), Rate: dec("0.41")},
		{Floor: dec("1817000"), Top: decimal.Zero, Base: dec("644489"), Rate: dec("0.45")},
	}

	// Section 6(2) primary rebate, 2024/2025. Pack applies the
	// under-65 rebate as the default; the secondary (65+) and
	// tertiary (75+) rebates require an Age threshold flag the
	// employee KRecord does not currently carry separately from
	// the SG age tier. The under-65 default is conservative
	// (smaller rebate → larger withholding) and the older-age
	// cohorts settle at year-end via the IT12 assessment.
	zaPrimaryRebateAnnual = dec("17235")

	// UIF (Unemployment Insurance Fund) employee share: 1% of
	// remuneration up to R17,712 / month. Cap is the contribution
	// itself (R177.12 / month) computed off the capped base.
	zaUIFRate              = dec("0.01")
	zaUIFMonthlyCeiling    = dec("17712")
	zaAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits ZA_PAYE and ZA_UIF for residents and
// non-residents alike (PAYE applies the same brackets and rebate;
// non-residents are only taxed on RSA-sourced income but the slip
// gross is already that). Zero-amount lines are omitted.
func (zaPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(zaAnnualPeriodFraction)
	annualGross := gross.Div(periodFraction)
	annualTax := walkZABrackets(annualGross)
	// Apply the section 6(2) primary rebate. Rebates reduce tax
	// payable but never below zero — a slip whose annualised
	// gross sits in the 18% bracket below the rebate emits no
	// PAYE line at all.
	annualTax = annualTax.Sub(zaPrimaryRebateAnnual)
	if annualTax.LessThan(decimal.Zero) {
		annualTax = decimal.Zero
	}
	periodPAYE := annualTax.Mul(periodFraction).Round(2)
	if periodPAYE.IsPositive() {
		out = append(out, Deduction{
			Code:   "ZA_PAYE",
			Name:   "PAYE (SARS, ZA)",
			Amount: periodPAYE,
		})
	}

	// UIF: 1% on monthly remuneration capped at R17,712. Convert
	// the slip's daily-equivalent gross back to a 30.4375-day
	// month so the cap behaves consistently for non-monthly
	// pay periods. Period-fraction here is the same one used
	// for PAYE annualisation; the monthly equivalent is
	// annualGross / 12.
	monthlyEquiv := annualGross.Div(decimal.NewFromInt(12))
	uifBase := monthlyEquiv
	if uifBase.GreaterThan(zaUIFMonthlyCeiling) {
		uifBase = zaUIFMonthlyCeiling
	}
	// Scale the (capped) monthly UIF back to the slip's actual
	// period so a weekly slip emits 7/30.4375 of the monthly UIF.
	uifMonthly := uifBase.Mul(zaUIFRate)
	uifPeriod := uifMonthly.Mul(decimal.NewFromInt(int64(days))).Div(decimal.NewFromFloat(30.4375)).Round(2)
	if uifPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "ZA_UIF",
			Name:   "UIF (employee share, ZA)",
			Amount: uifPeriod,
		})
	}

	return out, nil
}

// walkZABrackets walks the annual SARS schedule and returns the
// pre-rebate tax payable. Open-ended top bracket (Top == 0) is
// the catch-all for any income above the highest published floor.
func walkZABrackets(annual decimal.Decimal) decimal.Decimal {
	var match zaBracket
	matched := false
	for _, b := range zaBrackets {
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
