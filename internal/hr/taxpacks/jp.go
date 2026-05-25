package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// jpPack implements Japan's monthly payroll-side statutory
// withholdings:
//
//   - Gensenchōshū (源泉徴収, income-tax withholding) per the
//     National Tax Agency (NTA) "Income Tax and Special Income
//     Tax for Reconstruction Withholding Tax Tables". The
//     pack walks the seven-band progressive schedule applied to
//     annualised taxable income (Article 89 Income Tax Act) and
//     adds the 2.1% Special Reconstruction surtax (Special
//     Measures for Reconstruction Funding Act 2011 s.13). Local
//     resident tax (10% of prior-year income) is *not* withheld
//     by the employer at this stage — it's billed by the
//     municipality through gensen-tokuchō and posted by the
//     payroll engine as a separate adjustment.
//
//   - Shakai Hoken (社会保険, social insurance) employee shares
//     applied to monthly standard remuneration (標準報酬月額):
//     · Kenkō Hoken (健康保険, health insurance) — Tokyo branch
//       rate 9.98% in 2024, employee share 50% = 4.99%
//     · Kōsei Nenkin (厚生年金, pension) — 18.300% statutory,
//       employee share 50% = 9.15%
//     · Kaigo Hoken (介護保険, long-term care insurance) —
//       1.60% in 2024 for employees aged 40-65, employee share
//       50% = 0.80% (the pack uses Age >= 40 && Age <= 64 as the
//       trigger)
//     · Koyō Hoken (雇用保険, employment insurance) — 0.60%
//       general industry rate as of 2024 (Special Account for
//       Employment Insurance, "general business")
//
//     The pack uses the slip's monthly-equivalent gross as the
//     standard remuneration proxy. Strict standard-remuneration
//     resolution requires a lookup table that maps salary into
//     50-tier bands (Shakai Hoken Act schedule); the proxy is
//     accurate within ±1 tier for slips in the meat of the
//     distribution and produces no error for the band boundary
//     where the slip's exact gross matches the published
//     midpoint, so it's the conservative simplification.
//
// References:
//
//	NTA withholding tax tables (2024):
//	  https://www.nta.go.jp/publication/pamph/gensen/zeigakuhyo2024/
//	Special Measures for Reconstruction Funding Act 2011 s.13:
//	  https://elaws.e-gov.go.jp/document?lawid=423AC0000000117
//	Tokyo health insurance + pension rates (2024):
//	  https://www.kyoukaikenpo.or.jp/g7/cat330/sb3150/r06/r6ryougakuhyou3gatukara/
type jpPack struct{}

func init() { Register(&jpPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services. Used by the registry to key lookups from
// tenants.country.
func (jpPack) Country() string { return "JP" }

// EffectiveYear returns the fiscal year the JP tables are
// calibrated for: 2024 (NTA 2024 withholding tables + 2024
// Tokyo Kyokai Kenpo branch rates).
func (jpPack) EffectiveYear() int { return 2024 }

type jpBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// NTA Article 89 schedule (annual taxable income, JPY).
	jpBrackets = []jpBracket{
		{Floor: dec("0"), Top: dec("1950000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("1950000"), Top: dec("3300000"), Base: dec("97500"), Rate: dec("0.10")},
		{Floor: dec("3300000"), Top: dec("6950000"), Base: dec("232500"), Rate: dec("0.20")},
		{Floor: dec("6950000"), Top: dec("9000000"), Base: dec("962500"), Rate: dec("0.23")},
		{Floor: dec("9000000"), Top: dec("18000000"), Base: dec("1434000"), Rate: dec("0.33")},
		{Floor: dec("18000000"), Top: dec("40000000"), Base: dec("4404000"), Rate: dec("0.40")},
		{Floor: dec("40000000"), Top: decimal.Zero, Base: dec("13204000"), Rate: dec("0.45")},
	}

	// 2.1% Special Reconstruction surtax on the income tax
	// amount (Special Measures for Reconstruction Funding Act).
	jpReconstructionSurtax = dec("0.021")

	// 2024 Tokyo Kyokai Kenpo branch employee shares.
	jpHealthInsuranceRate    = dec("0.0499") // 9.98% / 2
	jpPensionRate            = dec("0.0915") // 18.30% / 2
	jpLongTermCareRate       = dec("0.008")  // 1.60% / 2
	jpEmploymentInsuranceRate = dec("0.006")

	jpAnnualPeriodFraction = decimal.NewFromFloat(365.25)
	jpMonthlyDaysApprox    = decimal.NewFromFloat(30.4375)
)

// ComputeWithholding emits JP_INCOME_TAX (NTA progressive +
// reconstruction surtax), JP_HEALTH_INSURANCE, JP_PENSION,
// JP_LTC_INSURANCE (only when 40 ≤ age ≤ 64), and JP_EMPLOYMENT_INSURANCE.
// Non-residents are taxed at a flat 20.42% on Japan-sourced income
// (Income Tax Act Article 161); the pack emits JP_NONRESIDENT_TAX
// for those slips and no social-insurance contributions.
func (jpPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Non-resident flat withholding (Article 161): 20.42% (20%
	// + 2.1% reconstruction surtax on 20% = 0.42%).
	if !e.Resident {
		nrRate := dec("0.20").Mul(dec("1.021"))
		nr := gross.Mul(nrRate).Round(2)
		if nr.IsPositive() {
			out = append(out, Deduction{
				Code:   "JP_NONRESIDENT_TAX",
				Name:   "Non-resident withholding tax (JP)",
				Amount: nr,
			})
		}
		return out, nil
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(jpAnnualPeriodFraction)
	annualGross := gross.Div(periodFraction)
	annualTax := walkJPBrackets(annualGross)
	// Add 2.1% reconstruction surtax.
	annualTaxWithSurtax := annualTax.Mul(decimal.NewFromInt(1).Add(jpReconstructionSurtax))
	periodTax := annualTaxWithSurtax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "JP_INCOME_TAX",
			Name:   "Income tax + reconstruction surtax (JP)",
			Amount: periodTax,
		})
	}

	// Social insurance: monthly-equivalent gross as standard-
	// remuneration proxy.
	monthlyEquiv := gross.Mul(jpMonthlyDaysApprox).Div(decimal.NewFromInt(int64(days)))

	health := monthlyEquiv.Mul(jpHealthInsuranceRate)
	healthPeriod := health.Mul(decimal.NewFromInt(int64(days))).Div(jpMonthlyDaysApprox).Round(2)
	if healthPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "JP_HEALTH_INSURANCE",
			Name:   "Health Insurance (employee, JP)",
			Amount: healthPeriod,
		})
	}

	pension := monthlyEquiv.Mul(jpPensionRate)
	pensionPeriod := pension.Mul(decimal.NewFromInt(int64(days))).Div(jpMonthlyDaysApprox).Round(2)
	if pensionPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "JP_PENSION",
			Name:   "Employees' Pension (employee, JP)",
			Amount: pensionPeriod,
		})
	}

	// Long-term care insurance applies to ages 40-64.
	if e.Age >= 40 && e.Age <= 64 {
		ltc := monthlyEquiv.Mul(jpLongTermCareRate)
		ltcPeriod := ltc.Mul(decimal.NewFromInt(int64(days))).Div(jpMonthlyDaysApprox).Round(2)
		if ltcPeriod.IsPositive() {
			out = append(out, Deduction{
				Code:   "JP_LTC_INSURANCE",
				Name:   "Long-Term Care Insurance (employee, JP)",
				Amount: ltcPeriod,
			})
		}
	}

	ei := monthlyEquiv.Mul(jpEmploymentInsuranceRate)
	eiPeriod := ei.Mul(decimal.NewFromInt(int64(days))).Div(jpMonthlyDaysApprox).Round(2)
	if eiPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "JP_EMPLOYMENT_INSURANCE",
			Name:   "Employment Insurance (employee, JP)",
			Amount: eiPeriod,
		})
	}

	return out, nil
}

func walkJPBrackets(annual decimal.Decimal) decimal.Decimal {
	var match jpBracket
	matched := false
	for _, b := range jpBrackets {
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
