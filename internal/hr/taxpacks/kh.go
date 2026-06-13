package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// khPack implements Cambodia's monthly payroll-side statutory
// withholdings:
//
//   - Tax on Salary (ToS / អាករលើប្រាក់បៀវត្ស) per the Law on
//     Taxation and Sub-Decree No. 196 ANKr.BK (2022). The schedule
//     is a genuinely MONTHLY progressive table (KHR), so the pack
//     applies it directly to the slip's monthly gross without
//     annualisation. A monthly dependent relief of KHR 150,000 per
//     minor child / dependent spouse (Sub-Decree 196 Art. 2)
//     reduces the taxable base.
//
//   - National Social Security Fund (NSSF) pension-scheme employee
//     contribution. The pension scheme (in force since Oct 2022)
//     charges 4% of the contributory wage for the first five years,
//     split 2% employee / 2% employer, on a contributory wage
//     graded between KHR 400,000 and KHR 1,200,000 per month. The
//     occupational-risk (0.8%) and health-care (2.6%) NSSF schemes
//     are employer-borne and are not slip deductions.
//
// Non-residents are taxed at a flat 20% on Cambodian-source salary
// (Law on Taxation Art. 42) with no dependent relief and no NSSF.
//
// References:
//
//	Law on Taxation (Tax on Salary, Art. 42–48):
//	  https://www.tax.gov.kh/en/law-on-taxation
//	Sub-Decree No. 196 ANKr.BK (2022 ToS monthly table + relief):
//	  https://www.tax.gov.kh/en/
//	NSSF pension scheme (Sub-Decree 32, 2021; contributory-wage
//	bands + 4% rate):
//	  https://www.nssf.gov.kh/
type khPack struct{}

func init() { Register(&khPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (khPack) Country() string { return "KH" }

// EffectiveYear returns the calendar year the KH tables are
// calibrated for: 2025 (ToS monthly table per Sub-Decree 196,
// NSSF pension contributory-wage bands current for 2025).
func (khPack) EffectiveYear() int { return 2025 }

type khBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Monthly Tax on Salary table (KHR). Base is cumulative tax at
	// each Floor. Applied directly to monthly taxable salary — the
	// schedule is published per month, not per year.
	khBracketsMonthly = []khBracket{
		{Floor: dec("0"), Top: dec("1500000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("1500000"), Top: dec("2000000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("2000000"), Top: dec("8500000"), Base: dec("25000"), Rate: dec("0.10")},
		{Floor: dec("8500000"), Top: dec("12500000"), Base: dec("675000"), Rate: dec("0.15")},
		{Floor: dec("12500000"), Top: decimal.Zero, Base: dec("1275000"), Rate: dec("0.20")},
	}

	// Monthly dependent relief per minor child / dependent spouse.
	khDependentRelief = dec("150000")

	// NSSF pension employee share (2% of contributory wage) and the
	// contributory-wage floor / ceiling.
	khNSSFPensionEmployeeRate = dec("0.02")
	khNSSFWageFloor           = dec("400000")
	khNSSFWageCeiling         = dec("1200000")

	// Non-resident flat rate on Cambodian-source salary.
	khNonResidentRate = dec("0.20")

	// Defensive clamp on dependent count (data-entry guard).
	khMaxDependents = 15
)

// ComputeWithholding emits KH_TOS (monthly Tax on Salary) and
// KH_NSSF_PENSION (employee pension contribution) for residents.
// Non-residents get a single KH_NONRESIDENT_TAX flat-rate line.
func (khPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) || period.Days() <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	if !e.Resident {
		nr := e.IncomeTaxBase(gross).Mul(khNonResidentRate).Round(2)
		if nr.IsPositive() {
			out = append(out, Deduction{
				Code:   "KH_NONRESIDENT_TAX",
				Name:   "Non-resident withholding tax (KH)",
				Amount: nr,
			})
		}
		return out, nil
	}

	deps := e.NumDependents
	if deps < 0 {
		deps = 0
	}
	if deps > khMaxDependents {
		deps = khMaxDependents
	}
	taxable := e.IncomeTaxBase(gross).Sub(khDependentRelief.Mul(decimal.NewFromInt(int64(deps))))
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	tos := walkKHBrackets(taxable, khBracketsMonthly).Round(2)
	if tos.IsPositive() {
		out = append(out, Deduction{
			Code:   "KH_TOS",
			Name:   "Tax on Salary (KH)",
			Amount: tos,
		})
	}

	// NSSF pension: 2% of the contributory wage, which is the
	// contribution base (full gross by default) banded between the
	// floor and ceiling.
	pensionBase := e.ContributionBase(gross)
	if pensionBase.LessThan(khNSSFWageFloor) {
		pensionBase = khNSSFWageFloor
	}
	if pensionBase.GreaterThan(khNSSFWageCeiling) {
		pensionBase = khNSSFWageCeiling
	}
	pension := pensionBase.Mul(khNSSFPensionEmployeeRate).Round(2)
	if pension.IsPositive() {
		out = append(out, Deduction{
			Code:   "KH_NSSF_PENSION",
			Name:   "NSSF pension contribution (employee, KH)",
			Amount: pension,
		})
	}

	return out, nil
}

func walkKHBrackets(monthly decimal.Decimal, scale []khBracket) decimal.Decimal {
	var match khBracket
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
