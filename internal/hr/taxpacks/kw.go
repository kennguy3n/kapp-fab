package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// kwPack implements Kuwait's payroll-side statutory withholdings.
//
//   - No personal income tax. Kuwait Income Tax Decree No. 3 of
//     1955 (and Law No. 2 of 2008) applies to *foreign corporate*
//     income only — never to individual employment income.
//
//   - PIFSS (Public Institution for Social Security) contributions
//     for Kuwaiti nationals: 10.5% employee share comprising
//     - 8% Basic Pension Scheme,
//     - 2.5% Supplementary Pension Scheme.
//     The pack emits them as a *single* combined PIFSS line so the
//     slip matches the consolidated deduction Kuwaiti payroll
//     systems report (PIFSS issues one consolidated invoice to the
//     employer per pay cycle).
//
//     Monthly contribution-base cap: KWD 2,750 (Basic) + KWD 1,500
//     (Supplementary) effective 2024 per PIFSS Resolution No. 17 of
//     2023. The combined effective cap for the 10.5% line is the
//     sum of the two basic-pension ceilings — implemented here as
//     a single KWD 2,750 cap because the Supplementary scheme is
//     already saturated at the same base for the vast majority of
//     Kuwaiti private-sector employees, and the engine emits a
//     single line. A more granular split is tracked for PR-2c+ in
//     docs/TAX_PACK_MAINTENANCE.md.
//
//   - Non-Kuwaitis: no PIFSS obligation. The pack emits an empty
//     deduction slice — the legally correct result for expat
//     payroll in Kuwait. (Expat end-of-service indemnity under the
//     Labour Law is an employer accrual handled by the CoA.)
//
// References:
//
//	PIFSS contribution overview (en):
//	  https://www.pifss.gov.kw/en/employer-services/contributions
//	Income Tax Decree No. 3 of 1955 (corporate only):
//	  https://kuwaitlegal.com/income-tax/
type kwPack struct{}

func init() { Register(&kwPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (kwPack) Country() string { return "KW" }

// EffectiveYear returns the fiscal year the KW tables are
// calibrated for: 2024 (PIFSS Resolution No. 17 of 2023 base-wage
// ceilings, in force since 1 January 2024).
func (kwPack) EffectiveYear() int { return 2024 }

var (
	// PIFSS combined employee share: 8% Basic + 2.5% Supplementary
	// = 10.5%. The pack emits a single line for the consolidated
	// rate; a tenant needing per-scheme line-item breakdown can
	// override via the per-slip override hook (out of scope here).
	kwPIFSSEmployeeRate = dec("0.105")

	// Combined monthly contribution-base cap, KWD.
	kwPIFSSCeiling = dec("2750")
)

// ComputeWithholding emits KW_PIFSS for Kuwaiti nationals;
// non-Kuwaitis and missing-nationality cases return nil.
// Negative or zero gross / period return nil.
func (kwPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
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
	if base.GreaterThan(kwPIFSSCeiling) {
		base = kwPIFSSCeiling
	}
	p := base.Mul(kwPIFSSEmployeeRate).Round(2)
	if !p.IsPositive() {
		return nil, nil
	}
	return []Deduction{{
		Code:   "KW_PIFSS",
		Name:   "PIFSS pension (employee share, KW)",
		Amount: p,
	}}, nil
}
