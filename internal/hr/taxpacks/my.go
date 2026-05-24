package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// myPack implements Malaysia's monthly payroll-side statutory
// withholdings:
//
//   - PCB / MTD (Potongan Cukai Bulanan / Monthly Tax Deduction)
//     under the Income Tax (Deduction from Remuneration) Rules
//     1994 as amended for YA 2024 by Budget 2023. The pack uses
//     the resident progressive schedule with the post-Budget-2023
//     middle-bracket reductions (35,001–50,000 → 6%, 50,001–
//     70,000 → 11%, 70,001–100,000 → 19%). The CCM "Computerised
//     Calculation Method" published by LHDN applies allowances /
//     reliefs that vary per employee; this pack ships the
//     straight progressive bracket walk (no reliefs deducted)
//     because the engine does not yet model TP1 / TP3 forms.
//     Annual tax is prorated to the slip period by
//     period.Days() / 365.25 (matching the AU / TH / ID packs
//     for off-cycle slip stability) rather than the strict
//     LHDN CCM divide-by-12. For a standard 31-day month this
//     yields period fraction 0.0849 vs 1/12 ≈ 0.0833, a ~2%
//     uplift over the LHDN CCM calculator; operators who file
//     MTD against the official calculator can either reconcile
//     at the end-of-year EA / e-Filing step or set
//     DeductionAccountMap to a TP1 relief override on a future
//     PR.
//
//   - EPF (Employees Provident Fund / KWSP) employee contribution
//     at 11% for employees under 60 and 5.5% for 60+, per the
//     EPF Act 1991 Third Schedule (Part A / Part C). No upper cap
//     on the employee share for the standard ratepath; the
//     historical wage-based reduction at RM 5,000 applies only to
//     the *employer* side.
//
//   - SOCSO (PERKESO) employee 0.5% under the Employees Social
//     Security Act 1969 First Schedule, capped at an insurable
//     wage of RM 5,000 / month. This pack applies the rate as a
//     flat 0.5% on the capped insurable wage, yielding RM 25.00
//     / month at the cap. PERKESO's published First Schedule
//     rounds the 4,900.01–5,000 band down to RM 24.75 / month
//     EE (each 100-RM band has a discrete rounded amount); the
//     RM 0.25 / month difference is within typical year-end
//     reconciliation tolerance and is recovered by the engine's
//     Form 2 / 8A reporting. Encoding the full First Schedule
//     banded table is a future-PR refinement tracked in
//     docs/TAX_PACK_MAINTENANCE.md. SOCSO stops accruing for
//     employees who first joined SOCSO at age 60 or above; this
//     pack does not branch on first-enrolment date — that's a
//     KRecord migration concern.
//
//   - EIS (Employment Insurance System) employee 0.2% under
//     Employment Insurance System Act 2017, same RM 5,000 /
//     month insurable wage ceiling. Flat-rate yields RM 10.00 /
//     month at the cap; PERKESO's Second Schedule rounds to
//     RM 9.90 / month EE at the 4,900.01–5,000 band. Same
//     reconciliation note as SOCSO above.
//
// References:
//
//	LHDN MTD schedule / CCM:
//	  https://www.hasil.gov.my/en/employer/responsibilities-of-employer/monthly-tax-deduction-mtd/
//	EPF Third Schedule:
//	  https://www.kwsp.gov.my/en/employer/contribution
//	SOCSO / EIS contribution tables:
//	  https://www.perkeso.gov.my/en/employer/contribution.html
type myPack struct{}

func init() { Register(&myPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (myPack) Country() string { return "MY" }

// EffectiveYear returns the fiscal year the MY tables are
// calibrated for: YA 2024 (Budget 2023 brackets, KWSP/SOCSO/EIS
// rates effective 2024). The bracket table moves with each annual
// finance act; the tax-rate-review CI workflow flags this for
// re-confirmation.
func (myPack) EffectiveYear() int { return 2024 }

// myBracket mirrors the LHDN MTD schedule. Floor / Top are
// chargeable annual income in MYR; Base is the cumulative tax at
// Floor; Rate applies to (income - Floor). Top = 0 is open-ended.
type myBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// LHDN Schedule (YA 2024 resident, after Budget 2023
	// middle-bracket reductions).
	myBracketsResident = []myBracket{
		{Floor: dec("0"), Top: dec("5000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("5000"), Top: dec("20000"), Base: dec("0"), Rate: dec("0.01")},
		{Floor: dec("20000"), Top: dec("35000"), Base: dec("150"), Rate: dec("0.03")},
		{Floor: dec("35000"), Top: dec("50000"), Base: dec("600"), Rate: dec("0.06")},
		{Floor: dec("50000"), Top: dec("70000"), Base: dec("1500"), Rate: dec("0.11")},
		{Floor: dec("70000"), Top: dec("100000"), Base: dec("3700"), Rate: dec("0.19")},
		{Floor: dec("100000"), Top: dec("400000"), Base: dec("9400"), Rate: dec("0.25")},
		{Floor: dec("400000"), Top: dec("600000"), Base: dec("84400"), Rate: dec("0.26")},
		{Floor: dec("600000"), Top: dec("2000000"), Base: dec("136400"), Rate: dec("0.28")},
		{Floor: dec("2000000"), Top: decimal.Zero, Base: dec("528400"), Rate: dec("0.30")},
	}

	// EPF employee contribution rates (Third Schedule).
	myEPFRateBelow60 = dec("0.11")
	myEPFRate60Plus  = dec("0.055")

	// Income Tax Act 1967 s.45 + LHDN public ruling on
	// non-resident employees: flat 30% on Malaysian-sourced
	// employment income (from YA 2020) with no reliefs.
	myNonResidentRate = dec("0.30")

	// SOCSO + EIS employee rates and the shared RM 5,000 / month
	// insurable wage ceiling.
	mySOCSOEmployeeRate = dec("0.005")
	myEISEmployeeRate   = dec("0.002")
	myInsurableCeiling  = dec("5000")

	myPeriodsPerYear = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits MY_PCB (resident progressive PIT),
// MY_EPF (employee KWSP share), MY_SOCSO and MY_EIS (capped at the
// RM 5,000 / month insurable wage). Non-residents (per ITA 1967
// s.45) instead get MY_NONRESIDENT_TAX at the flat 30% rate, no
// reliefs, and no statutory contributions (EPF/SOCSO/EIS are
// citizen-and-permanent-resident only). Lines with a zero or
// negative computed amount are omitted so the slip stays free of
// cosmetic zero rows.
func (myPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	// Non-resident: flat 30% on gross. EPF / SOCSO / EIS
	// eligibility is restricted to citizens and permanent
	// residents under each respective scheme's enabling act,
	// so non-resident slips emit only the income-tax line.
	if !e.Resident {
		nr := gross.Mul(myNonResidentRate).Round(2)
		if !nr.IsPositive() {
			return nil, nil
		}
		return []Deduction{{
			Code:   "MY_NONRESIDENT_TAX",
			Name:   "Non-resident PCB flat 30% (MY)",
			Amount: nr,
		}}, nil
	}

	// Annualise → walk brackets → prorate. Same period-fraction
	// pattern as the AU pack so off-cycle slips compute correctly.
	periodFraction := decimal.NewFromInt(int64(days)).Div(myPeriodsPerYear)
	annualGross := gross.Div(periodFraction)
	annualTax := walkMYBrackets(annualGross, myBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)

	out := []Deduction{}
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "MY_PCB",
			Name:   "PCB / MTD withholding (MY)",
			Amount: periodTax,
		})
	}

	// EPF employee share — no insurable-wage cap on the employee
	// side under the standard rate path; the rate steps at age 60.
	epfRate := myEPFRateBelow60
	if e.Age >= 60 {
		epfRate = myEPFRate60Plus
	}
	epf := gross.Mul(epfRate).Round(2)
	if epf.IsPositive() {
		out = append(out, Deduction{
			Code:   "MY_EPF",
			Name:   "EPF / KWSP (employee share, MY)",
			Amount: epf,
		})
	}

	// SOCSO + EIS share the RM 5,000 / month insurable ceiling.
	// For pay periods that are not exactly one calendar month the
	// statutory schedule is ambiguous; this pack applies the
	// ceiling per-period as the conservative reading (matching
	// PERKESO's pro-rata guidance for daily-rated workers).
	insurable := gross
	if insurable.GreaterThan(myInsurableCeiling) {
		insurable = myInsurableCeiling
	}
	socso := insurable.Mul(mySOCSOEmployeeRate).Round(2)
	if socso.IsPositive() {
		out = append(out, Deduction{
			Code:   "MY_SOCSO",
			Name:   "SOCSO / PERKESO (employee share, MY)",
			Amount: socso,
		})
	}
	eis := insurable.Mul(myEISEmployeeRate).Round(2)
	if eis.IsPositive() {
		out = append(out, Deduction{
			Code:   "MY_EIS",
			Name:   "EIS (employee share, MY)",
			Amount: eis,
		})
	}

	return out, nil
}

// walkMYBrackets resolves annual tax from the LHDN bracket table.
// Identical to walkAUBrackets shape; shares the (Floor, Top, Base,
// Rate) form. Replicated rather than abstracted because the
// `myBracket` and `auBracket` types are deliberately distinct so
// the bracket-table source-of-truth lives one struct away from its
// pack and a future schedule change can't accidentally cross-leak.
func walkMYBrackets(annual decimal.Decimal, scale []myBracket) decimal.Decimal {
	var match myBracket
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
