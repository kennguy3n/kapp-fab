package taxpacks

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// cnPack implements China's payroll-side statutory withholdings for
// the 2025 fiscal year. China taxes employment income through the
// 累计预扣预缴 (cumulative withholding) method, which makes it
// materially different from the annualise-one-period packs in the
// rest of the roster: the withholding for a slip is the cumulative
// tax due on year-to-date taxable income minus the cumulative tax
// already withheld in the prior months of the same calendar year.
// This pack therefore reads EmployeeInfo.YTDGross (the year-to-date
// gross *before* this slip) and the pay-period end month rather than
// prorating an annual figure by days/365.25.
//
// Components emitted per slip:
//
//   - CN_SOCIAL_INSURANCE — the employee share of the mandatory
//     social-insurance contributions deducted monthly on gross:
//     养老保险 (pension)         8%
//     医疗保险 (medical)         2%   + a flat 大额医疗 critical-
//     illness contribution (¥3/mo,
//     the Beijing reference value)
//     失业保险 (unemployment)    0.5%
//     Maternity (生育) and work-injury (工伤) insurance are
//     employer-only and produce no employee deduction.
//
//   - CN_HOUSING_FUND — 住房公积金, the employee share of the
//     housing provident fund. The statutory band is 5%–12% and the
//     rate is set per city, so the pack reads EmployeeInfo.Canton
//     (re-used as the city key) and overrides the default 12% for
//     the four first-tier cities wired in below; any other / empty
//     city falls back to 12% (the common upper-band default).
//
//   - CN_IIT — 个人所得税 (individual income tax) computed with the
//     annual cumulative withholding method. The cumulative taxable
//     income base is:
//
//     cumulative gross
//     − cumulative standard deduction (¥5,000 / month)
//     − cumulative employee statutory contributions
//     (social insurance + housing fund + flat medical)
//
//     walked through the seven-band annual cumulative rate table
//     (with the published quick-deduction constants) to get the
//     cumulative tax due; the slip's withholding is that minus the
//     cumulative tax due through the prior month.
//
// Scope / simplifications (documented so a future maintainer does
// not "fix" them blindly):
//
//   - The social-insurance and housing-fund contribution base is
//     legally capped at 300% of the local average monthly wage (and
//     floored at 60%). Those city-specific ceilings are NOT modelled
//     — contributions are computed on the full gross, which slightly
//     over-withholds for very high earners (conservative for the
//     ledger; the year-end reconciliation settles the difference).
//   - Special additional deductions (子女教育, 住房贷款利息,
//     赡养老人, …) reduce the IIT base but are claimed by the
//     employee and reconciled at year-end (汇算清缴); they are out of
//     scope for the baseline withholding pack, so omitting them
//     over-withholds rather than under-withholds.
//   - The cumulative method assumes monthly payroll (the norm for
//     Chinese IIT) and accrues the ¥5,000 standard deduction once per
//     month. The cumulative month index is EmployeeInfo.MonthsEmployedYTD
//     (the count of months the employee has received income this year,
//     including the slip's own month) so mid-year starters are handled
//     correctly; when that field is unset (0) the pack falls back to
//     the pay-period end month, which is correct for a full-year
//     employee. The index is clamped to 12 (a tax year has at most
//     twelve months) so a bad input cannot over-credit the standard
//     deduction and under-withhold.
//
// References:
//
//	国家税务总局 — 个人所得税预扣预缴方法 (cumulative withholding):
//	  https://www.chinatax.gov.cn/
//	个人所得税预扣率表一 (resident wage cumulative rate table):
//	  https://www.gov.cn/zhengce/
//	社会保险 / 住房公积金 缴存比例:
//	  http://www.mohrss.gov.cn/
type cnPack struct{}

func init() { Register(&cnPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (cnPack) Country() string { return "CN" }

// EffectiveYear is the calendar year the rates and brackets in this
// pack are sourced from (STA cumulative-withholding rate table +
// social-insurance / housing-fund contribution ratios, 2025).
func (cnPack) EffectiveYear() int { return 2025 }

// cnIITBracket is one band of the annual cumulative withholding rate
// table (预扣率表一). QuickDeduction is the published 速算扣除数 that
// turns the marginal-band walk into a single multiply-subtract.
type cnIITBracket struct {
	Floor          decimal.Decimal
	Top            decimal.Decimal // 0 = open-ended top band
	Rate           decimal.Decimal
	QuickDeduction decimal.Decimal
}

var (
	cnPensionRate      = dec("0.08")  // 养老保险 (employee)
	cnMedicalRate      = dec("0.02")  // 医疗保险 (employee)
	cnUnemploymentRate = dec("0.005") // 失业保险 (employee)
	// Flat 大额医疗互助 (critical-illness) contribution. Cities
	// vary; ¥3/month is the Beijing reference value used here as a
	// representative flat add-on to the percentage medical rate.
	cnMedicalFixedMonthly = dec("3")

	// Monthly standard deduction (减除费用): ¥5,000 / month
	// (¥60,000 / year), accrued cumulatively across the year.
	cnMonthlyStandardDeduction = dec("5000")

	// Housing provident fund employee rate. The statutory band is
	// 5%–12%; the default is the 12% upper band and the four
	// first-tier cities override it with a representative rate
	// inside the band. Keyed by uppercased city name (Canton).
	cnHousingFundDefaultRate = dec("0.12")
	cnHousingFundRates       = map[string]decimal.Decimal{
		"BEIJING":   dec("0.12"),
		"SHANGHAI":  dec("0.07"),
		"GUANGZHOU": dec("0.05"),
		"SHENZHEN":  dec("0.05"),
	}

	// 预扣率表一 — annual cumulative withholding rate table for
	// resident wage income. Bands are on cumulative taxable income
	// (CNY/year); the quick-deduction constants are the published
	// 速算扣除数.
	cnIITBrackets = []cnIITBracket{
		{Floor: dec("0"), Top: dec("36000"), Rate: dec("0.03"), QuickDeduction: dec("0")},
		{Floor: dec("36000"), Top: dec("144000"), Rate: dec("0.10"), QuickDeduction: dec("2520")},
		{Floor: dec("144000"), Top: dec("300000"), Rate: dec("0.20"), QuickDeduction: dec("16920")},
		{Floor: dec("300000"), Top: dec("420000"), Rate: dec("0.25"), QuickDeduction: dec("31920")},
		{Floor: dec("420000"), Top: dec("660000"), Rate: dec("0.30"), QuickDeduction: dec("52920")},
		{Floor: dec("660000"), Top: dec("960000"), Rate: dec("0.35"), QuickDeduction: dec("85920")},
		{Floor: dec("960000"), Top: dec("0"), Rate: dec("0.45"), QuickDeduction: dec("181920")},
	}
)

// cnHousingFundRate resolves the employee housing-fund rate for the
// employee's city (Canton), falling back to the 12% default for an
// empty or unknown city.
func cnHousingFundRate(city string) decimal.Decimal {
	if r, ok := cnHousingFundRates[strings.ToUpper(strings.TrimSpace(city))]; ok {
		return r
	}
	return cnHousingFundDefaultRate
}

// cnCumulativeIIT returns the cumulative IIT due on the given
// cumulative taxable income via the 预扣率表一 quick-deduction walk.
// Non-positive taxable income yields zero.
func cnCumulativeIIT(taxable decimal.Decimal) decimal.Decimal {
	if taxable.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	for _, b := range cnIITBrackets {
		if b.Top.IsZero() || taxable.LessThanOrEqual(b.Top) {
			tax := taxable.Mul(b.Rate).Sub(b.QuickDeduction)
			if tax.IsNegative() {
				return decimal.Zero
			}
			return tax
		}
	}
	return decimal.Zero // unreachable: the last band is open-ended
}

// cnCumulativeTaxable returns the year-to-date taxable income base at
// the given cumulative month count: cumulative gross minus the
// cumulative ¥5,000/month standard deduction minus the cumulative
// employee statutory contributions (SI + housing fund at
// deductibleRate, plus the flat medical add-on per month).
func cnCumulativeTaxable(cumGross, deductibleRate, months decimal.Decimal) decimal.Decimal {
	basic := cnMonthlyStandardDeduction.Mul(months)
	statutory := cumGross.Mul(deductibleRate).Add(cnMedicalFixedMonthly.Mul(months))
	taxable := cumGross.Sub(basic).Sub(statutory)
	if taxable.IsNegative() {
		return decimal.Zero
	}
	return taxable
}

// ComputeWithholding emits up to three lines (CN_SOCIAL_INSURANCE,
// CN_HOUSING_FUND, CN_IIT). Negative or zero gross — and zero-day
// periods — return nil.
func (cnPack) ComputeWithholding(_ context.Context, employee EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	hfRate := cnHousingFundRate(employee.Canton)
	// Employee social-insurance rate excluding housing fund:
	// pension 8% + medical 2% + unemployment 0.5%.
	siRate := cnPensionRate.Add(cnMedicalRate).Add(cnUnemploymentRate)

	out := []Deduction{}

	// Social insurance + housing fund run on the contribution base (full
	// gross by default).
	contribGross := employee.ContributionBase(gross)

	// --- Social insurance (employee share) for this slip ---
	socialInsurance := contribGross.Mul(siRate).Add(cnMedicalFixedMonthly).Round(2)
	if socialInsurance.IsPositive() {
		out = append(out, Deduction{
			Code:   "CN_SOCIAL_INSURANCE",
			Name:   "Social insurance — pension/medical/unemployment (employee, CN)",
			Amount: socialInsurance,
		})
	}

	// --- Housing provident fund (employee share), city-dependent ---
	housingFund := contribGross.Mul(hfRate).Round(2)
	if housingFund.IsPositive() {
		out = append(out, Deduction{
			Code:   "CN_HOUSING_FUND",
			Name:   "Housing provident fund (employee, CN)",
			Amount: housingFund,
		})
	}

	// --- IIT via the annual cumulative withholding method ---
	// deductibleRate is the full pre-tax-deductible employee
	// statutory rate (SI + housing fund). The flat medical add-on
	// is applied per-month inside cnCumulativeTaxable.
	deductibleRate := siRate.Add(hfRate)

	// IIT runs the cumulative method on the income-tax base: pre-tax /
	// salary-sacrifice deductions reduce taxable income, while the social
	// insurance / housing-fund contributions above stay on the full gross.
	// IncomeTaxYTD / IncomeTaxBase fall back to the full-gross figures when
	// no pre-tax reduction is supplied, preserving the legacy behaviour.
	cumGrossPrior := employee.IncomeTaxYTD()
	if cumGrossPrior.IsNegative() {
		cumGrossPrior = decimal.Zero
	}
	cumGrossNow := cumGrossPrior.Add(employee.IncomeTaxBase(gross))

	// The cumulative month index is the number of months the
	// employee has received employment income this year, including
	// the current slip's month. Per 国税发〔2018〕61号 the ¥5,000
	// standard deduction accrues from the first month of employment
	// income, not from January, so a mid-year starter must not be
	// credited the calendar month's worth of deductions.
	// EmployeeInfo.MonthsEmployedYTD carries that count; when it is
	// unset (0, e.g. a pre-existing KRecord) the pack falls back to
	// the pay-period end month, which is correct for the common
	// full-year employee (monthly payroll is the norm for Chinese
	// IIT). The prior cumulative figure is taken through the
	// previous month.
	monthIndex := int64(employee.MonthsEmployedYTD)
	if monthIndex <= 0 {
		monthIndex = int64(period.End.Month())
	}
	// A tax year has at most twelve months; clamp so a bad input
	// (data-entry error) cannot over-credit the ¥5,000/month standard
	// deduction and silently under-withhold.
	if monthIndex > 12 {
		monthIndex = 12
	}
	monthsElapsed := decimal.NewFromInt(monthIndex)
	monthsPrior := monthsElapsed.Sub(decimal.NewFromInt(1))

	cumTaxableNow := cnCumulativeTaxable(cumGrossNow, deductibleRate, monthsElapsed)
	cumTaxablePrior := cnCumulativeTaxable(cumGrossPrior, deductibleRate, monthsPrior)

	periodIIT := cnCumulativeIIT(cumTaxableNow).Sub(cnCumulativeIIT(cumTaxablePrior))
	if periodIIT.IsNegative() {
		// The cumulative method can produce a negative monthly
		// figure (a refund) when earlier months over-withheld.
		// Payroll convention shows that as zero on the slip and
		// lets the year-end reconciliation (汇算清缴) settle it.
		periodIIT = decimal.Zero
	}
	periodIIT = periodIIT.Round(2)
	if periodIIT.IsPositive() {
		out = append(out, Deduction{
			Code:   "CN_IIT",
			Name:   "Individual income tax (IIT, CN)",
			Amount: periodIIT,
		})
	}

	return out, nil
}
