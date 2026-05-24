package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// saPack implements Saudi Arabia's payroll-side statutory
// withholdings.
//
//   - No personal income tax. KSA has never levied a personal income
//     tax on employment income; the 2018 VAT introduction and 2023
//     VAT rate hike are consumption taxes, not payroll. Zakat applies
//     to legal entities, not employees.
//
//   - GOSI (General Organization for Social Insurance) contributions
//     for Saudi national employees:
//       - Annuities branch (retirement / pension): 9% employee share.
//       - SANED (unemployment insurance): 0.75% employee share.
//     Both apply to the contribution wage (basic + housing allowance,
//     in practice approximated by gross at the payroll cadence the
//     engine emits) up to the SAR 45,000 / month statutory ceiling
//     (GOSI Royal Decree No. M/45 of 1421H + Law of Unemployment
//     Insurance, Royal Decree No. M/18 of 1435H).
//
//   - Non-Saudi employees: zero employee deduction. The employer
//     pays a 2% GOSI Occupational Hazards contribution on their
//     behalf — that is *not* a payroll deduction so the pack does
//     not emit a line for it.
//
// GOSI employee-rate breakdown matches the published GOSI website
// schedule (effective 1 Jul 2024 — the 2024 reform raised the
// employee Annuities rate from 9% to 9.5% phased over a decade,
// but the first phase took effect 1 Jul 2025 not 2024; this pack
// implements the FY 2024 figures which were 9% Annuities + 0.75%
// SANED). Maintainers should bump this in line with the
// scheduled rate ramp.
//
// References:
//
//	GOSI contribution rates (FY 2024):
//	  https://www.gosi.gov.sa/GOSIOnline/Contributions
//	Royal Decree No. M/18 of 1435H (SANED):
//	  https://www.boe.gov.sa/ViewSystemDetails.aspx?lang=en&SystemID=287
//	Royal Decree No. M/45 of 1421H (GOSI law):
//	  https://www.boe.gov.sa/ViewSystemDetails.aspx?lang=en&SystemID=63
type saPack struct{}

func init() { Register(&saPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (saPack) Country() string { return "SA" }

// EffectiveYear returns the fiscal year the SA tables are
// calibrated for: 2024. The 2025 phased rate ramp (Royal Decree
// No. M/273 of 1445H, Phase 1) is *not* applied here so this
// pack only matches FY 2024 payroll runs. PR-2c+ will revisit
// when the phased schedule is fully wired.
func (saPack) EffectiveYear() int { return 2024 }

var (
	// FY 2024 GOSI employee shares. The Annuities rate steps up
	// 0.5pp / year starting 1 Jul 2025 — see EffectiveYear note.
	saGOSIAnnuitiesEmployeeRate = dec("0.09")
	saGOSISanedEmployeeRate     = dec("0.0075")

	// Monthly contribution-wage cap, SAR. Both branches share
	// the same ceiling.
	saGOSICeiling = dec("45000")
)

// ComputeWithholding emits SA_GOSI_PENSION (9%) + SA_GOSI_SANED
// (0.75%) for Saudi nationals (Nationality == "local") and an
// empty slice for non-Saudis (the employer-paid 2% Occupational
// Hazards branch is not an employee deduction). Negative or zero
// gross / period return nil.
func (saPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	// Non-Saudi → no employee deduction.
	if !isGCCNational(e.Nationality) {
		return nil, nil
	}

	base := gross
	if base.GreaterThan(saGOSICeiling) {
		base = saGOSICeiling
	}

	out := []Deduction{}
	if p := base.Mul(saGOSIAnnuitiesEmployeeRate).Round(2); p.IsPositive() {
		out = append(out, Deduction{
			Code:   "SA_GOSI_PENSION",
			Name:   "GOSI Annuities (employee share, SA)",
			Amount: p,
		})
	}
	if s := base.Mul(saGOSISanedEmployeeRate).Round(2); s.IsPositive() {
		out = append(out, Deduction{
			Code:   "SA_GOSI_SANED",
			Name:   "GOSI SANED unemployment insurance (employee share, SA)",
			Amount: s,
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
