package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// tnPack implements Tunisia's payroll-side statutory withholdings:
//
//   - Personal income tax (Impôt sur le Revenu des Personnes
//     Physiques, IRPP) on salary per the Code de l'IRPP et de l'IS,
//     using the eight-band schedule introduced by the Finance Law
//     2025 (Loi de Finances 2025). The pack annualises the slip's
//     gross (days / 365.25), applies the 10% professional-expenses
//     deduction (capped at TND 2,000/year), walks the progressive
//     schedule (TND) and prorates the annual tax back to the slip
//     period.
//
//   - CNSS (Caisse Nationale de Sécurité Sociale) employee
//     contribution for the régime salarié (RSNA): 9.18% of gross
//     salary, no ceiling.
//
// References:
//
//	Code de l'IRPP et de l'IS + Loi de Finances 2025 (barème):
//	  http://www.finances.gov.tn/
//	Direction Générale des Impôts:
//	  http://www.impots.finances.gov.tn/
//	CNSS contribution rates (RSNA 9.18% employee):
//	  https://www.cnss.tn/
type tnPack struct{}

func init() { Register(&tnPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (tnPack) Country() string { return "TN" }

// EffectiveYear returns the calendar year the TN schedule is
// calibrated for: 2025 (Loi de Finances 2025 eight-band IRPP
// barème; CNSS RSNA 9.18% employee rate).
func (tnPack) EffectiveYear() int { return 2025 }

type tnBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Loi de Finances 2025 IRPP progressive schedule (annual net
	// taxable income, TND). Base is cumulative tax at each Floor.
	tnBracketsResident = []tnBracket{
		{Floor: dec("0"), Top: dec("5000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("5000"), Top: dec("10000"), Base: dec("0"), Rate: dec("0.15")},
		{Floor: dec("10000"), Top: dec("20000"), Base: dec("750"), Rate: dec("0.25")},
		{Floor: dec("20000"), Top: dec("30000"), Base: dec("3250"), Rate: dec("0.30")},
		{Floor: dec("30000"), Top: dec("40000"), Base: dec("6250"), Rate: dec("0.33")},
		{Floor: dec("40000"), Top: dec("50000"), Base: dec("9550"), Rate: dec("0.36")},
		{Floor: dec("50000"), Top: dec("70000"), Base: dec("13150"), Rate: dec("0.38")},
		{Floor: dec("70000"), Top: decimal.Zero, Base: dec("20750"), Rate: dec("0.40")},
	}

	// Standard professional-expenses deduction: 10% of gross,
	// capped at TND 2,000/year.
	tnProfessionalDeductionRate = dec("0.10")
	tnProfessionalDeductionCap  = dec("2000")

	// CNSS employee contribution (régime salarié RSNA): 9.18%.
	tnCNSSEmployeeRate = dec("0.0918")

	tnAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits TN_IRPP (annualised progressive income
// tax, net of the professional deduction) and TN_CNSS (social-
// security employee contribution).
func (tnPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// CNSS is itself deductible from the IRPP base; compute it
	// first so the income-tax base is net of social security. It runs
	// on the contribution base (full gross by default).
	cnss := e.ContributionBase(gross).Mul(tnCNSSEmployeeRate).Round(2)

	periodFraction := decimal.NewFromInt(int64(days)).Div(tnAnnualPeriodFraction)
	// IRPP runs on the post-pre-tax base less CNSS; CNSS keeps full gross.
	annualGross := e.IncomeTaxBase(gross).Div(periodFraction)
	annualCNSS := cnss.Div(periodFraction)

	profDeduction := annualGross.Mul(tnProfessionalDeductionRate)
	if profDeduction.GreaterThan(tnProfessionalDeductionCap) {
		profDeduction = tnProfessionalDeductionCap
	}
	taxable := annualGross.Sub(annualCNSS).Sub(profDeduction)
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	annualTax := walkTNBrackets(taxable, tnBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "TN_IRPP",
			Name:   "Income tax IRPP (TN)",
			Amount: periodTax,
		})
	}

	if cnss.IsPositive() {
		out = append(out, Deduction{
			Code:   "TN_CNSS",
			Name:   "CNSS social security (employee, TN)",
			Amount: cnss,
		})
	}

	return out, nil
}

func walkTNBrackets(annual decimal.Decimal, scale []tnBracket) decimal.Decimal {
	var match tnBracket
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
