package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// noPack implements Norway's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - Inntektsskatt (general income tax). Flat 22% on alminnelig
//     inntekt (general income after a simplified
//     minstefradrag of NOK 104,450 / yr — 46% of gross up to
//     the cap).
//
//   - Trinnskatt (bracket tax) on personal income above NOK
//     217,400 / yr:
//        217,400 →   306,050     1.7%
//        306,050 →   697,150     4.0%
//        697,150 →   942,400    13.6%
//        942,400 → 1,410,750    16.6%
//      1,410,750 → open         17.6%
//     Bracket tax applies on top of the 22% inntektsskatt.
//
//   - Trygdeavgift (national insurance) on personal income.
//     7.7% for employees (under 70). Income below NOK 99,650
//     is exempt; between NOK 99,650 and ~NOK 109,950 the
//     contribution phases up from zero. This pack uses the
//     straight 7.7% above the floor for simplicity.
//
// References:
//
//	Skatteetaten — skattesatser 2025:
//	  https://www.skatteetaten.no/satser/
//	Skatteetaten — Trinnskatt:
//	  https://www.skatteetaten.no/person/skatt/skattekort/skattekort-info/trinnskatt/
//	NAV — Trygdeavgift:
//	  https://www.nav.no/no/person/arbeid/skatt-og-pensjon/medlemskap-og-avgifter
type noPack struct{}

func init() { Register(&noPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (noPack) Country() string { return "NO" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (Skatteetaten satser 2025).
func (noPack) EffectiveYear() int { return 2025 }

type noBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	noInntektsskattRate = dec("0.22")
	// Simplified minstefradrag (standard deduction): 46% of
	// gross, capped at NOK 104,450 / yr. Real-world payroll uses
	// per-employee skattekort; this approximation matches the
	// 2025 official cap.
	noMinstefradragRate = dec("0.46")
	noMinstefradragCap  = dec("104450")

	// Cumulative tax at each Floor — verified arithmetically:
	//
	//   B1 → 0 (no tax below 217,400)
	//   B2 → 0       + (306,050   - 217,400) × 0.017 = 1,507.05
	//   B3 → 1507.05 + (697,150   - 306,050) × 0.040 = 17,151.05
	//   B4 → 17151.05 + (942,400  - 697,150) × 0.136 = 50,505.05
	//   B5 → 50505.05 + (1,410,750 - 942,400) × 0.166 = 128,251.15
	//
	// TestBracketTablesAreContiguousNO pins these in
	// europe_extended_packs_test.go so a future rate update can't
	// reintroduce a transcription bug.
	noTrinnskattBrackets = []noBracket{
		{Floor: dec("217400"), Top: dec("306050"), Base: decimal.Zero, Rate: dec("0.017")},
		{Floor: dec("306050"), Top: dec("697150"), Base: dec("1507.05"), Rate: dec("0.040")},
		{Floor: dec("697150"), Top: dec("942400"), Base: dec("17151.05"), Rate: dec("0.136")},
		{Floor: dec("942400"), Top: dec("1410750"), Base: dec("50505.05"), Rate: dec("0.166")},
		{Floor: dec("1410750"), Top: decimal.Zero, Base: dec("128251.15"), Rate: dec("0.176")},
	}

	noTrygdeavgiftRate  = dec("0.077")
	noTrygdeavgiftFloor = dec("99650")

	noAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to three lines:
//
//   - NO_TRYGDEAVGIFT (national insurance, 7.7% above the floor)
//   - NO_INNTEKTSSKATT (22% flat on general income post-
//     minstefradrag)
//   - NO_TRINNSKATT (bracket tax above NOK 217,400 / yr)
//
// Negative or zero gross returns nil.
func (noPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(noAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// Trygdeavgift — 7.7% on personal income above floor.
	if annualGross.GreaterThan(noTrygdeavgiftFloor) {
		annualTryg := annualGross.Mul(noTrygdeavgiftRate)
		periodTryg := annualTryg.Mul(periodFraction).Round(2)
		if periodTryg.IsPositive() {
			out = append(out, Deduction{
				Code:   "NO_TRYGDEAVGIFT",
				Name:   "Trygdeavgift (NO)",
				Amount: periodTryg,
			})
		}
	}

	// Minstefradrag = min(46% × gross, cap).
	minstefradrag := annualGross.Mul(noMinstefradragRate)
	if minstefradrag.GreaterThan(noMinstefradragCap) {
		minstefradrag = noMinstefradragCap
	}
	alminneligInntekt := annualGross.Sub(minstefradrag)
	if alminneligInntekt.LessThan(decimal.Zero) {
		alminneligInntekt = decimal.Zero
	}

	annualInntektsskatt := alminneligInntekt.Mul(noInntektsskattRate)
	periodInntektsskatt := annualInntektsskatt.Mul(periodFraction).Round(2)
	if periodInntektsskatt.IsPositive() {
		out = append(out, Deduction{
			Code:   "NO_INNTEKTSSKATT",
			Name:   "Inntektsskatt (NO)",
			Amount: periodInntektsskatt,
		})
	}

	// Trinnskatt — bracket walk on annual gross (personal
	// income), prorated back.
	annualTrinnskatt := walkNOBrackets(annualGross, noTrinnskattBrackets)
	periodTrinnskatt := annualTrinnskatt.Mul(periodFraction).Round(2)
	if periodTrinnskatt.IsPositive() {
		out = append(out, Deduction{
			Code:   "NO_TRINNSKATT",
			Name:   "Trinnskatt (NO)",
			Amount: periodTrinnskatt,
		})
	}

	return out, nil
}

// walkNOBrackets walks the trinnskatt table.
func walkNOBrackets(annual decimal.Decimal, brackets []noBracket) decimal.Decimal {
	var match noBracket
	matched := false
	for _, b := range brackets {
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
