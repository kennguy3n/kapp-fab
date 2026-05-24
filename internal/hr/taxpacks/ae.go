package taxpacks

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// aePack implements the United Arab Emirates' payroll-side
// statutory withholdings.
//
//   - No personal income tax. The UAE has never levied a personal
//     income tax on employment income. Corporate Tax (Federal
//     Decree-Law No. 47 of 2022) applies at the legal-entity level
//     and is not a payroll deduction, so no employment-side income
//     line is ever emitted.
//
//   - GPSSA pension contribution: 5% employee share of the monthly
//     contribution base (Federal Law No. 7 of 1999 as amended by
//     Federal Decree-Law No. 57 of 2023, in force from 31 October
//     2023). Applies *only* to UAE nationals and (since 2024,
//     reciprocally) GCC nationals working in the UAE under the
//     unified GCC Protection Extension Scheme. Non-GCC expats
//     (the overwhelming majority of UAE payroll) pay nothing — the
//     pack reflects this by emitting an empty deduction slice.
//
//     The contribution base is capped at AED 50,000 / month
//     (GPSSA Resolution No. 25 of 2023). Beyond the cap, the 5%
//     employee share saturates at AED 2,500.
//
//   - End-of-service gratuity (Article 51 of Federal Decree-Law No.
//     33 of 2021) is an *accrued employer liability*, not a payroll
//     deduction from the employee's salary. The pack therefore does
//     not emit a gratuity line; the accrual is handled by the CoA
//     template in PR-3 (ae_ifrs_basic.json).
//
// EmployeeInfo gating:
//
//   - Nationality == "local" (case-insensitive) selects the GPSSA
//     branch. The taxpacks.EmployeeInfo documentation for
//     Nationality covers this default-to-"expat" convention; the
//     UAE pack honours it exactly.
//
// References:
//
//	GPSSA contribution rates after the 2023 amendment:
//	  https://www.gpssa.gov.ae/en/Pages/Contributions.aspx
//	Federal Decree-Law No. 47 of 2022 (Corporate Tax) — confirms
//	there is no Personal Income Tax:
//	  https://mof.gov.ae/corporate-tax/
//	Federal Decree-Law No. 33 of 2021, Art. 51 (gratuity):
//	  https://mohre.gov.ae/en/laws-and-regulations.aspx
type aePack struct{}

func init() { Register(&aePack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (aePack) Country() string { return "AE" }

// EffectiveYear returns the fiscal year the AE tables are
// calibrated for: 2024 (post the October 2023 GPSSA rate-update
// + January 2024 GCC reciprocity). The no-PIT position has been
// stable since federation in 1971.
func (aePack) EffectiveYear() int { return 2024 }

var (
	// GPSSA employee contribution rate after the Federal
	// Decree-Law No. 57 of 2023 amendment.
	aeGPSSAEmployeeRate = dec("0.05")

	// Monthly contribution-base cap, AED. GPSSA Resolution
	// No. 25 of 2023. Above this, the 5% saturates.
	aeGPSSACeiling = dec("50000")
)

// ComputeWithholding emits AE_GPSSA for UAE / GCC nationals and
// returns an empty slice for expats (the legally correct
// result — there is no employee-side withholding for non-nationals
// in the UAE). Negative or zero gross / period return nil.
func (aePack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	// Expat / unknown nationality → no employee deduction.
	if !isGCCNational(e.Nationality) {
		return nil, nil
	}

	base := gross
	if base.GreaterThan(aeGPSSACeiling) {
		base = aeGPSSACeiling
	}
	gpssa := base.Mul(aeGPSSAEmployeeRate).Round(2)
	if !gpssa.IsPositive() {
		return nil, nil
	}
	return []Deduction{{
		Code:   "AE_GPSSA",
		Name:   "GPSSA pension (employee share, AE)",
		Amount: gpssa,
	}}, nil
}

// isGCCNational normalises Nationality to the "local" branch for
// every GCC pack. Empty defaults to "expat" per EmployeeInfo's
// documented convention. "local" is the canonical KRecord value
// for a national of the country whose pack is running; a tenant
// running GCC-reciprocal payroll for, say, a Bahraini national
// employed in the UAE today still records Nationality = "local"
// from the *UAE pack's* perspective because the reciprocal scheme
// makes them eligible for GPSSA contributions. Per-jurisdiction
// gating beyond this binary is out of scope for PR-2c and tracked
// in docs/TAX_PACK_MAINTENANCE.md.
func isGCCNational(nat string) bool {
	return strings.EqualFold(strings.TrimSpace(nat), "local")
}
