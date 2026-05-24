package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// qaPack implements Qatar's payroll-side statutory withholdings.
//
//   - No personal income tax. Qatar has never levied a personal
//     income tax on employment income (Income Tax Law No. 24 of
//     2018 applies to corporate / business income only).
//
//   - GRSIA (General Retirement and Social Insurance Authority)
//     retirement-fund employee contribution: 5% of basic + social
//     allowance, capped at QAR 100,000 / month, applicable only to
//     Qatari nationals working for the public *or* private sector
//     (Law No. 1 of 2022 unified the schemes from 1 Jan 2023).
//
//   - Non-Qataris and unregistered domestic-help categories pay no
//     employee social-security contribution. The pack emits an
//     empty deduction slice for them — the legally correct result.
//
// End-of-service gratuity (Law No. 14 of 2004 Labour Law, Art. 54)
// is an employer-side accrual, not a payroll deduction; the
// QA CoA template handles the liability account in PR-3.
//
// References:
//
//	GRSIA contribution overview:
//	  https://www.grsia.gov.qa/en/Pages/contribution.aspx
//	Law No. 1 of 2022 (retirement / social insurance unification):
//	  https://www.almeezan.qa/LawView.aspx?opt&LawID=8459&language=en
type qaPack struct{}

func init() { Register(&qaPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (qaPack) Country() string { return "QA" }

// EffectiveYear returns the fiscal year the QA tables are
// calibrated for: 2024 (Law No. 1 of 2022 fully in force since
// 1 January 2023; the rate has been stable since).
func (qaPack) EffectiveYear() int { return 2024 }

var (
	// GRSIA employee contribution rate, Law No. 1 of 2022.
	qaRetirementEmployeeRate = dec("0.05")

	// Monthly contribution-base cap, QAR.
	qaRetirementCeiling = dec("100000")
)

// ComputeWithholding emits QA_RETIREMENT (5%) for Qatari
// nationals; non-Qataris and missing-nationality cases return
// nil. Negative or zero gross / period return nil.
func (qaPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	if !isGCCNational(e.Nationality) {
		return nil, nil
	}

	base := gross
	if base.GreaterThan(qaRetirementCeiling) {
		base = qaRetirementCeiling
	}
	r := base.Mul(qaRetirementEmployeeRate).Round(2)
	if !r.IsPositive() {
		return nil, nil
	}
	return []Deduction{{
		Code:   "QA_RETIREMENT",
		Name:   "GRSIA retirement fund (employee share, QA)",
		Amount: r,
	}}, nil
}
