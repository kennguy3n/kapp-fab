package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// lbPack implements Lebanon's payroll-side statutory withholdings:
//
//   - Payroll income tax on salaries and wages ("R10") per Decree-
//     Law No. 144/1959 (Income Tax Law), Chapter I, as re-scaled by
//     Budget Law 2024. The pack annualises the slip's gross
//     (days / 365.25), applies the annual personal exemption,
//     walks the progressive schedule (LBP) and prorates the annual
//     tax back to the slip period.
//
//   - National Social Security Fund (NSSF) employee contribution for
//     the sickness & maternity branch: 3% of the contributory wage,
//     capped at the NSSF sickness/maternity ceiling. The family-
//     allowance and end-of-service-indemnity branches are employer-
//     funded and are not slip deductions.
//
// IMPORTANT — high review cadence: Lebanon's tax brackets, personal
// exemption and the NSSF contributory ceiling have been re-scaled
// repeatedly since 2019 to track the collapse of the lira. The LBP
// figures encoded here reflect Budget Law 2024 and the 2023/2024
// NSSF circulars; they MUST be re-checked on every Budget Law and
// CNSS circular (see docs/TAX_PACK_MAINTENANCE.md). Employers
// running USD "fresh dollar" payroll should configure a USD-
// denominated pack variant rather than relying on these LBP bands.
//
// References:
//
//	Income Tax Law (Decree-Law 144/1959) + Budget Law 2024:
//	  http://www.finance.gov.lb/
//	Ministry of Finance payroll-tax circulars:
//	  http://www.finance.gov.lb/en-us/Finance/TaxesFees
//	National Social Security Fund (CNSS) contribution rates:
//	  http://www.cnss.gov.lb/
type lbPack struct{}

func init() { Register(&lbPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (lbPack) Country() string { return "LB" }

// EffectiveYear returns the calendar year the LB bands are
// calibrated for: 2024 (Budget Law 2024 R10 schedule + 2023/2024
// NSSF sickness/maternity ceiling).
func (lbPack) EffectiveYear() int { return 2024 }

type lbBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Budget Law 2024 R10 progressive schedule (annual taxable
	// income after the personal exemption, LBP). Base is cumulative
	// tax at each Floor.
	lbBracketsResident = []lbBracket{
		{Floor: dec("0"), Top: dec("360000000"), Base: dec("0"), Rate: dec("0.04")},
		{Floor: dec("360000000"), Top: dec("900000000"), Base: dec("14400000"), Rate: dec("0.07")},
		{Floor: dec("900000000"), Top: dec("1800000000"), Base: dec("52200000"), Rate: dec("0.12")},
		{Floor: dec("1800000000"), Top: dec("3600000000"), Base: dec("160200000"), Rate: dec("0.16")},
		{Floor: dec("3600000000"), Top: dec("7200000000"), Base: dec("448200000"), Rate: dec("0.21")},
		{Floor: dec("7200000000"), Top: dec("13500000000"), Base: dec("1204200000"), Rate: dec("0.25")},
		{Floor: dec("13500000000"), Top: decimal.Zero, Base: dec("2779200000"), Rate: dec("0.27")},
	}

	// Annual personal exemption (Budget Law 2024, LBP).
	lbPersonalExemption = dec("450000000")

	// NSSF employee sickness/maternity contribution: 3% of the
	// contributory wage, capped at the monthly ceiling.
	lbNSSFEmployeeRate = dec("0.03")
	lbNSSFWageCeiling  = dec("90000000")

	lbAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits LB_INCOME_TAX (annualised progressive R10
// payroll tax) and LB_NSSF (NSSF sickness/maternity employee
// contribution).
func (lbPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(lbAnnualPeriodFraction)
	annualGross := e.IncomeTaxBase(gross).Div(periodFraction)
	taxable := annualGross.Sub(lbPersonalExemption)
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	annualTax := walkLBBrackets(taxable, lbBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "LB_INCOME_TAX",
			Name:   "Payroll income tax R10 (LB)",
			Amount: periodTax,
		})
	}

	// NSSF runs on the contribution base (full gross by default).
	nsssBase := e.ContributionBase(gross)
	if nsssBase.GreaterThan(lbNSSFWageCeiling) {
		nsssBase = lbNSSFWageCeiling
	}
	nssf := nsssBase.Mul(lbNSSFEmployeeRate).Round(2)
	if nssf.IsPositive() {
		out = append(out, Deduction{
			Code:   "LB_NSSF",
			Name:   "NSSF sickness/maternity contribution (employee, LB)",
			Amount: nssf,
		})
	}

	return out, nil
}

func walkLBBrackets(annual decimal.Decimal, scale []lbBracket) decimal.Decimal {
	var match lbBracket
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
