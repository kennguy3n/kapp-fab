package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// bhPack implements Bahrain's payroll-side statutory withholdings.
//
//   - No personal income tax. Bahrain has no personal income tax;
//     the 2018 VAT and 2025 corporate income tax on multinational
//     enterprises (DMTT, in force from 1 January 2025) are not
//     payroll deductions.
//
//   - SIO (Social Insurance Organisation) contributions, split by
//     nationality (Law No. 24 of 1976 + Royal Decree No. 78 of
//     2006 + Royal Decree No. 1 of 2022 rate ramp):
//
//       Bahraini nationals (employee share):
//         - 8% Pension (Old-Age, Disability, Death)
//         - 1% Unemployment
//
//       Non-Bahraini employees (employee share):
//         - 1% Unemployment only.
//         - The employer pays 3% Occupational Hazards on their
//           behalf which is *not* an employee deduction so the pack
//           does not emit a line for it.
//
//     Monthly contribution-wage cap: BHD 4,000 (Bahraini) / BHD
//     4,000 (non-Bahraini for the unemployment branch). The 1%
//     unemployment rate has applied to both groups since 2007.
//
// Note on the 2022 rate ramp (Royal Decree No. 1 of 2022): the
// Bahraini pension rate is scheduled to rise 1pp / year starting
// 1 May 2022 from 7% to 11% by 2026. This pack implements the FY
// 2024 figure (8%); maintainers must bump in line with the
// schedule. See docs/TAX_PACK_MAINTENANCE.md.
//
// References:
//
//	SIO contribution overview:
//	  https://www.sio.gov.bh/Information/SocialInsurance
//	Royal Decree No. 1 of 2022 (pension rate ramp):
//	  https://www.legalaffairs.gov.bh/AdvancedSearchDetails.aspx?id=46870
type bhPack struct{}

func init() { Register(&bhPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (bhPack) Country() string { return "BH" }

// EffectiveYear returns the fiscal year the BH tables are
// calibrated for: 2024 (Royal Decree No. 1 of 2022 Phase 3 rate
// of 8%, in force 1 May 2024 – 30 April 2025).
func (bhPack) EffectiveYear() int { return 2024 }

var (
	// Bahraini nationals — split employee shares.
	bhSIOBahrainiPensionRate      = dec("0.08")
	bhSIOBahrainiUnemploymentRate = dec("0.01")

	// Non-Bahraini employees — unemployment only.
	bhSIONonBahrainiUnemploymentRate = dec("0.01")

	// Monthly contribution-wage cap, BHD. Same ceiling for both
	// nationality groups under the unified scheme.
	bhSIOCeiling = dec("4000")
)

// ComputeWithholding emits BH_SIO_PENSION + BH_SIO_UNEMPLOYMENT
// for Bahraini nationals (8% + 1%) and only BH_SIO_UNEMPLOYMENT
// for non-Bahrainis (1%). Both branches honour the BHD 4,000 cap
// and the standard non-positive / zero-period guards.
func (bhPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	base := gross
	if base.GreaterThan(bhSIOCeiling) {
		base = bhSIOCeiling
	}

	out := []Deduction{}
	if isGCCNational(e.Nationality) {
		if p := base.Mul(bhSIOBahrainiPensionRate).Round(2); p.IsPositive() {
			out = append(out, Deduction{
				Code:   "BH_SIO_PENSION",
				Name:   "SIO pension (employee share, BH)",
				Amount: p,
			})
		}
		if u := base.Mul(bhSIOBahrainiUnemploymentRate).Round(2); u.IsPositive() {
			out = append(out, Deduction{
				Code:   "BH_SIO_UNEMPLOYMENT",
				Name:   "SIO unemployment insurance (employee share, BH)",
				Amount: u,
			})
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	}

	// Non-Bahraini path — unemployment only.
	if u := base.Mul(bhSIONonBahrainiUnemploymentRate).Round(2); u.IsPositive() {
		out = append(out, Deduction{
			Code:   "BH_SIO_UNEMPLOYMENT",
			Name:   "SIO unemployment insurance (employee share, BH)",
			Amount: u,
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
