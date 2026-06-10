package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// hkPack implements Hong Kong's payroll-side statutory withholdings.
//
// Hong Kong does NOT operate a pay-as-you-earn (PAYE) income-tax
// withholding system. Salaries Tax (Inland Revenue Ordinance Cap.
// 112) is assessed directly on the taxpayer through provisional and
// final assessments issued by the Inland Revenue Department (IRD);
// the employer's only income-tax obligation is the annual IR56B
// reporting return, not source withholding. The pack therefore
// emits NO income-tax deduction line.
//
// The one mandatory payroll-side employee deduction is the
// Mandatory Provident Fund (MPF) employee mandatory contribution
// under the Mandatory Provident Fund Schemes Ordinance (Cap. 485):
//
//   - Rate: 5% of the employee's relevant income.
//   - Minimum relevant income level: HK$7,100/month. An employee
//     whose relevant income for the contribution period is below
//     this floor makes NO mandatory contribution (the employer
//     still contributes its own 5%, but that is an employer cost,
//     not a slip deduction).
//   - Maximum relevant income level: HK$30,000/month, capping the
//     employee mandatory contribution at HK$1,500/month.
//
// The pack treats the slip's gross as the monthly relevant income
// (the dominant SME monthly-payroll case) and applies the monthly
// floor / ceiling directly, mirroring the SG pack's monthly-proxy
// simplification. Casual / daily-rated employees fall under a
// separate daily contribution schedule (HK$280 floor, HK$1,000
// ceiling per day) that the engine handles via a dedicated casual
// slip type; this pack covers the standard monthly contribution
// period.
//
// References:
//
//	MPFA contribution rules + min/max relevant income levels:
//	  https://www.mpfa.org.hk/en/mpf-system/system-features/contributions
//	Inland Revenue Ordinance Cap. 112 (Salaries Tax — assessed,
//	not withheld):
//	  https://www.elegislation.gov.hk/hk/cap112
//	Mandatory Provident Fund Schemes Ordinance Cap. 485:
//	  https://www.elegislation.gov.hk/hk/cap485
type hkPack struct{}

func init() { Register(&hkPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (hkPack) Country() string { return "HK" }

// EffectiveYear returns the calendar year the MPF relevant-income
// levels in this pack are calibrated for. The HK$7,100 floor has
// applied since Nov 2013 and the HK$30,000 ceiling since Jun 2014;
// both remain current for 2025.
func (hkPack) EffectiveYear() int { return 2025 }

var (
	// MPF employee mandatory contribution rate (MPFSO Cap. 485).
	hkMPFRate = dec("0.05")

	// Minimum relevant income level: below this monthly figure the
	// employee makes no mandatory contribution.
	hkMPFMinRelevantIncome = dec("7100")

	// Maximum relevant income level: caps the contribution base.
	hkMPFMaxRelevantIncome = dec("30000")
)

// ComputeWithholding emits HK_MPF_EMPLOYEE — the 5% Mandatory
// Provident Fund employee mandatory contribution, floored at the
// HK$7,100 minimum relevant income level and capped at the
// HK$30,000 maximum relevant income level. No income tax is
// withheld (Hong Kong Salaries Tax is assessed directly by the
// IRD, not deducted at source).
func (hkPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) || period.Days() <= 0 {
		return nil, nil
	}

	// Below the minimum relevant income level → no employee
	// mandatory contribution.
	if gross.LessThan(hkMPFMinRelevantIncome) {
		return nil, nil
	}

	base := gross
	if base.GreaterThan(hkMPFMaxRelevantIncome) {
		base = hkMPFMaxRelevantIncome
	}
	mpf := base.Mul(hkMPFRate).Round(2)
	if !mpf.IsPositive() {
		return nil, nil
	}
	return []Deduction{
		{
			Code:   "HK_MPF_EMPLOYEE",
			Name:   "MPF Mandatory Contribution (employee, HK)",
			Amount: mpf,
		},
	}, nil
}
