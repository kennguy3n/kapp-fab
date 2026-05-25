package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// gbPack implements the United Kingdom's payroll-side statutory
// withholdings for the 2025-26 tax year (6 April 2025 → 5 April
// 2026):
//
//   - PAYE Income Tax (HMRC). England/Wales/Northern Ireland
//     resident scale: personal allowance £12,570, basic 20% to
//     £50,270, higher 40% to £125,140, additional 45% above.
//     Personal allowance tapers down by £1 for every £2 of
//     adjusted net income above £100,000, so the £12,570
//     allowance is fully withdrawn at £125,140. The taper is
//     applied annualised before the bracket walk so it lines up
//     with HMRC's actual tables.
//
//   - National Insurance Contributions, Class 1 employee
//     primary contributions. 8% from the Primary Threshold
//     (£12,570 / yr = £242 / week = £1,048 / month) up to the
//     Upper Earnings Limit (£50,270 / yr = £967 / week =
//     £4,189 / month), then 2% above. Rates effective from 6
//     April 2024 onward (Autumn Statement 2023 cuts).
//
//   - Plan 1 student loan deduction: 9% on monthly earnings
//     above the £24,990 / yr threshold (≈ £2,083 / month). Plan 2,
//     Plan 4, Plan 5, and Postgraduate Loan thresholds are
//     documented as constants for future per-employee gating
//     once the wizard captures the plan type. Plan 1 is used as
//     the default for employees who flagged HasStudentLoan but
//     have no PermitType-encoded plan code (consistent with
//     HMRC's "Plan 1 unless told otherwise" guidance from the
//     SL3 booklet, although employers must capture the plan from
//     the P45 / starter checklist in practice).
//
// Personal allowance gating is annualised: the pack uses the
// slip's gross prorated to an annual figure to decide whether
// the £100,000 taper applies. Tenants whose employees cross the
// taper mid-year should override on the slip; this pack mirrors
// HMRC's PAYE behaviour which is also strictly cumulative,
// applying the taper based on YTD earnings at slip date.
//
// References:
//
//	HMRC PAYE rates and thresholds 2025/26:
//	  https://www.gov.uk/government/publications/rates-and-allowances-income-tax/income-tax-rates-and-allowances-for-current-and-past-years
//	National Insurance rates for employees:
//	  https://www.gov.uk/national-insurance-rates-letters
//	Student loan repayment thresholds:
//	  https://www.gov.uk/repaying-your-student-loan/what-you-pay
type gbPack struct{}

func init() { Register(&gbPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (gbPack) Country() string { return "GB" }

// EffectiveYear returns the fiscal year the GB tables are
// calibrated for: 2025 (HMRC 2025-26 tax-year tables; rates
// effective 6 April 2025).
func (gbPack) EffectiveYear() int { return 2025 }

// gbBracket is one row of the PAYE percentage method table.
// Floor / Top are annualised gross above the personal allowance;
// Rate is the marginal rate applied to (taxable - Floor) up to Top.
type gbBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal // 0 = open-ended
	Rate  decimal.Decimal
}

var (
	// HMRC 2025-26 PAYE brackets, applied to taxable income
	// AFTER the personal allowance has been deducted.
	// Scottish rate-payers have their own scale (5 bands); not
	// implemented here — the wizard does not yet capture
	// residency for Scotland vs rUK.
	gbPAYEBrackets = []gbBracket{
		{Floor: dec("0"), Top: dec("37700"), Rate: dec("0.20")},        // basic rate
		{Floor: dec("37700"), Top: dec("125140"), Rate: dec("0.40")},   // higher rate
		{Floor: dec("125140"), Top: decimal.Zero, Rate: dec("0.45")},   // additional rate
	}

	gbPersonalAllowance       = dec("12570")
	gbPersonalAllowanceTaper  = dec("100000")
	gbPersonalAllowanceFully  = dec("125140") // allowance fully withdrawn at this level

	// NIC Class 1 employee primary thresholds (annualised).
	gbNICPrimaryThreshold = dec("12570")
	gbNICUpperEarnings    = dec("50270")
	gbNICMainRate         = dec("0.08") // 8% from PT to UEL
	gbNICAdditionalRate   = dec("0.02") // 2% above UEL

	// Student loan thresholds (annual). Plan 1 is the default
	// when the EmployeeInfo flag is set without a specific
	// plan code.
	gbStudentLoanRate         = dec("0.09")
	gbStudentLoanPlan1Annual  = dec("24990") // Plan 1 — pre-2012 starters
	// Plan 2/4/5/PGL retained as documentation; selection
	// requires per-employee plan capture which the wizard
	// adds in a later PR.
	_ = dec("27295") // Plan 2 (post-2012 England/Wales)
	_ = dec("31395") // Plan 4 (Scotland)
	_ = dec("25000") // Plan 5 (post-2023 England)
	_ = dec("21000") // Postgraduate Loan (6% rate)

	gbPeriodsPerYear = decimal.NewFromFloat(365.25)
)

func (gbPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(gbPeriodsPerYear)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	// Resolve the personal allowance after the £100k taper.
	// Annualised gross is the right input here because HMRC's
	// taper compares "adjusted net income" — which for a pure-
	// salary employee with no other sources is the same as
	// annualised gross at the current cadence.
	allowance := gbResolvePersonalAllowance(annualGross)

	// PAYE bracket walk on taxable = annualGross - allowance.
	taxable := annualGross.Sub(allowance)
	if taxable.LessThan(decimal.Zero) {
		taxable = decimal.Zero
	}
	annualTax := walkGBBrackets(taxable, gbPAYEBrackets)
	periodTax := annualTax.Mul(periodFraction).Round(2)

	out := []Deduction{}
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "GB_PAYE",
			Name:   "PAYE income tax (GB)",
			Amount: periodTax,
		})
	}

	// NIC Class 1 employee primary contributions on the slip
	// gross (no annualisation — NIC is computed per-pay-period
	// at the pay-period-prorated thresholds).
	if nic := gbComputeNIC(gross, periodFraction); nic.IsPositive() {
		out = append(out, Deduction{
			Code:   "GB_NIC",
			Name:   "National Insurance Class 1 (employee, GB)",
			Amount: nic,
		})
	}

	// Student loan: Plan 1 threshold prorated to slip period.
	// EmployeeInfo doesn't currently expose HasStudentLoan
	// directly — we infer from FilingType == "student_loan" as
	// a stopgap until the wizard adds the field. Other packs
	// follow the same convention for jurisdiction-specific
	// per-employee flags that haven't yet been promoted to
	// first-class EmployeeInfo fields.
	if gbHasStudentLoan(e) {
		threshold := gbStudentLoanPlan1Annual.Mul(periodFraction)
		if gross.GreaterThan(threshold) {
			sl := gross.Sub(threshold).Mul(gbStudentLoanRate).Round(2)
			if sl.IsPositive() {
				out = append(out, Deduction{
					Code:   "GB_STUDENT_LOAN",
					Name:   "Student loan repayment Plan 1 (GB)",
					Amount: sl,
				})
			}
		}
	}

	return out, nil
}

// gbResolvePersonalAllowance applies the £100k taper. For every
// £2 of annualised gross above £100,000, £1 is withdrawn from
// the personal allowance, fully withdrawing it at £125,140.
func gbResolvePersonalAllowance(annualGross decimal.Decimal) decimal.Decimal {
	if annualGross.LessThanOrEqual(gbPersonalAllowanceTaper) {
		return gbPersonalAllowance
	}
	if annualGross.GreaterThanOrEqual(gbPersonalAllowanceFully) {
		return decimal.Zero
	}
	// Taper: allowance reduces by 1 for every 2 over £100k.
	excess := annualGross.Sub(gbPersonalAllowanceTaper)
	reduction := excess.Div(decimal.NewFromInt(2))
	allowance := gbPersonalAllowance.Sub(reduction)
	if allowance.LessThan(decimal.Zero) {
		return decimal.Zero
	}
	return allowance
}

// gbComputeNIC computes the Class 1 employee primary contribution.
// NIC uses per-pay-period thresholds rather than annualised ones,
// so we prorate the annual PT / UEL by the period fraction.
func gbComputeNIC(gross, periodFraction decimal.Decimal) decimal.Decimal {
	pt := gbNICPrimaryThreshold.Mul(periodFraction)
	uel := gbNICUpperEarnings.Mul(periodFraction)
	if gross.LessThanOrEqual(pt) {
		return decimal.Zero
	}
	var nic decimal.Decimal
	hi := gross
	if hi.GreaterThan(uel) {
		hi = uel
	}
	nic = hi.Sub(pt).Mul(gbNICMainRate)
	if gross.GreaterThan(uel) {
		nic = nic.Add(gross.Sub(uel).Mul(gbNICAdditionalRate))
	}
	return nic.Round(2)
}

// gbHasStudentLoan returns true if the slip should accrue student
// loan deductions. Today this gates off FilingType == "student_loan"
// pending a first-class EmployeeInfo.HasStudentLoan field.
func gbHasStudentLoan(e EmployeeInfo) bool {
	return e.FilingType == "student_loan"
}

// walkGBBrackets walks the PAYE schedule against taxable income
// (post-allowance). Same contract as walkBrackets but no Base
// column — each band's tax is computed as the full marginal
// amount on its slice of taxable income.
func walkGBBrackets(taxable decimal.Decimal, brackets []gbBracket) decimal.Decimal {
	var tax decimal.Decimal
	for _, b := range brackets {
		if taxable.LessThanOrEqual(b.Floor) {
			break
		}
		hi := b.Top
		if hi.IsZero() || taxable.LessThan(hi) {
			hi = taxable
		}
		tax = tax.Add(hi.Sub(b.Floor).Mul(b.Rate))
	}
	return tax
}
