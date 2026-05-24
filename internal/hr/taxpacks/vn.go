package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// vnPack implements Vietnam's monthly payroll-side statutory
// withholdings:
//
//   - PIT (Personal Income Tax) under Law on Personal Income Tax
//     04/2007/QH12 + amendments (Resolution 954/2020/UBTVQH14 on
//     personal deductions). The pack applies the 7-bracket
//     progressive schedule (5–35%) directly to monthly taxable
//     income after the personal deduction (11,000,000 VND / month)
//     and dependent deduction (4,400,000 VND / month per
//     qualifying dependent). The brackets in the statute are
//     defined per month (unlike most jurisdictions that publish
//     annual brackets), so the pack does not annualise; it
//     applies them at the slip's effective monthly rate via
//     period.Days() / 30.4375.
//
//     Non-resident individuals (foreign nationals present in
//     Vietnam for less than 183 days in a tax year and lacking a
//     permanent residence) are taxed at a flat 20% on VN-sourced
//     employment income under PIT Law article 26 — the progressive
//     schedule does not apply. The pack branches on
//     EmployeeInfo.Resident: false → flat 20% on gross, no
//     SI/HI/UI; true → progressive PIT + the three social
//     contributions.
//
//   - SI / HI / UI: Social Insurance 8%, Health Insurance 1.5%,
//     Unemployment Insurance 1% — employee shares per Law on
//     Social Insurance 58/2014/QH13 and Decree 28/2015/ND-CP.
//     Apply to residents only; foreign non-residents do not
//     contribute (they have neither SI nor UI eligibility, and
//     HI is granted via separate employer-side health-insurance
//     for foreign workers under Decree 146/2018/ND-CP — the
//     employee-deducted line is residents-only).
//     SI/HI insurable wage is capped at 20× the base salary
//     (lương cơ sở; 2,340,000 VND from 1 Jul 2024 → 46,800,000
//     VND / month). UI is capped at 20× the regional minimum
//     wage; this pack uses the Region I 2024 figure
//     (4,960,000 × 20 = 99,200,000 VND / month).
//
// References:
//
//	Law on PIT 04/2007/QH12 (Article 22):
//	  https://english.luatvietnam.vn/laws/Law-No-04-2007-QH12-on-Personal-Income-Tax-25879-Doc1.html
//	Resolution 954/2020 (personal deduction):
//	  https://thuvienphapluat.vn/van-ban/Tien-te-Ngan-hang/Nghi-quyet-954-2020-UBTVQH14-ve-muc-giam-tru-gia-canh-444985.aspx
//	Decree 73/2024/ND-CP (base salary 2.34M effective 1 Jul 2024):
//	  https://datafiles.chinhphu.vn/cpp/files/vbpq/2024/6/73-cp.pdf
type vnPack struct{}

func init() { Register(&vnPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (vnPack) Country() string { return "VN" }

// EffectiveYear returns the fiscal year the VN tables are
// calibrated for: 2024 — Resolution 954/2020 personal deductions
// + Decree 73/2024 base-salary 2.34M (effective 1 July 2024).
func (vnPack) EffectiveYear() int { return 2024 }

type vnBracket struct {
	Floor decimal.Decimal // monthly taxable income, VND
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Article 22 schedule — monthly taxable income in VND.
	vnBracketsResident = []vnBracket{
		{Floor: dec("0"), Top: dec("5000000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("5000000"), Top: dec("10000000"), Base: dec("250000"), Rate: dec("0.10")},
		{Floor: dec("10000000"), Top: dec("18000000"), Base: dec("750000"), Rate: dec("0.15")},
		{Floor: dec("18000000"), Top: dec("32000000"), Base: dec("1950000"), Rate: dec("0.20")},
		{Floor: dec("32000000"), Top: dec("52000000"), Base: dec("4750000"), Rate: dec("0.25")},
		{Floor: dec("52000000"), Top: dec("80000000"), Base: dec("9750000"), Rate: dec("0.30")},
		{Floor: dec("80000000"), Top: decimal.Zero, Base: dec("18150000"), Rate: dec("0.35")},
	}

	vnPersonalDeduction  = dec("11000000")
	vnDependentDeduction = dec("4400000")

	// Defense-in-depth upper bound on declared dependents.
	// Resolution 954/2020/UBTVQH14 does not impose a hard
	// statutory cap on the dependent count (the Vietnamese
	// PIT Law allows any qualifying dependent — children,
	// parents, spouses lacking taxable income), but a wizard
	// or payroll-import bug sending a runaway value (e.g.
	// 10_000) would silently zero out the VN_PIT line via
	// the dependent-deduction math (11M + 10_000 × 4.4M ≈
	// 44 billion VND of synthetic deduction). Twenty
	// dependents already covers any plausible real-world
	// household and matches the conservative defense-in-depth
	// pattern used by the TH pack (thMaxDependents). Operators
	// with legitimately higher counts can land a future
	// per-tenant override; the cap below trips well above the
	// 99.99 %ile of real declarations.
	vnMaxDependents = 20

	vnSIRate = dec("0.08")
	vnHIRate = dec("0.015")
	vnUIRate = dec("0.01")

	// 20× base salary cap (2,340,000 × 20).
	vnSIHICeiling = dec("46800000")
	// 20× Region I minimum wage cap (4,960,000 × 20).
	vnUICeiling = dec("99200000")

	// Average month length used to scale off-cycle slips against
	// the statutory monthly bracket table.
	vnAvgMonthDays = decimal.NewFromFloat(30.4375)

	// PIT Law art. 26: non-resident flat 20% on VN-sourced
	// employment income.
	vnNonResidentRate = dec("0.20")
)

// ComputeWithholding emits VN_PIT (after personal + dependent
// deductions) plus VN_SI / VN_HI / VN_UI for residents.
// Non-residents (per PIT Law art. 26) get VN_NONRESIDENT_TAX at
// the flat 20% rate and no social contributions. Zero-amount
// lines are omitted.
func (vnPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Non-resident foreign individual: flat 20% on VN-sourced
	// employment income, no social contributions.
	if !e.Resident {
		nrTax := gross.Mul(vnNonResidentRate).Round(2)
		if nrTax.IsPositive() {
			out = append(out, Deduction{
				Code:   "VN_NONRESIDENT_TAX",
				Name:   "Non-resident PIT flat 20% (VN)",
				Amount: nrTax,
			})
		}
		return out, nil
	}

	// Bring the slip onto an average-month basis so off-cycle /
	// non-monthly slips compare against the statute's monthly
	// brackets.
	monthFraction := decimal.NewFromInt(int64(days)).Div(vnAvgMonthDays)
	monthlyGross := gross.Div(monthFraction)

	deps := e.NumDependents
	if deps < 0 {
		deps = 0
	}
	if deps > vnMaxDependents {
		deps = vnMaxDependents
	}
	deduction := vnPersonalDeduction.Add(
		vnDependentDeduction.Mul(decimal.NewFromInt(int64(deps))),
	)
	taxable := monthlyGross.Sub(deduction)
	if taxable.LessThan(decimal.Zero) {
		taxable = decimal.Zero
	}
	monthlyTax := walkVNBrackets(taxable, vnBracketsResident)
	periodTax := monthlyTax.Mul(monthFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "VN_PIT",
			Name:   "Personal income tax withholding (VN)",
			Amount: periodTax,
		})
	}

	// SI / HI share the 20× base-salary cap.
	siHiBase := gross
	if siHiBase.GreaterThan(vnSIHICeiling) {
		siHiBase = vnSIHICeiling
	}
	if si := siHiBase.Mul(vnSIRate).Round(2); si.IsPositive() {
		out = append(out, Deduction{
			Code:   "VN_SI",
			Name:   "Social Insurance (employee share, VN)",
			Amount: si,
		})
	}
	if hi := siHiBase.Mul(vnHIRate).Round(2); hi.IsPositive() {
		out = append(out, Deduction{
			Code:   "VN_HI",
			Name:   "Health Insurance (employee share, VN)",
			Amount: hi,
		})
	}

	// UI uses the 20× region-I minimum-wage cap.
	uiBase := gross
	if uiBase.GreaterThan(vnUICeiling) {
		uiBase = vnUICeiling
	}
	if ui := uiBase.Mul(vnUIRate).Round(2); ui.IsPositive() {
		out = append(out, Deduction{
			Code:   "VN_UI",
			Name:   "Unemployment Insurance (employee share, VN)",
			Amount: ui,
		})
	}
	return out, nil
}

func walkVNBrackets(monthly decimal.Decimal, scale []vnBracket) decimal.Decimal {
	var match vnBracket
	matched := false
	for _, b := range scale {
		if monthly.LessThanOrEqual(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.Base.Add(monthly.Sub(match.Floor).Mul(match.Rate))
}
