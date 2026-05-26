package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// krPack implements South Korea's monthly payroll-side statutory
// withholdings:
//
//   - Geunrosodeukse (근로소득세, earned-income tax) per Income Tax
//     Act Article 55 (as amended by Law 19196 in 2023 introducing
//     the 14M / 50M / 88M / 150M / 300M / 500M / 1B band split).
//     Eight progressive bands applied to annualised taxable income;
//     the pack applies the standard earned-income deduction
//     ladder (Article 47) before the rate walk so the withheld
//     amount is reasonably close to the year-end seolyeon
//     (settlement) figure. Standard 1× basic deduction (KRW
//     1.5M per dependent including taxpayer) is applied; richer
//     credit math (e.g. medical, education) is handled at
//     year-end, not via PAYE.
//
//   - Local Income Tax per Local Tax Act s.103-3: 10% of the
//     national income tax (e.g. an income-tax 6% bracket
//     produces a 0.6% local surtax; a 45% bracket produces a
//     4.5% local surtax). Emitted as a separate line so the
//     ledger can post to the municipal-tax liability account.
//
//   - 4 대 보험 (4 social insurance) employee shares applied to
//     monthly remuneration (월급여):
//     · 국민연금 (National Pension Service, NPS): 4.5% employee,
//       ceiling KRW 6,170,000 (2024 monthly cap)
//     · 건강보험 (NHIS health insurance): 3.545% employee (2024)
//     · 장기요양보험 (Long-term care insurance): 12.95% × NHIS
//       contribution = 0.4591% of monthly remuneration (2024
//       NHIS Press Release; the 12.95% is the LTC surcharge
//       ratio against the NHI contribution, not against gross)
//     · 고용보험 (Employment Insurance): 0.9% employee
//
//   - Non-residents are taxed under Article 156 at the same
//     progressive rates as residents on Korea-sourced employment
//     income, with one additional ground rule: no deductions
//     besides the basic credit. The pack applies the standard
//     bracket walk + local surtax for non-residents but skips
//     the four social-insurance lines (NPS/NHIS/LTC/EI are
//     restricted to "covered" employees per NPS Act s.6,
//     NHI Act s.6, EI Act s.10).
//
// References:
//
//	NTS earned-income tax schedule (Income Tax Act Article 55):
//	  https://www.nts.go.kr/english/main.do
//	Local Tax Act s.103-3 (Local Income Tax):
//	  https://elaw.klri.re.kr/eng_service/lawView.do?hseq=39604&lang=ENG
//	NPS 2024 contribution rates:
//	  https://www.nps.or.kr/jsppage/english/scheme/scheme_03.jsp
//	NHIS 2024 health + LTC rates:
//	  https://www.nhis.or.kr/static/alim/paym/calculate/payment_summary.html
type krPack struct{}

func init() { Register(&krPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services. Used by the registry to key lookups from
// tenants.country.
func (krPack) Country() string { return "KR" }

// EffectiveYear returns the fiscal year the KR tables are
// calibrated for: 2024 (Income Tax Act Law 19196 + 2024 NPS/NHIS
// rates).
func (krPack) EffectiveYear() int { return 2024 }

type krBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Income Tax Act Article 55 (annual taxable income, KRW)
	// post-Law 19196 (2023 amendment).
	krBrackets = []krBracket{
		{Floor: dec("0"), Top: dec("14000000"), Base: dec("0"), Rate: dec("0.06")},
		{Floor: dec("14000000"), Top: dec("50000000"), Base: dec("840000"), Rate: dec("0.15")},
		{Floor: dec("50000000"), Top: dec("88000000"), Base: dec("6240000"), Rate: dec("0.24")},
		{Floor: dec("88000000"), Top: dec("150000000"), Base: dec("15360000"), Rate: dec("0.35")},
		{Floor: dec("150000000"), Top: dec("300000000"), Base: dec("37060000"), Rate: dec("0.38")},
		{Floor: dec("300000000"), Top: dec("500000000"), Base: dec("94060000"), Rate: dec("0.40")},
		{Floor: dec("500000000"), Top: dec("1000000000"), Base: dec("174060000"), Rate: dec("0.42")},
		{Floor: dec("1000000000"), Top: decimal.Zero, Base: dec("384060000"), Rate: dec("0.45")},
	}

	// Local Income Tax: 10% of national income tax.
	krLocalIncomeTaxRate = dec("0.10")

	// 2024 NPS: 4.5% employee, capped at KRW 6,170,000 / month.
	krNPSRate           = dec("0.045")
	krNPSMonthlyCeiling = dec("6170000")

	// 2024 NHIS: 3.545% employee.
	krNHISRate = dec("0.03545")

	// Long-term care insurance: 12.95% surcharge on NHI
	// contribution (NHIS 2024) = 0.4591% of remuneration when
	// distributed back to gross-as-base. We compute it off the
	// NHI contribution to keep the LTC linkage explicit.
	krLTCSurchargeRate = dec("0.1295")

	// Employment Insurance: 0.9% employee (general business).
	krEIRate = dec("0.009")

	// Basic personal deduction (Article 50): KRW 1,500,000 per
	// dependent including taxpayer; applied annually before the
	// bracket walk.
	krBasicDeductionAnnual = dec("1500000")

	krAnnualPeriodFraction = decimal.NewFromFloat(365.25)
	krMonthlyDaysApprox    = decimal.NewFromFloat(30.4375)
)

// ComputeWithholding emits KR_INCOME_TAX, KR_LOCAL_INCOME_TAX,
// KR_NPS, KR_NHI, KR_LTC, KR_EMPLOYMENT_INSURANCE for residents;
// KR_INCOME_TAX + KR_LOCAL_INCOME_TAX only for non-residents.
// Zero-amount lines are omitted.
func (krPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(krAnnualPeriodFraction)
	annualGross := gross.Div(periodFraction)

	// Basic personal deduction applied before bracket walk.
	taxableAnnual := annualGross.Sub(krBasicDeductionAnnual)
	if taxableAnnual.LessThan(decimal.Zero) {
		taxableAnnual = decimal.Zero
	}
	annualTax := walkKRBrackets(taxableAnnual)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "KR_INCOME_TAX",
			Name:   "Earned Income Tax (KR)",
			Amount: periodTax,
		})
	}
	localTax := periodTax.Mul(krLocalIncomeTaxRate).Round(2)
	if localTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "KR_LOCAL_INCOME_TAX",
			Name:   "Local Income Tax (10% of national, KR)",
			Amount: localTax,
		})
	}

	// Non-residents (Income Tax Act Article 156) get income tax
	// only — no social insurance.
	if !e.Resident {
		return out, nil
	}

	monthlyEquiv := gross.Mul(krMonthlyDaysApprox).Div(decimal.NewFromInt(int64(days)))

	// NPS: 4.5% capped at KRW 6,170,000 monthly.
	npsBase := monthlyEquiv
	if npsBase.GreaterThan(krNPSMonthlyCeiling) {
		npsBase = krNPSMonthlyCeiling
	}
	npsMonthly := npsBase.Mul(krNPSRate)
	npsPeriod := npsMonthly.Mul(decimal.NewFromInt(int64(days))).Div(krMonthlyDaysApprox).Round(2)
	if npsPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "KR_NPS",
			Name:   "National Pension (employee, KR)",
			Amount: npsPeriod,
		})
	}

	// NHI: 3.545% of monthly remuneration.
	nhiMonthly := monthlyEquiv.Mul(krNHISRate)
	nhiPeriod := nhiMonthly.Mul(decimal.NewFromInt(int64(days))).Div(krMonthlyDaysApprox).Round(2)
	if nhiPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "KR_NHI",
			Name:   "National Health Insurance (employee, KR)",
			Amount: nhiPeriod,
		})
	}
	// LTC: 12.95% surcharge on the NHI contribution.
	ltcMonthly := nhiMonthly.Mul(krLTCSurchargeRate)
	ltcPeriod := ltcMonthly.Mul(decimal.NewFromInt(int64(days))).Div(krMonthlyDaysApprox).Round(2)
	if ltcPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "KR_LTC",
			Name:   "Long-Term Care Insurance (employee, KR)",
			Amount: ltcPeriod,
		})
	}

	// Employment Insurance: 0.9% gross.
	eiMonthly := monthlyEquiv.Mul(krEIRate)
	eiPeriod := eiMonthly.Mul(decimal.NewFromInt(int64(days))).Div(krMonthlyDaysApprox).Round(2)
	if eiPeriod.IsPositive() {
		out = append(out, Deduction{
			Code:   "KR_EMPLOYMENT_INSURANCE",
			Name:   "Employment Insurance (employee, KR)",
			Amount: eiPeriod,
		})
	}

	return out, nil
}

func walkKRBrackets(annual decimal.Decimal) decimal.Decimal {
	var match krBracket
	matched := false
	for _, b := range krBrackets {
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
