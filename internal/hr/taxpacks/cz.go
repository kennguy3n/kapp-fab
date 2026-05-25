package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// czPack implements Czech Republic's payroll-side statutory
// withholdings for the 2025 fiscal year:
//
//   - Daň z příjmů fyzických osob (personal income tax): two
//     brackets in 2025. 15% on annual taxable income up to
//     CZK 1,582,812 (48 × průměrná mzda 32,975); 23% on the
//     excess. The pack applies the standard sleva na poplatníka
//     (taxpayer credit) of CZK 30,840 / yr against the gross
//     liability before producing the deduction line.
//
//   - Sociální pojištění (social insurance, employee share):
//     6.5% flat on gross. 2025 annual cap = 48 × průměrná
//     mzda = CZK 1,582,812 (same as the income-tax bracket
//     cutoff); the pack enforces the cap via YTD-aware
//     accumulation against EmployeeInfo.YTDGross.
//
//   - Zdravotní pojištění (health insurance, employee share):
//     4.5% flat on gross. No cap.
//
// References:
//
//	Finanční správa — daň z příjmů 2025:
//	  https://www.financnisprava.cz/cs/dane/dane/dan-z-prijmu/dan-z-prijmu-fyzickych-osob
//	ČSSZ — sociální pojištění 2025:
//	  https://www.cssz.cz/sazby-pojistneho
//	Všeobecná zdravotní pojišťovna — zdravotní pojištění 2025:
//	  https://www.vzp.cz/platci/pojistne
type czPack struct{}

func init() { Register(&czPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (czPack) Country() string { return "CZ" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (Finanční správa + ČSSZ + VZP 2025).
func (czPack) EffectiveYear() int { return 2025 }

var (
	czPITLowRate         = dec("0.15")
	czPITHighRate        = dec("0.23")
	czPITBracketCutoff   = dec("1582812")
	czSlevaNaPoplatnika  = dec("30840") // annual taxpayer credit

	czSocialRate    = dec("0.065")
	czSocialAnnCap  = dec("1582812")

	czHealthRate = dec("0.045")

	czAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to three lines:
//
//   - CZ_SP   (social insurance employee share, capped via YTD)
//   - CZ_ZP   (health insurance, no cap)
//   - CZ_PIT  (income tax 15/23% with annual taxpayer credit)
//
// Negative or zero gross returns nil.
func (czPack) ComputeWithholding(_ context.Context, employee EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(czAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}

	out := []Deduction{}

	// Sociální pojištění — 6.5%, capped via YTD.
	socialBase := gross
	if employee.YTDGross.GreaterThanOrEqual(czSocialAnnCap) {
		socialBase = decimal.Zero
	} else if employee.YTDGross.Add(gross).GreaterThan(czSocialAnnCap) {
		socialBase = czSocialAnnCap.Sub(employee.YTDGross)
	}
	social := socialBase.Mul(czSocialRate).Round(2)
	if social.IsPositive() {
		out = append(out, Deduction{
			Code:   "CZ_SP",
			Name:   "Sociální pojištění (CZ)",
			Amount: social,
		})
	}

	// Zdravotní pojištění — 4.5%, no cap.
	health := gross.Mul(czHealthRate).Round(2)
	if health.IsPositive() {
		out = append(out, Deduction{
			Code:   "CZ_ZP",
			Name:   "Zdravotní pojištění (CZ)",
			Amount: health,
		})
	}

	// PIT — 15/23% with annual taxpayer credit.
	annualGross := gross.Div(periodFraction)
	var annualPIT decimal.Decimal
	if annualGross.LessThanOrEqual(czPITBracketCutoff) {
		annualPIT = annualGross.Mul(czPITLowRate)
	} else {
		annualPIT = czPITBracketCutoff.Mul(czPITLowRate).Add(
			annualGross.Sub(czPITBracketCutoff).Mul(czPITHighRate),
		)
	}
	annualPIT = annualPIT.Sub(czSlevaNaPoplatnika)
	if annualPIT.LessThan(decimal.Zero) {
		annualPIT = decimal.Zero
	}
	periodPIT := annualPIT.Mul(periodFraction).Round(2)
	if periodPIT.IsPositive() {
		out = append(out, Deduction{
			Code:   "CZ_PIT",
			Name:   "Daň z příjmů (CZ)",
			Amount: periodPIT,
		})
	}

	return out, nil
}
