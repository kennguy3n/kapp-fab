package taxpacks

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// itPack implements Italy's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - IRPEF (Imposta sul Reddito delle Persone Fisiche): the
//     national income tax. Effective from 1 January 2024, Legge
//     di Bilancio 2024 collapsed the previous four IRPEF
//     brackets into three (23% / 35% / 43%); these are retained
//     for 2025. Brackets are annual, expressed in EUR.
//
//   - "No Tax Area" (detrazione lavoro dipendente): the
//     statutory minimum is not a personal allowance subtracted
//     from gross — Italy uses tax credits (detrazioni). For
//     employees with annual reddito complessivo ≤ €8,500, the
//     detrazione fully offsets the IRPEF gross tax. For
//     reddito between €8,500 and €28,000 the detrazione tapers
//     linearly down to €1,910; between €28,000 and €50,000 it
//     tapers from €1,910 to €0; above €50,000 it is zero. This
//     pack implements the published Detrazione formula.
//
//   - Addizionale Regionale all'IRPEF: a regional surcharge
//     ranging from 1.23% (Veneto, Trento) to 3.33% (Piemonte,
//     Liguria, Campania). The national average for payroll
//     accrual purposes is ~1.73%. EmployeeInfo.PermitType is
//     used to select a specific region's rate via a small
//     lookup; the empty default uses the national average.
//
//   - Addizionale Comunale all'IRPEF: a municipal surcharge
//     averaging 0.80% nationally (range 0% – 0.9%). Without a
//     municipality identifier on the slip, this pack applies
//     the national average — operators in high-rate
//     municipalities should override per-employee.
//
//   - INPS (Istituto Nazionale della Previdenza Sociale)
//     employee share: 9.19% for ordinary employees on monthly
//     gross up to the "prima fascia" (€55,008 / yr in 2025);
//     10.19% above (the 1% MaggiorAzione applies on the
//     portion above the first ceiling). Capped at the
//     "massimale" of €120,607 / yr.
//
// References:
//
//	Agenzia delle Entrate — IRPEF 2024/2025:
//	  https://www.agenziaentrate.gov.it/portale/web/guest/aliquote-e-detrazioni-irpef
//	Legge di Bilancio 2024 (DL 216/2023):
//	  https://www.gazzettaufficiale.it/eli/id/2023/12/30/23G00227/sg
//	INPS contributi 2025:
//	  https://www.inps.it/it/it/dati-e-bilanci/circolari-numero-...
type itPack struct{}

func init() { Register(&itPack{}) }

func (itPack) Country() string { return "IT" }

// EffectiveYear returns the fiscal year the IT tables are
// calibrated for: 2025 (post-Legge di Bilancio 2024 three-band
// IRPEF + INPS 2025 ceilings).
func (itPack) EffectiveYear() int { return 2025 }

type itBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Post-Bilancio-2024 IRPEF brackets (3 bands).
	itIRPEFBrackets = []itBracket{
		{Floor: dec("0"), Top: dec("28000"), Base: dec("0"), Rate: dec("0.23")},
		{Floor: dec("28000"), Top: dec("50000"), Base: dec("6440"), Rate: dec("0.35")},
		{Floor: dec("50000"), Top: decimal.Zero, Base: dec("14140"), Rate: dec("0.43")},
	}

	// Detrazione lavoro dipendente — published values (Allegato
	// A art. 13 TUIR + 2024 amendments).
	itDetrazioneFloor    = dec("8500")    // full detrazione if income ≤ this
	itDetrazioneStep1Top = dec("28000")   // taper-1 ceiling
	itDetrazioneStep2Top = dec("50000")   // taper-2 ceiling
	itDetrazioneAtFloor  = dec("1955")    // detrazione at €8,500
	itDetrazioneAt28k    = dec("1910")    // detrazione at €28,000
	itDetrazioneAt50k    = dec("0")       // detrazione at €50,000

	// Regional surcharges (selected). Empty / unknown
	// PermitType falls back to the national average.
	itAddizionaleRegionale = map[string]decimal.Decimal{
		"LOMBARDIA":   dec("0.0173"),
		"LAZIO":       dec("0.0333"), // top band; some bands lower
		"VENETO":      dec("0.0123"),
		"PIEMONTE":    dec("0.0333"),
		"LIGURIA":     dec("0.0333"),
		"CAMPANIA":    dec("0.0333"),
		"SICILIA":     dec("0.0173"),
		"SARDEGNA":    dec("0.0173"),
		"TOSCANA":     dec("0.0173"),
		"EMILIA":      dec("0.0193"),
	}
	itAddizionaleRegionaleAvg = dec("0.0173")
	itAddizionaleComunaleAvg  = dec("0.0080")

	// INPS employee rates + ceilings (2025).
	itINPSBaseRate     = dec("0.0919")
	itINPSAddRate      = dec("0.0100") // 1% MaggiorAzione above primo massimale
	itINPSFirstCeiling = dec("55008")  // annual, 2025
	itINPSMassimale    = dec("120607") // annual cap

	itAnnualDays = decimal.NewFromFloat(365.25)
)

func (itPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(itAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// IRPEF gross tax (bracket walk on annual gross).
	annualGrossTax := walkITBrackets(annualGross, itIRPEFBrackets)

	// Detrazione lavoro dipendente — reduce IRPEF gross by the
	// employment income credit. The detrazione is itself a
	// function of annual gross (see itComputeDetrazione).
	detrazione := itComputeDetrazione(annualGross)
	annualNetTax := annualGrossTax.Sub(detrazione)
	if annualNetTax.LessThan(decimal.Zero) {
		annualNetTax = decimal.Zero
	}

	periodTax := annualNetTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "IT_IRPEF",
			Name:   "IRPEF (IT)",
			Amount: periodTax,
		})

		// Addizionali — computed against annual gross, scaled
		// down to slip. PermitType selects the region.
		regRate := itResolveRegionRate(e.PermitType)
		if reg := annualGross.Mul(regRate).Mul(periodFraction).Round(2); reg.IsPositive() {
			out = append(out, Deduction{
				Code:   "IT_ADDIZIONALE_REGIONALE",
				Name:   "Addizionale regionale IRPEF (IT)",
				Amount: reg,
			})
		}
		if com := annualGross.Mul(itAddizionaleComunaleAvg).Mul(periodFraction).Round(2); com.IsPositive() {
			out = append(out, Deduction{
				Code:   "IT_ADDIZIONALE_COMUNALE",
				Name:   "Addizionale comunale IRPEF (IT)",
				Amount: com,
			})
		}
	}

	// INPS — capped at the massimale, with the 1% MaggiorAzione
	// applied between the first ceiling and the massimale.
	if inps := itComputeINPS(gross, e.YTDGross, periodFraction); inps.IsPositive() {
		out = append(out, Deduction{
			Code:   "IT_INPS",
			Name:   "INPS (employee share, IT)",
			Amount: inps,
		})
	}

	return out, nil
}

// itComputeDetrazione resolves the lavoro-dipendente tax credit
// for the given annual income, using the piecewise tapers from
// art. 13 TUIR.
func itComputeDetrazione(annual decimal.Decimal) decimal.Decimal {
	if annual.LessThanOrEqual(itDetrazioneFloor) {
		return itDetrazioneAtFloor
	}
	if annual.GreaterThanOrEqual(itDetrazioneStep2Top) {
		return itDetrazioneAt50k
	}
	if annual.LessThanOrEqual(itDetrazioneStep1Top) {
		// Linear interpolation from (8500, 1955) to (28000, 1910).
		// Δ = 1910 - 1955 = -45 across (28000 - 8500 = 19500).
		// detrazione = 1955 + (-45) * (annual - 8500) / 19500.
		span := itDetrazioneStep1Top.Sub(itDetrazioneFloor)
		delta := itDetrazioneAt28k.Sub(itDetrazioneAtFloor)
		return itDetrazioneAtFloor.Add(
			delta.Mul(annual.Sub(itDetrazioneFloor)).Div(span),
		)
	}
	// (28000, 1910) → (50000, 0). Linear taper.
	span := itDetrazioneStep2Top.Sub(itDetrazioneStep1Top)
	delta := itDetrazioneAt50k.Sub(itDetrazioneAt28k)
	return itDetrazioneAt28k.Add(
		delta.Mul(annual.Sub(itDetrazioneStep1Top)).Div(span),
	)
}

// itResolveRegionRate looks up the regional IRPEF surcharge by
// PermitType (region code/name). Empty / unknown falls back to
// the national average.
func itResolveRegionRate(permitType string) decimal.Decimal {
	key := strings.ToUpper(strings.TrimSpace(permitType))
	if r, ok := itAddizionaleRegionale[key]; ok {
		return r
	}
	return itAddizionaleRegionaleAvg
}

// itComputeINPS computes the employee INPS contribution against
// the slip's gross. The 1% maggiorazione kicks in once cumulative
// YTD gross exceeds the first ceiling; the contribution stops
// accruing at the massimale.
func itComputeINPS(gross, ytd, periodFraction decimal.Decimal) decimal.Decimal {
	if ytd.GreaterThanOrEqual(itINPSMassimale) {
		return decimal.Zero
	}
	// Cap gross to the massimale-remaining headroom.
	remaining := itINPSMassimale.Sub(ytd)
	if gross.GreaterThan(remaining) {
		gross = remaining
	}

	// Portion below the first ceiling: base rate.
	// Portion above: base + add.
	if ytd.Add(gross).LessThanOrEqual(itINPSFirstCeiling) {
		return gross.Mul(itINPSBaseRate).Round(2)
	}
	if ytd.GreaterThanOrEqual(itINPSFirstCeiling) {
		// Entire slip in the upper band.
		return gross.Mul(itINPSBaseRate.Add(itINPSAddRate)).Round(2)
	}
	// Straddle: split the slip at the first ceiling.
	belowCeiling := itINPSFirstCeiling.Sub(ytd)
	aboveCeiling := gross.Sub(belowCeiling)
	below := belowCeiling.Mul(itINPSBaseRate)
	above := aboveCeiling.Mul(itINPSBaseRate.Add(itINPSAddRate))
	return below.Add(above).Round(2)
}

// walkITBrackets walks the IRPEF schedule against annual gross.
func walkITBrackets(annual decimal.Decimal, brackets []itBracket) decimal.Decimal {
	var match itBracket
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


