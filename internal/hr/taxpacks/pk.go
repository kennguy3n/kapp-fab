package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// pkPack implements Pakistan's payroll-side statutory withholdings:
//
//   - Income tax deducted at source on salary under s. 149 of the
//     Income Tax Ordinance 2001, using the salaried-individual
//     progressive slabs from the annual Finance Act (applicable
//     where salary is more than 75% of taxable income). The pack
//     annualises the slip's gross (days / 365.25), walks the
//     progressive schedule (PKR) and prorates the annual tax back
//     to the slip period.
//
//   - Employees' Old-Age Benefits Institution (EOBI) employee
//     contribution under the EOBI Act 1976: 1% of the minimum wage
//     (the statutory insurable wage), a fixed PKR 370/month while
//     the federal minimum wage is PKR 37,000/month. The employer's
//     5% share is not a slip deduction. EOBI is collected only on
//     insurable employment; the pack emits it for resident
//     employees alongside income tax.
//
// A non-resident individual's salary for services rendered in
// Pakistan is taxed on the same salaried slabs; the pack applies the
// bracket walk for non-residents but omits EOBI (which attaches to
// insurable local employment handled by a dedicated slip type).
//
// References:
//
//	Income Tax Ordinance 2001, s. 149 + Finance Act salaried slabs:
//	  https://www.fbr.gov.pk/
//	FBR salary tax card (current tax year):
//	  https://www.fbr.gov.pk/categ/income-tax/51147/131159
//	EOBI Act 1976 (1% employee contribution on minimum wage):
//	  https://www.eobi.gov.pk/
type pkPack struct{}

func init() { Register(&pkPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (pkPack) Country() string { return "PK" }

// EffectiveYear returns the calendar year the PK slabs are
// calibrated for: 2024 (Finance Act 2024, tax year 2025 salaried
// slabs; EOBI insurable wage PKR 37,000).
func (pkPack) EffectiveYear() int { return 2024 }

type pkBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Finance Act 2024 salaried progressive schedule (annual
	// taxable income, PKR). Base is cumulative tax at each Floor.
	pkBracketsResident = []pkBracket{
		{Floor: dec("0"), Top: dec("600000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("600000"), Top: dec("1200000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("1200000"), Top: dec("2200000"), Base: dec("30000"), Rate: dec("0.15")},
		{Floor: dec("2200000"), Top: dec("3200000"), Base: dec("180000"), Rate: dec("0.25")},
		{Floor: dec("3200000"), Top: dec("4100000"), Base: dec("430000"), Rate: dec("0.30")},
		{Floor: dec("4100000"), Top: decimal.Zero, Base: dec("700000"), Rate: dec("0.35")},
	}

	// EOBI employee contribution: 1% of the statutory insurable
	// wage (the federal minimum wage), a fixed monthly amount.
	pkEOBIInsurableWage = dec("37000")
	pkEOBIEmployeeRate  = dec("0.01")

	pkAnnualPeriodFraction = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits PK_INCOME_TAX (annualised progressive
// deduction at source) and, for residents, PK_EOBI_EMPLOYEE (fixed
// 1%-of-minimum-wage old-age-benefit contribution).
func (pkPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(pkAnnualPeriodFraction)
	// Income tax runs on the post-pre-tax base; EOBI keeps its fixed
	// insurable-wage contribution base.
	annualGross := e.IncomeTaxBase(gross).Div(periodFraction)
	annualTax := walkPKBrackets(annualGross, pkBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "PK_INCOME_TAX",
			Name:   "Income tax deducted at source (PK)",
			Amount: periodTax,
		})
	}

	if !e.Resident {
		return out, nil
	}

	// EOBI employee contribution: 1% of the insurable wage, scaled
	// to the slip period so a non-monthly slip is not over-charged.
	monthsInPeriod := decimal.NewFromInt(int64(days)).Div(decimal.NewFromFloat(30.4375))
	eobi := pkEOBIInsurableWage.Mul(pkEOBIEmployeeRate).Mul(monthsInPeriod).Round(2)
	if eobi.IsPositive() {
		out = append(out, Deduction{
			Code:   "PK_EOBI_EMPLOYEE",
			Name:   "EOBI old-age benefit contribution (employee, PK)",
			Amount: eobi,
		})
	}

	return out, nil
}

func walkPKBrackets(annual decimal.Decimal, scale []pkBracket) decimal.Decimal {
	var match pkBracket
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
