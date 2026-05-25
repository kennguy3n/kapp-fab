package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// ngPack implements Nigeria's monthly payroll-side statutory
// withholdings:
//
//   - PAYE (Personal Income Tax) per the Personal Income Tax Act
//     (CAP P8 LFN 2004, as amended by the Finance Act 2020). Six
//     progressive bands applied to annualised taxable income
//     (Sixth Schedule). The taxable base is gross minus
//     consolidated relief allowance (the higher of ₦200,000 or
//     1% of gross + 20% of gross) and statutory pension / NHF
//     contributions, per s.33. This pack uses the most-common
//     SME formulation: deduct pension (8%) and NHF (2.5%) from
//     gross before annualising and walking the bands.
//
//   - Pension Reform Act 2014 s.4(1): mandatory 8% employee
//     contribution to a Retirement Savings Account, computed on
//     the sum of basic salary + housing allowance + transport
//     allowance. Slips that don't break the components out use
//     the gross as the contribution base — that's an over-
//     contribution in the legally-conservative direction
//     (refunded on reconciliation) and is the safer default.
//
//   - National Housing Fund Act 1992 s.4: 2.5% employee
//     contribution on monthly basic salary. The Act mandates
//     monthly remittance via the Federal Mortgage Bank of
//     Nigeria. Same gross-as-base simplification applies.
//
// References:
//
//	Personal Income Tax Act (CAP P8 LFN 2004) + Finance Act 2020:
//	  https://www.firs.gov.ng/personal-income-tax/
//	Pension Reform Act 2014:
//	  https://www.pencom.gov.ng/legislation/
//	National Housing Fund Act 1992:
//	  https://fmbn.gov.ng/national-housing-fund/
type ngPack struct{}

func init() { Register(&ngPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services. Used by the registry to key lookups from
// tenants.country.
func (ngPack) Country() string { return "NG" }

// EffectiveYear returns the fiscal year the NG tables are
// calibrated for: 2024 (PITA Sixth Schedule as last updated by
// the Finance Act 2020).
func (ngPack) EffectiveYear() int { return 2024 }

type ngBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// PITA Sixth Schedule (annual taxable income, NGN). Bands
	// are cumulative: first ₦300k @ 7%, next ₦300k @ 11%,
	// next ₦500k @ 15%, next ₦500k @ 19%, next ₦1.6M @ 21%,
	// above ₦3.2M @ 24%.
	ngBrackets = []ngBracket{
		{Floor: dec("0"), Top: dec("300000"), Base: dec("0"), Rate: dec("0.07")},
		{Floor: dec("300000"), Top: dec("600000"), Base: dec("21000"), Rate: dec("0.11")},
		{Floor: dec("600000"), Top: dec("1100000"), Base: dec("54000"), Rate: dec("0.15")},
		{Floor: dec("1100000"), Top: dec("1600000"), Base: dec("129000"), Rate: dec("0.19")},
		{Floor: dec("1600000"), Top: dec("3200000"), Base: dec("224000"), Rate: dec("0.21")},
		{Floor: dec("3200000"), Top: decimal.Zero, Base: dec("560000"), Rate: dec("0.24")},
	}

	// Consolidated relief allowance per PITA s.33(2): higher of
	// ₦200,000 OR 1% of gross + 20% of gross. Applied annually.
	ngCRAFloor      = dec("200000")
	ngCRAFixedShare = dec("0.01") // 1% of gross
	ngCRARate       = dec("0.20") // 20% of gross

	// Pension Reform Act 2014 s.4(1): 8% employee share.
	ngPensionRate = dec("0.08")

	// National Housing Fund Act 1992 s.4: 2.5% on basic salary.
	ngNHFRate = dec("0.025")

	ngAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits NG_PENSION, NG_NHF, and NG_PAYE.
// Pension and NHF are deducted from the gross before the PAYE
// taxable base is computed, per PITA s.33 (statutory deductions
// are allowable in addition to the consolidated relief allowance).
// Zero-amount lines are omitted.
func (ngPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Pension first (employee share, 8% of gross).
	pension := gross.Mul(ngPensionRate).Round(2)
	if pension.IsPositive() {
		out = append(out, Deduction{
			Code:   "NG_PENSION",
			Name:   "RSA pension (employee, 8%)",
			Amount: pension,
		})
	}
	// NHF: 2.5% of monthly basic. Using gross as base is a
	// documented over-contribution; reconciliation via FMBN.
	nhf := gross.Mul(ngNHFRate).Round(2)
	if nhf.IsPositive() {
		out = append(out, Deduction{
			Code:   "NG_NHF",
			Name:   "National Housing Fund (employee, 2.5%)",
			Amount: nhf,
		})
	}

	// PAYE: gross minus pension minus NHF, then annualise, apply
	// consolidated relief allowance, walk the brackets.
	periodFraction := decimal.NewFromInt(int64(days)).Div(ngAnnualPeriodFraction)
	netForPAYE := gross.Sub(pension).Sub(nhf)
	annualGross := netForPAYE.Div(periodFraction)

	// CRA: higher of ₦200k or 1%+20% = 21% of gross.
	craShare := annualGross.Mul(ngCRAFixedShare.Add(ngCRARate))
	cra := craShare
	if cra.LessThan(ngCRAFloor) {
		cra = ngCRAFloor
	}
	taxableAnnual := annualGross.Sub(cra)
	if taxableAnnual.LessThan(decimal.Zero) {
		taxableAnnual = decimal.Zero
	}

	annualTax := walkNGBrackets(taxableAnnual)
	periodPAYE := annualTax.Mul(periodFraction).Round(2)
	if periodPAYE.IsPositive() {
		out = append(out, Deduction{
			Code:   "NG_PAYE",
			Name:   "PAYE (PITA, NG)",
			Amount: periodPAYE,
		})
	}

	return out, nil
}

func walkNGBrackets(annual decimal.Decimal) decimal.Decimal {
	var match ngBracket
	matched := false
	for _, b := range ngBrackets {
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
