package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// sgPack implements Singapore's payroll-side statutory withholdings:
//
//   - No PAYE for residents: Singapore tax (Income Tax) is self-
//     assessed annually via IR8A / Form B / Form B1. Employers have
//     no monthly Pay-As-You-Earn obligation, so ComputeWithholding
//     emits no income-tax line for resident employees. This is the
//     legally correct behaviour, not a stub.
//
//   - 15% flat withholding on the gross for non-resident
//     employment income (Income Tax Act s.40A; IRAS e-Tax Guide
//     "Tax for Non-Resident Employees"). Non-residents are taxed
//     at the higher of 15% flat or the resident progressive
//     schedule; the resident progressive schedule only exceeds 15%
//     above ~SGD 320k of chargeable income, so the flat 15% is the
//     usual outcome and is what this pack applies. The resident
//     scale would only matter for highly compensated non-residents
//     who should be referred to a tax adviser regardless.
//
//   - CPF (Central Provident Fund) employee contribution at the
//     statutory rates effective 1 Jan 2025 (CPF Board, "CPF
//     Contribution Rates from 1 January 2025"). Tiers step at age
//     55 / 60 / 65 / 70. The Ordinary Wage ceiling for 2025 is
//     SGD 7,400 / month; the engine treats EmployeeInfo.Resident
//     as the SG citizen / PR proxy because foreign work-permit
//     holders (EP / SP / WP) have no CPF obligation.
//
// References:
//
//	IRAS Tax for Non-Resident Employees (s.40A flat 15%):
//	  https://www.iras.gov.sg/taxes/individual-income-tax/employees/tax-for-non-resident-employees
//	CPF Board contribution rates 2025:
//	  https://www.cpf.gov.sg/employer/employer-obligations/cpf-contributions-for-your-employees
type sgPack struct{}

func init() { Register(&sgPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services. Used by the registry to key lookups from
// tenants.country.
func (sgPack) Country() string { return "SG" }

// EffectiveYear returns the fiscal year the SG tables are
// calibrated for: 2025 IRAS / CPF Board schedules. The CPF tier
// table is the published 1 January 2025 set; the non-resident
// flat rate has been 15% since 2009.
func (sgPack) EffectiveYear() int { return 2025 }

// sgCPFTier is one row of the CPF Contribution Rates table.
// UpperAge is exclusive against the lookup expression `age <
// UpperAge`, so a row with UpperAge==56 matches every age in
// [0, 55]. The CPF Board's published tier definitions use
// inclusive lower bounds ("55 and below", "above 55 to 60",
// etc.) so the UpperAge stored here is `inclusive_upper + 1`.
// Rates are the *employee* share only — the employer share is
// not a payroll deduction.
type sgCPFTier struct {
	UpperAge int             // exclusive upper bound. 0 = open-ended (oldest tier).
	Rate     decimal.Decimal // employee CPF share (proportion of OW).
}

var (
	// CPF Board contribution rates effective 1 Jan 2025
	// (Singapore citizens / Permanent Residents, third-year onward).
	// First-/second-year PR rates are lower; this pack implements the
	// standard schedule because the engine does not yet track PR
	// vesting year. Operators can override via the per-slip override
	// hook once introduced; for now the conservative-higher rate is
	// applied, which is the IRAS / CPF Board expectation absent an
	// explicit "graduated" payroll classification on the KRecord.
	// UpperAge values are `inclusive_upper + 1` against the
	// `age < UpperAge` test in resolveCPFEmployeeRate. The CPF
	// Board defines the schedule with inclusive lower bounds
	// ("55 and below" → 20%, "above 55 to 60" → 17%, etc.) so a
	// 55-year-old must match the 20% tier, not 17%. See the
	// boundary-age tests in apac_packs_test.go that pin every
	// edge (55/56, 60/61, 65/66, 70/71) against this table.
	sgCPFTiers = []sgCPFTier{
		{UpperAge: 56, Rate: dec("0.20")},  // ≤55
		{UpperAge: 61, Rate: dec("0.17")},  // 56-60
		{UpperAge: 66, Rate: dec("0.115")}, // 61-65
		{UpperAge: 71, Rate: dec("0.075")}, // 66-70
		{UpperAge: 0, Rate: dec("0.05")},   // 71+
	}

	// 2025 Ordinary Wage ceiling per CPF Board. Beyond this, CPF
	// only accrues on the AW (Additional Wages) ceiling, which is
	// out of scope for this minimum-viable cut — additional wages
	// are paid separately and would arrive through a different
	// payroll component. The OW cap is the production-correct
	// behaviour for a regular monthly salary slip below SGD 7,400.
	sgCPFMonthlyOWCeiling = dec("7400")

	// Singapore non-resident employment income flat withholding.
	// 15% per Income Tax Act s.40A.
	sgNonResidentRate = dec("0.15")
)

// ComputeWithholding emits SG_NONRESIDENT_TAX (15% flat) for
// non-residents and SG_CPF_EMPLOYEE (tiered by age, capped at the
// 2025 OW ceiling) for citizens / PRs. Both branches return nil
// for non-positive gross or zero-day periods.
func (sgPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Non-resident path: flat 15% withholding on the gross. No
	// CPF — CPF is restricted to citizens and PRs by statute.
	if !e.Resident {
		out = append(out, Deduction{
			Code:   "SG_NONRESIDENT_TAX",
			Name:   "Non-resident withholding tax (SG)",
			Amount: gross.Mul(sgNonResidentRate).Round(2),
		})
		return out, nil
	}

	// Resident path: no PAYE, only CPF. Resolve the age tier; an
	// unknown / zero age conservatively maps to the youngest tier
	// (highest employee CPF rate) — over-withholding is refundable
	// by CPF Board adjustment in the next pay run.
	rate := resolveCPFEmployeeRate(e.Age)

	// CPF is applied to gross capped at the OW ceiling.
	owBase := gross
	if owBase.GreaterThan(sgCPFMonthlyOWCeiling) {
		owBase = sgCPFMonthlyOWCeiling
	}
	cpf := owBase.Mul(rate).Round(2)
	if cpf.IsPositive() {
		out = append(out, Deduction{
			Code:   "SG_CPF_EMPLOYEE",
			Name:   "CPF (employee share, SG)",
			Amount: cpf,
		})
	}
	return out, nil
}

// resolveCPFEmployeeRate returns the employee CPF contribution rate
// for the given age. Ages ≤0 fall to the youngest tier (highest
// rate) — the safer side of an unknown KRecord.
func resolveCPFEmployeeRate(age int) decimal.Decimal {
	if age <= 0 {
		return sgCPFTiers[0].Rate
	}
	for _, t := range sgCPFTiers {
		if t.UpperAge == 0 {
			return t.Rate
		}
		if age < t.UpperAge {
			return t.Rate
		}
	}
	// Unreachable — the open-ended tier (UpperAge == 0) handles
	// every age past 70 — but a fail-safe default keeps the
	// compiler happy and guards against a future table edit that
	// accidentally drops the open-ended row.
	return sgCPFTiers[len(sgCPFTiers)-1].Rate
}
