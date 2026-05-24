package taxpacks

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// inPack implements India's monthly payroll-side statutory
// withholdings:
//
//   - TDS (Tax Deducted at Source) under the New Tax Regime
//     (section 115BAC, post-Budget-2024 amendment), which is the
//     *default* regime from AY 2024-25 onwards. The pack annualises
//     monthly gross via period.Days() / 365.25 and walks the
//     6-bracket schedule (0/5/10/15/20/30%). A flat standard
//     deduction of ₹75,000 is subtracted from annual gross before
//     the brackets (Finance (No. 2) Act 2024, raising it from the
//     prior ₹50,000). Section 87A rebate up to ₹25,000 zeroes out
//     tax on annual income ≤ ₹7,00,000 — implemented as a
//     post-bracket clamp matching the Income Tax Department's
//     calculator.
//
//     The Old Tax Regime is recognised via EmployeeInfo.TaxRegime
//     == "old" but defaults to "new" when empty *or unknown*
//     (matching the post-Budget-2023 statutory default). Any
//     non-"old" value falls back to "new" so a typo or
//     unrecognised string cannot silently zero out TDS — the
//     pack picks the conservative (revenue-protecting) default.
//     The pack documents the old-regime fallback as out of scope
//     for this PR — employees opting out must declare via Form
//     10-IEA which the wizard does not yet capture; PR-2c+ will
//     revisit.
//
//   - EPF (Employees' Provident Fund) employee share: 12% of
//     basic salary, capped at the ₹15,000 / month statutory wage
//     ceiling (EPF Act, Para 26A + EPFO 2014 amendment). The pack
//     treats `gross` as the basic for slip simplicity; tenants
//     wanting a basic-split need to encode it via the salary
//     structure rather than relying on this pack to derive basic
//     from gross.
//
//   - ESI (Employees' State Insurance) employee share: 0.75% of
//     gross wages, applicable only when monthly wages ≤ ₹21,000
//     (ESI Act, Section 2(9) + 2019 amendment). The pack skips
//     ESI entirely when monthly gross exceeds the threshold.
//
//   - Professional Tax: state-specific. Maharashtra slab applied
//     when EmployeeInfo.PermitType (used here as a state-code
//     proxy because Frappe HRMS reuses the same field for
//     state-by-jurisdiction selection) is "MH" — ₹200 / month
//     for monthly gross > ₹10,000 with ₹300 deducted in February
//     for the annual catch-up. Other states return zero;
//     Karnataka's slab is provided as a documented constant for
//     PR-2c+ to wire in via PermitType == "KA".
//
// References:
//
//	Finance (No. 2) Act 2024 (standard deduction + brackets):
//	  https://incometaxindia.gov.in/Pages/acts/finance-act-2024.aspx
//	Section 87A rebate:
//	  https://incometaxindia.gov.in/Pages/tools/tax-calculator.aspx
//	EPF Act, Para 26A:
//	  https://www.epfindia.gov.in/site_docs/PDFs/Acts/EPF_MP_Act_1952.pdf
//	ESI Act, Section 2(9) (wage threshold):
//	  https://www.esic.gov.in/attachments/publicationfile/3a8b3dab3c2c75aaee8f7f7cf3e4a87c.pdf
//	Maharashtra Professional Tax Act, 1975:
//	  https://mahagst.gov.in/en/professional-tax
type inPack struct{}

func init() { Register(&inPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (inPack) Country() string { return "IN" }

// EffectiveYear returns the fiscal year the IN tables are
// calibrated for: 2024 (Finance (No. 2) Act 2024 brackets +
// ₹75,000 standard deduction + ₹25,000 87A rebate, applicable for
// AY 2025-26 / FY 2024-25 returns).
func (inPack) EffectiveYear() int { return 2024 }

type inBracket struct {
	Floor decimal.Decimal // annual taxable income, INR
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// New regime (default) post-Budget-2024 schedule. Brackets
	// in INR per FY 2024-25.
	inBracketsNewRegime = []inBracket{
		{Floor: dec("0"), Top: dec("300000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("300000"), Top: dec("700000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("700000"), Top: dec("1000000"), Base: dec("20000"), Rate: dec("0.10")},
		{Floor: dec("1000000"), Top: dec("1200000"), Base: dec("50000"), Rate: dec("0.15")},
		{Floor: dec("1200000"), Top: dec("1500000"), Base: dec("80000"), Rate: dec("0.20")},
		{Floor: dec("1500000"), Top: decimal.Zero, Base: dec("140000"), Rate: dec("0.30")},
	}

	inStandardDeduction = dec("75000") // FY 2024-25, new regime
	inRebate87ALimit    = dec("700000") // Annual income threshold for rebate
	inRebate87AMax      = dec("25000")  // Maximum rebate amount

	inEPFRate    = dec("0.12")
	inEPFCeiling = dec("15000") // monthly basic-wage cap

	inESIRate      = dec("0.0075")
	inESIThreshold = dec("21000") // monthly wage limit for ESI applicability

	// Professional tax slabs — Maharashtra. Karnataka shown as
	// documentation for PR-2c+; currently unused.
	inPTMaharashtraMonthly  = dec("200")
	inPTMaharashtraFloor    = dec("10000") // monthly gross > 10k → PT applies
	inPTMaharashtraFebExtra = dec("100")   // Feb deduction is 300 (200 + 100 catch-up)

	inAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits IN_TDS, IN_EPF, IN_ESI, IN_PT.
// Zero-amount lines are omitted.
func (inPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// TDS — new regime by default (Budget-2023 default election).
	// Defense-in-depth: any unrecognised string (typos, future
	// regime codes, garbled wizard input) collapses to "new" so
	// the pack cannot silently skip TDS by accident. Explicitly
	// declaring "old" still bypasses the bracket walk — that
	// path is intentionally inert until PR-2c+ wires Form
	// 10-IEA + the 80C/80D/HRA deduction stack.
	regime := strings.ToLower(strings.TrimSpace(e.TaxRegime))
	if regime != "old" {
		regime = "new"
	}

	if regime == "new" {
		periodFraction := decimal.NewFromInt(int64(days)).Div(inAnnualDays)
		if periodFraction.IsPositive() {
			annualGross := gross.Div(periodFraction)
			taxableAnnual := annualGross.Sub(inStandardDeduction)
			if taxableAnnual.LessThan(decimal.Zero) {
				taxableAnnual = decimal.Zero
			}
			annualTax := walkINBrackets(taxableAnnual, inBracketsNewRegime)
			// Section 87A rebate (new regime, post-Finance-Act-2023):
			//
			//  1. If taxable income ≤ ₹7,00,000, the rebate
			//     wipes out the entire computed tax (capped at
			//     the ₹25,000 statutory maximum).
			//
			//  2. The proviso to s.87A (added by Finance Act
			//     2023, retained by Finance Act 2024) prevents
			//     the cliff at exactly ₹7,00,001: when income
			//     marginally exceeds the limit, the tax payable
			//     is capped at the *excess* over ₹7,00,000.
			//     i.e. a taxpayer at ₹7,00,100 pays at most ₹100
			//     of tax, not ~₹25,005. The cap applies while
			//     it improves the taxpayer's position (solving
			//     20,000 + 0.10 × (x - 700000) = x - 700000
			//     gives the break-even at x ≈ ₹7,22,222 — above
			//     that the bracket-walk tax is already lower than
			//     the excess, so the cap is a no-op).
			//
			// Threshold is compared against taxable income
			// (post-standard-deduction) to match the ITD's own
			// income-tax calculator; the ITD's worked examples
			// use the same convention.
			switch {
			case taxableAnnual.LessThanOrEqual(inRebate87ALimit) && annualTax.LessThanOrEqual(inRebate87AMax):
				// Within the rebate envelope — full waiver.
				annualTax = decimal.Zero
			case taxableAnnual.GreaterThan(inRebate87ALimit):
				// Marginal relief: cap tax at (income - 7L)
				// while that figure is lower than the
				// bracket-walk result.
				excess := taxableAnnual.Sub(inRebate87ALimit)
				if excess.LessThan(annualTax) {
					annualTax = excess
				}
			}
			periodTax := annualTax.Mul(periodFraction).Round(2)
			if periodTax.IsPositive() {
				out = append(out, Deduction{
					Code:   "IN_TDS",
					Name:   "TDS withholding — new regime (IN)",
					Amount: periodTax,
				})
			}
		}
	}
	// Old regime: out of scope for PR-2b. Surfacing TaxRegime as
	// "old" today yields no IN_TDS line — the wizard will
	// reject "old" at the API layer until PR-2c+ wires Form
	// 10-IEA capture and the 80C/80D/HRA stack.

	// EPF: 12% of basic, capped at the statutory ₹15,000 / month.
	// We treat gross as basic per the comment block above; a
	// tenant with a distinct basic must encode it via the
	// salary_structure component split rather than rely on this
	// pack to derive it.
	epfBase := gross
	if epfBase.GreaterThan(inEPFCeiling) {
		epfBase = inEPFCeiling
	}
	if epf := epfBase.Mul(inEPFRate).Round(2); epf.IsPositive() {
		out = append(out, Deduction{
			Code:   "IN_EPF",
			Name:   "EPF employee contribution (IN)",
			Amount: epf,
		})
	}

	// ESI: 0.75% of gross, only when monthly wages ≤ ₹21,000. We
	// apply the threshold against gross directly since this pack
	// runs on the slip's natural monthly cadence; off-cycle slips
	// scale through the engine, not here.
	if gross.LessThanOrEqual(inESIThreshold) {
		if esi := gross.Mul(inESIRate).Round(2); esi.IsPositive() {
			out = append(out, Deduction{
				Code:   "IN_ESI",
				Name:   "ESI employee contribution (IN)",
				Amount: esi,
			})
		}
	}

	// Professional Tax: state-specific. Maharashtra is wired in
	// today; Karnataka / WB / TN follow in PR-2c+ via PermitType
	// state-code resolution.
	state := strings.ToUpper(strings.TrimSpace(e.PermitType))
	if state == "MH" && gross.GreaterThan(inPTMaharashtraFloor) {
		pt := inPTMaharashtraMonthly
		// February catch-up: ₹300 instead of ₹200 to reach the
		// ₹2,500 annual maximum (₹200 × 11 + ₹300 = ₹2,500).
		if period.End.Month() == 2 {
			pt = pt.Add(inPTMaharashtraFebExtra)
		}
		out = append(out, Deduction{
			Code:   "IN_PT",
			Name:   "Professional Tax — Maharashtra (IN)",
			Amount: pt,
		})
	}

	return out, nil
}

// walkINBrackets walks the new-regime schedule. The Top of the
// last bracket is decimal.Zero, treated as "no upper bound".
func walkINBrackets(annual decimal.Decimal, scale []inBracket) decimal.Decimal {
	var match inBracket
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
