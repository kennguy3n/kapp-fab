package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// fiPack implements Finland's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - Valtion tulovero (state income tax). Progressive
//     2025 schedule on annual earned income:
//        0       →   21,200   12.64%
//        21,200  →   31,500   19.00%
//        31,500  →   52,100   30.25%
//        52,100  →   88,200   34.00%
//        88,200  → open       44.00%
//
//   - Kunnallisvero (municipal income tax). 2025 weighted
//     national average is 7.50% (the major 2023 SOTE reform
//     transferred ~12.6 percentage points of municipal tax
//     into the new state tax above). Helsinki = 5.36%,
//     others up to ~10.30%. This pack uses 7.50% as the
//     default with optional overrides keyed by kunta code.
//
//   - TyEL (työeläke, employee pension). 2025 employee
//     share: 7.15% for employees under 53 (and ≥ 17), 8.65%
//     for employees aged 53-62, then back to 7.15% from 63
//     onward. Selection branches on EmployeeInfo.Age; default
//     (Age zero) is 7.15%.
//
//   - Sairausvakuutusmaksu (sickness-insurance premium):
//     1.06% (päivärahamaksu, payable on earnings ≥ EUR 16,862)
//     + 0.51% (sairaanhoitomaksu, residents only). This pack
//     uses the combined 1.57% above the floor and skips it
//     entirely below.
//
// References:
//
//	Vero — tuloverolaki 2025:
//	  https://www.vero.fi/syventavat-vero-ohjeet/
//	Kela — sairausvakuutusmaksu 2025:
//	  https://www.kela.fi/sairausvakuutusmaksut
//	ETK — TyEL-maksu 2025:
//	  https://www.etk.fi/tilastot-ja-tutkimus/tilastot/elaketurvakeskuksen-tilastot
type fiPack struct{}

func init() { Register(&fiPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (fiPack) Country() string { return "FI" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (Verohallinto 2025 verokortit + TyEL
// + sairausvakuutusmaksu).
func (fiPack) EffectiveYear() int { return 2025 }

type fiBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	fiStateTaxBrackets = []fiBracket{
		{Floor: dec("0"), Top: dec("21200"), Base: decimal.Zero, Rate: dec("0.1264")},
		{Floor: dec("21200"), Top: dec("31500"), Base: dec("2679.68"), Rate: dec("0.19")},
		{Floor: dec("31500"), Top: dec("52100"), Base: dec("4636.68"), Rate: dec("0.3025")},
		{Floor: dec("52100"), Top: dec("88200"), Base: dec("10868.18"), Rate: dec("0.34")},
		{Floor: dec("88200"), Top: decimal.Zero, Base: dec("23142.18"), Rate: dec("0.44")},
	}

	fiDefaultKuntaRate = dec("0.075")
	fiKuntaRates       = map[string]decimal.Decimal{
		"HELSINKI": dec("0.0536"),
		"ESPOO":    dec("0.0536"),
		"TAMPERE":  dec("0.0790"),
		"TURKU":    dec("0.0795"),
		"OULU":     dec("0.0810"),
	}

	fiTyELDefaultRate  = dec("0.0715")
	fiTyELMidlifeRate  = dec("0.0865")

	fiSavaPaivarahaRate     = dec("0.0106")
	fiSavaSairaanhoitoRate  = dec("0.0051")
	fiSavaFloorAnnualEUR    = dec("16862")

	fiAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to four lines:
//
//   - FI_TYEL          (TyEL employee pension, age-banded)
//   - FI_SAVA          (sickness-insurance, 1.57% combined,
//                       only above the EUR 16,862 / yr floor)
//   - FI_VALTION_VERO  (state income tax, 5-bracket walk)
//   - FI_KUNNALLISVERO (municipal income tax, 7.5% default
//                       or per-kunta override)
//
// Negative or zero gross returns nil.
func (fiPack) ComputeWithholding(_ context.Context, employee EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(fiAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// TyEL: 8.65% for ages 53-62 (inclusive), 7.15% otherwise.
	tyelRate := fiTyELDefaultRate
	if employee.Age >= 53 && employee.Age <= 62 {
		tyelRate = fiTyELMidlifeRate
	}
	tyel := gross.Mul(tyelRate).Round(2)
	if tyel.IsPositive() {
		out = append(out, Deduction{
			Code:   "FI_TYEL",
			Name:   "TyEL employee pension (FI)",
			Amount: tyel,
		})
	}

	// SAVA: 1.57% combined above EUR 16,862 / yr.
	if annualGross.GreaterThan(fiSavaFloorAnnualEUR) {
		sava := gross.Mul(fiSavaPaivarahaRate.Add(fiSavaSairaanhoitoRate)).Round(2)
		if sava.IsPositive() {
			out = append(out, Deduction{
				Code:   "FI_SAVA",
				Name:   "Sairausvakuutusmaksu (FI)",
				Amount: sava,
			})
		}
	}

	// State income tax — 5-bracket walk on annual gross.
	annualStateTax := walkFIBrackets(annualGross, fiStateTaxBrackets)
	periodStateTax := annualStateTax.Mul(periodFraction).Round(2)
	if periodStateTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "FI_VALTION_VERO",
			Name:   "Valtion tulovero (FI)",
			Amount: periodStateTax,
		})
	}

	// Municipal tax — flat per-kunta rate on annual gross.
	kuntaRate := fiDefaultKuntaRate
	if r, ok := fiKuntaRates[employee.Canton]; ok {
		kuntaRate = r
	}
	annualKunta := annualGross.Mul(kuntaRate)
	periodKunta := annualKunta.Mul(periodFraction).Round(2)
	if periodKunta.IsPositive() {
		out = append(out, Deduction{
			Code:   "FI_KUNNALLISVERO",
			Name:   "Kunnallisvero (FI)",
			Amount: periodKunta,
		})
	}

	return out, nil
}

// walkFIBrackets walks the Finnish state income tax schedule.
func walkFIBrackets(annual decimal.Decimal, brackets []fiBracket) decimal.Decimal {
	var match fiBracket
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
