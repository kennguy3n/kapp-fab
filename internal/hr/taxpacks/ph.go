package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// phPack implements the Philippines's monthly payroll-side
// statutory withholdings:
//
//   - Withholding tax: Bureau of Internal Revenue (BIR) Revenue
//     Memorandum Order No. 23-2023 schedule, derived from the
//     TRAIN law (RA 10963) second-tranche brackets effective
//     1 January 2023 onwards. The pack annualises monthly gross
//     and walks the 6-bracket progressive schedule
//     (0/15/20/25/30/35%) for residents (citizens, resident
//     aliens, and non-resident aliens engaged in trade or
//     business — NRAETB). The 250,000 PHP annual exempt
//     threshold is built into the first bracket (Floor = 0,
//     Top = 250,000, Rate = 0).
//
//     Non-resident aliens *not* engaged in trade or business
//     (NRANETB, NIRC s.25(B)) are taxed at a flat 25% on PH-
//     sourced gross income with no exempt threshold and no
//     SSS/PhilHealth/Pag-IBIG eligibility. The pack branches on
//     EmployeeInfo.Resident: true → progressive schedule + the
//     three social contributions; false → flat 25% only. This
//     matches BIR Form 1604-CF and CAS s.25 / s.57(A).
//
//   - SSS employee contribution per the 2025 SSS Contribution
//     Schedule (Social Security Act of 2018 + EO 717 c. 2023):
//     5% on monthly compensation capped at the PHP 30,000 MSC
//     ceiling → max EE PHP 1,500 / month. Simplified from the
//     bracketed published schedule; PR review confirms.
//
//   - PhilHealth (RA 11223 + 2025 contribution circular):
//     5% total premium / 2.5% EE share, floor PHP 10,000 / ceiling
//     PHP 100,000 monthly compensation → EE PHP 250-2,500.
//
//   - Pag-IBIG / HDMF (RA 9679 + 2024 HDMF Circular 460):
//     2% EE on monthly compensation up to PHP 10,000 ceiling →
//     max EE PHP 200 / month (post-2024 HDMF rate hike).
//
// References:
//
//	BIR RMO 23-2023 (withholding tax tables):
//	  https://www.bir.gov.ph/index.php/2023-rrmo/12-rmo-2023
//	SSS contribution schedule:
//	  https://www.sss.gov.ph/contribution-schedule
//	PhilHealth 2025 circular:
//	  https://www.philhealth.gov.ph/circulars/
//	HDMF Circular 460 (Pag-IBIG):
//	  https://www.pagibigfund.gov.ph/circulars.html
type phPack struct{}

func init() { Register(&phPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (phPack) Country() string { return "PH" }

// EffectiveYear returns the fiscal year the PH tables are
// calibrated for: 2024 (BIR TRAIN tranche 2 + 2024 HDMF /
// PhilHealth rates). The SSS 2025 schedule is consistent with the
// 2024 figures the pack uses.
func (phPack) EffectiveYear() int { return 2024 }

type phBracket struct {
	Floor decimal.Decimal // annual taxable income, PHP
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// BIR RMO 23-2023 / RA 10963 second-tranche schedule.
	phBracketsResident = []phBracket{
		{Floor: dec("0"), Top: dec("250000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("250000"), Top: dec("400000"), Base: dec("0"), Rate: dec("0.15")},
		{Floor: dec("400000"), Top: dec("800000"), Base: dec("22500"), Rate: dec("0.20")},
		{Floor: dec("800000"), Top: dec("2000000"), Base: dec("102500"), Rate: dec("0.25")},
		{Floor: dec("2000000"), Top: dec("8000000"), Base: dec("402500"), Rate: dec("0.30")},
		{Floor: dec("8000000"), Top: decimal.Zero, Base: dec("2202500"), Rate: dec("0.35")},
	}

	phSSSEmployeeRate = dec("0.05")
	phSSSCeiling      = dec("30000") // monthly MSC ceiling

	phPhilHealthRate    = dec("0.025") // 2.5% EE share of 5% total premium
	phPhilHealthFloor   = dec("10000")
	phPhilHealthCeiling = dec("100000")

	phPagIBIGRate    = dec("0.02") // 2% EE post-2024
	phPagIBIGCeiling = dec("10000")

	// NIRC s.25(B): flat 25% on PH-sourced gross income for non-
	// resident aliens not engaged in trade or business. No
	// brackets, no exempt threshold, no social contributions.
	phNRANETBRate = dec("0.25")

	phPeriodsPerYear = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits PH_WITHHOLDING_TAX (BIR progressive
// schedule), PH_SSS, PH_PHILHEALTH, PH_PAGIBIG for residents and
// resident aliens / NRAETB. Non-resident aliens not engaged in
// trade or business (NRANETB) get PH_NRANETB_TAX at the flat
// 25% rate per NIRC s.25(B) and no social contributions.
// Zero-amount lines are omitted.
func (phPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Non-resident alien not engaged in trade or business: flat
	// 25% on PH-sourced gross income, no social contributions.
	if !e.Resident {
		nranetbTax := gross.Mul(phNRANETBRate).Round(2)
		if nranetbTax.IsPositive() {
			out = append(out, Deduction{
				Code:   "PH_NRANETB_TAX",
				Name:   "NRANETB flat withholding 25% (PH)",
				Amount: nranetbTax,
			})
		}
		return out, nil
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(phPeriodsPerYear)
	annualGross := gross.Div(periodFraction)

	annualTax := walkPHBrackets(annualGross, phBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "PH_WITHHOLDING_TAX",
			Name:   "BIR withholding tax (PH)",
			Amount: periodTax,
		})
	}

	// SSS: 5% of monthly compensation, capped at MSC ceiling.
	sssBase := gross
	if sssBase.GreaterThan(phSSSCeiling) {
		sssBase = phSSSCeiling
	}
	if sss := sssBase.Mul(phSSSEmployeeRate).Round(2); sss.IsPositive() {
		out = append(out, Deduction{
			Code:   "PH_SSS",
			Name:   "SSS (employee share, PH)",
			Amount: sss,
		})
	}

	// PhilHealth: 2.5% on monthly compensation in the floor / ceiling band.
	phBase := gross
	if phBase.LessThan(phPhilHealthFloor) {
		phBase = phPhilHealthFloor
	}
	if phBase.GreaterThan(phPhilHealthCeiling) {
		phBase = phPhilHealthCeiling
	}
	if ph := phBase.Mul(phPhilHealthRate).Round(2); ph.IsPositive() {
		out = append(out, Deduction{
			Code:   "PH_PHILHEALTH",
			Name:   "PhilHealth (employee share, PH)",
			Amount: ph,
		})
	}

	// Pag-IBIG: 2% capped at PHP 10,000 monthly comp → max PHP 200.
	pagBase := gross
	if pagBase.GreaterThan(phPagIBIGCeiling) {
		pagBase = phPagIBIGCeiling
	}
	if pag := pagBase.Mul(phPagIBIGRate).Round(2); pag.IsPositive() {
		out = append(out, Deduction{
			Code:   "PH_PAGIBIG",
			Name:   "Pag-IBIG / HDMF (employee share, PH)",
			Amount: pag,
		})
	}

	return out, nil
}

func walkPHBrackets(annual decimal.Decimal, scale []phBracket) decimal.Decimal {
	var match phBracket
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
