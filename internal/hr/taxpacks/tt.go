package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// ttPack implements Trinidad & Tobago's monthly payroll
// withholding: PAYE (Pay-As-You-Earn — IRD Schedule), NIS
// (National Insurance Scheme, employee class lookup), and
// Health Surcharge (flat weekly amount).
//
// PAYE — Income Tax Act §76. Schedule:
//   - Personal allowance: TTD 90,000/year exempt.
//   - 25% on chargeable income up to TTD 1,000,000.
//   - 30% on chargeable income above TTD 1,000,000.
//
// NIS — NIBTT Class table. The classes are flat TTD per week
// amounts indexed by the employee's "average weekly earnings"
// bracket (16 classes for 2024+). The pack uses the 2024 table
// (NIBTT effective 6 March 2023, still in force for 2025). The
// employee contribution column is what's withheld; the brackets
// approximate a 4.0% employee rate on capped earnings (cap is
// class XVI top: AWE TTD 13,600/week → employee TTD 543.99/wk).
//
// Health Surcharge — Income Tax Act §76A:
//   - Weekly earnings ≤ TTD 109.00     → TTD 4.80/week.
//   - Weekly earnings >  TTD 109.00    → TTD 8.25/week.
//
// References:
//
//	Income Tax Act §76, §76A:
//	  https://rgd.legalaffairs.gov.tt/laws2/alphabetical_list/lawspdfs/75.01.pdf
//	IRD — Computation of PAYE (2024):
//	  https://www.ird.gov.tt/employer/PAYE.html
//	NIBTT — Earnings Classes and Contributions (2023+):
//	  https://www.nibtt.net/Contributions/EarningsClass.html
type ttPack struct{}

func init() { Register(&ttPack{}) }

func (ttPack) Country() string  { return "TT" }
func (ttPack) EffectiveYear() int { return 2025 }

// ttNISClass is one row of the NIBTT earnings-class table.
// LowerWeekly / UpperWeekly are the AWE bracket (TTD); EmployeeContribWeekly
// is the published employee TTD/week withholding for the class.
type ttNISClass struct {
	LowerWeekly         decimal.Decimal
	UpperWeekly         decimal.Decimal // 0 = open-ended
	EmployeeContribWeekly decimal.Decimal
}

var (
	ttPersonalAllowance       = dec("90000")
	ttPAYEThreshold           = dec("1000000")
	ttPAYERate1               = dec("0.25")
	ttPAYERate2               = dec("0.30")
	ttPAYEBaseTier2           = dec("250000") // 1,000,000 × 25%
	ttAnnualDays              = decimal.NewFromFloat(365.25)
	ttWeeksPerMonth           = dec("4.333")
	ttHealthSurchargeLowWeekly = dec("4.80")
	ttHealthSurchargeHighWeekly = dec("8.25")
	ttHealthThresholdWeekly    = dec("109.00")

	// NIBTT 2023+ Earnings Classes (Class I–XVI). Employee
	// contributions are the published weekly TTD amounts (the
	// total class × 0.333 employee share).
	ttNISClasses = []ttNISClass{
		{LowerWeekly: dec("0"), UpperWeekly: dec("199.99"), EmployeeContribWeekly: dec("11.20")},     // I
		{LowerWeekly: dec("200"), UpperWeekly: dec("339.99"), EmployeeContribWeekly: dec("17.96")},   // II
		{LowerWeekly: dec("340"), UpperWeekly: dec("449.99"), EmployeeContribWeekly: dec("26.30")},   // III
		{LowerWeekly: dec("450"), UpperWeekly: dec("609.99"), EmployeeContribWeekly: dec("35.36")},   // IV
		{LowerWeekly: dec("610"), UpperWeekly: dec("759.99"), EmployeeContribWeekly: dec("45.71")},   // V
		{LowerWeekly: dec("760"), UpperWeekly: dec("929.99"), EmployeeContribWeekly: dec("56.40")},   // VI
		{LowerWeekly: dec("930"), UpperWeekly: dec("1119.99"), EmployeeContribWeekly: dec("68.36")},  // VII
		{LowerWeekly: dec("1120"), UpperWeekly: dec("1299.99"), EmployeeContribWeekly: dec("80.66")}, // VIII
		{LowerWeekly: dec("1300"), UpperWeekly: dec("1489.99"), EmployeeContribWeekly: dec("92.96")}, // IX
		{LowerWeekly: dec("1490"), UpperWeekly: dec("1709.99"), EmployeeContribWeekly: dec("106.61")},// X
		{LowerWeekly: dec("1710"), UpperWeekly: dec("1909.99"), EmployeeContribWeekly: dec("120.96")},// XI
		{LowerWeekly: dec("1910"), UpperWeekly: dec("2139.99"), EmployeeContribWeekly: dec("135.31")},// XII
		{LowerWeekly: dec("2140"), UpperWeekly: dec("2379.99"), EmployeeContribWeekly: dec("150.86")},// XIII
		{LowerWeekly: dec("2380"), UpperWeekly: dec("2629.99"), EmployeeContribWeekly: dec("167.41")},// XIV
		{LowerWeekly: dec("2630"), UpperWeekly: dec("3137.99"), EmployeeContribWeekly: dec("184.46")},// XV
		{LowerWeekly: dec("3138"), UpperWeekly: decimal.Zero, EmployeeContribWeekly: dec("207.05")},  // XVI
	}
)

func (ttPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	periodFraction := decimal.NewFromInt(int64(days)).Div(ttAnnualDays)
	if !periodFraction.IsPositive() {
		return out, nil
	}

	// Approximate weeks-in-period via days/7 for the NIS lookup
	// and the health surcharge step. For a standard monthly
	// payroll period this collapses to 4.333 weeks.
	weeks := decimal.NewFromInt(int64(days)).Div(decimal.NewFromInt(7))
	weeklyGross := decimal.Zero
	if weeks.IsPositive() {
		weeklyGross = gross.Div(weeks)
	}

	// NIS — class lookup by weekly AWE, then × weeks.
	nisWeekly := lookupTTNIS(weeklyGross)
	if nis := nisWeekly.Mul(weeks).Round(2); nis.IsPositive() {
		out = append(out, Deduction{Code: "TT_NIS", Name: "NIS (National Insurance, employee)", Amount: nis})
	}

	// Health Surcharge — flat weekly step.
	hs := ttHealthSurchargeLowWeekly
	if weeklyGross.GreaterThan(ttHealthThresholdWeekly) {
		hs = ttHealthSurchargeHighWeekly
	}
	if total := hs.Mul(weeks).Round(2); total.IsPositive() {
		out = append(out, Deduction{Code: "TT_HEALTH_SURCHARGE", Name: "Health Surcharge", Amount: total})
	}

	// PAYE — annualise, subtract personal allowance, walk.
	annualGross := gross.Div(periodFraction)
	chargeable := annualGross.Sub(ttPersonalAllowance)
	if chargeable.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	annualTax := decimal.Zero
	if chargeable.LessThanOrEqual(ttPAYEThreshold) {
		annualTax = chargeable.Mul(ttPAYERate1)
	} else {
		annualTax = ttPAYEBaseTier2.Add(chargeable.Sub(ttPAYEThreshold).Mul(ttPAYERate2))
	}
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{Code: "TT_PAYE", Name: "PAYE (Pay-As-You-Earn)", Amount: periodTax})
	}
	return out, nil
}

func lookupTTNIS(weekly decimal.Decimal) decimal.Decimal {
	if weekly.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	for _, c := range ttNISClasses {
		if c.UpperWeekly.IsZero() {
			return c.EmployeeContribWeekly
		}
		if weekly.GreaterThanOrEqual(c.LowerWeekly) && weekly.LessThanOrEqual(c.UpperWeekly) {
			return c.EmployeeContribWeekly
		}
	}
	return decimal.Zero
}
