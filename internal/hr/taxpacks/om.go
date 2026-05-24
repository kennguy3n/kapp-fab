package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// omPack implements Oman's payroll-side statutory withholdings.
//
//   - No personal income tax for the FY 2024 cadence this pack is
//     calibrated for. Oman announced a Personal Income Tax (Royal
//     Decree No. 56/2025) that takes effect from 1 January 2028 at
//     5% on annual income above OMR 42,000 — *not* before. The
//     pack therefore emits no employment-income line; PR-2c+ will
//     wire the 2028 schedule when its effective date approaches.
//
//   - PASI (Public Authority for Social Insurance) contributions
//     for Omani nationals: 8% employee share comprising 7% Pension
//     + 1% Disability / Death (Royal Decree No. 72/91, as amended
//     by Royal Decree No. 61 of 2013).
//
//     Monthly contribution-wage cap: OMR 3,000 per PASI Resolution
//     No. 522 of 2019.
//
//   - Non-Omanis pay no employee social-security contribution. The
//     employer's 1% Occupational Hazards branch for non-Omanis is
//     not an employee deduction. End-of-service gratuity (Royal
//     Decree No. 35/2003 Labour Law, Article 39) is an accrued
//     employer liability.
//
// References:
//
//	PASI contribution overview:
//	  https://www.pasi.gov.om/en/contributions
//	Royal Decree No. 56/2025 (PIT, effective 1 Jan 2028):
//	  https://omanobserver.om/article/oman-issues-personal-income-tax-law
type omPack struct{}

func init() { Register(&omPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (omPack) Country() string { return "OM" }

// EffectiveYear returns the fiscal year the OM tables are
// calibrated for: 2024 (PASI Resolution No. 522 of 2019 ceilings,
// pre-PIT cadence). The 2028 PIT rollout is tracked in
// docs/TAX_PACK_MAINTENANCE.md.
func (omPack) EffectiveYear() int { return 2024 }

var (
	// PASI employee combined share: 7% Pension + 1% Disability /
	// Death = 8%. Emitted as a single PASI line to match the
	// consolidated invoice PASI issues to the employer per pay
	// cycle.
	omPASIEmployeeRate = dec("0.08")

	// Monthly contribution-wage cap, OMR.
	omPASICeiling = dec("3000")
)

// ComputeWithholding emits OM_PASI for Omani nationals; non-
// Omanis and missing-nationality cases return nil. Negative or
// zero gross / period return nil.
func (omPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
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
	if base.GreaterThan(omPASICeiling) {
		base = omPASICeiling
	}
	p := base.Mul(omPASIEmployeeRate).Round(2)
	if !p.IsPositive() {
		return nil, nil
	}
	return []Deduction{{
		Code:   "OM_PASI",
		Name:   "PASI pension (employee share, OM)",
		Amount: p,
	}}, nil
}
