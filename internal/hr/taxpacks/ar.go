package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// arPack implements Argentina's monthly federal payroll
// withholding: Impuesto a las Ganancias (4ta categoría — earnings
// from labour) per ARCA's monthly progressive table, plus the
// employee share of the three statutory social-security
// contributions (Jubilación 11%, INSSJP 3%, Obra Social 3%).
//
// Ganancias 4ta categoría — Ley 20.628 (T.O. 2019) with the
// Régimen Cedular reform (Lei 27.725/2024) and the inflationary
// indexation per Resolución General ARCA. The schedule is
// progressive across 8 monthly brackets in ARS; the "mínimo no
// imponible" + "deducciones especiales" + per-dependent
// deduction must be subtracted from the gross before walking the
// brackets. The table updates multiple times per fiscal year (the
// Ley 27.617 indexation mechanism re-indexes thresholds quarterly
// against RIPTE / IPC), so this pack pins the 2025-Q1 schedule
// and exposes the indexation constants for mechanical bumps.
//
// Social security (employee share, ANSeS):
//   - Jubilación (SIPA): 11% of remunerative gross.
//   - INSSJP (PAMI):     3% of remunerative gross.
//   - Obra Social:       3% of remunerative gross.
//
// All three are capped at the "Máximo Imponible Previsional" —
// MOPRE for Ley 24.241, currently ARS 4,562,829.81/month for the
// March-2025 base period (Resolución ANSeS 56/2025).
//
// References:
//
//	Ley 27.725 (Régimen Cedular sobre los mayores ingresos):
//	  https://servicios.infoleg.gob.ar/infolegInternet/anexos/385000-389999/389093/norma.htm
//	ARCA — Resolución General 5641/2025 (escala Ganancias 2025):
//	  https://www.argentina.gob.ar/normativa/nacional/resolución_general-5641-2025
//	ARCA — Tablas de retención Ganancias 4ta categoría (vigente):
//	  https://www.afip.gob.ar/gananciasYBienes/personales/escalas.asp
//	ANSeS — Bases Imponibles Previsionales (Res. 56/2025):
//	  https://www.argentina.gob.ar/anses/jubilados-pensionados/aportes-y-contribuciones
type arPack struct{}

func init() { Register(&arPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code.
func (arPack) Country() string { return "AR" }

// EffectiveYear pins the fiscal year the bracket table tracks
// (2025). Inflation indexation may bump this in mid-year minor
// updates; the EffectiveYear remains the calendar year of the
// most-recent quarterly re-indexation.
func (arPack) EffectiveYear() int { return 2025 }

type arBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

// Source: ARCA RG 5641/2025 (March-2025 re-indexation).
// Anexo II — Escala Art. 94 LIG (ganancia neta sujeta a impuesto).
// Annual table (used here as the source-of-truth; monthly
// withholding is just annual/12). All amounts in ARS.
var (
	arGananciasBrackets = []arBracket{
		{Floor: dec("0"), Top: dec("1200000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("1200000"), Top: dec("2400000"), Base: dec("60000"), Rate: dec("0.09")},
		{Floor: dec("2400000"), Top: dec("3600000"), Base: dec("168000"), Rate: dec("0.12")},
		{Floor: dec("3600000"), Top: dec("5400000"), Base: dec("312000"), Rate: dec("0.15")},
		{Floor: dec("5400000"), Top: dec("10800000"), Base: dec("582000"), Rate: dec("0.19")},
		{Floor: dec("10800000"), Top: dec("16200000"), Base: dec("1608000"), Rate: dec("0.23")},
		{Floor: dec("16200000"), Top: dec("24300000"), Base: dec("2850000"), Rate: dec("0.27")},
		{Floor: dec("24300000"), Top: dec("32400000"), Base: dec("5037000"), Rate: dec("0.31")},
		{Floor: dec("32400000"), Top: decimal.Zero, Base: dec("7548000"), Rate: dec("0.35")},
	}

	// Mínimo no imponible (MNI), Special deduction for employees
	// (Deducción Especial Inc. c LIG), and per-dependent deduction
	// per ARCA RG 5641/2025. Values are ANNUAL ARS; the pack
	// scales to the slip period using days/365.25. Updates land
	// here together with the bracket re-indexation.
	arMNIAnnual            = dec("3091035")
	arDeduccionEspAnnual   = dec("14836968") // 4.8× MNI for employees in relation of dependency.
	arPerDependentAnnual   = dec("2911135")  // hijos / hijastros under-18.
	arAnnualDays           = decimal.NewFromFloat(365.25)
	arSocialJubilacionRate = dec("0.11")
	arSocialINSSJPRate     = dec("0.03")
	arSocialObraSocialRate = dec("0.03")

	// Máximo Imponible Previsional (MOPRE), ANSeS Res. 56/2025
	// March-2025 reference period. Monthly.
	arMOPREMonthly = dec("4562829.81")
)

// ComputeWithholding emits up to four Deduction lines:
// AR_JUBILACION, AR_INSSJP, AR_OBRA_SOCIAL, AR_GANANCIAS.
//
// Ganancias is computed by annualising slip gross via
// days/365.25, subtracting the annual MNI + special deduction +
// per-dependent deduction, walking the annual bracket schedule,
// then prorating back to the slip period.
//
// Social-security lines apply per-slip on remunerative gross
// capped at MOPRE (period-prorated).
//
// Negative / zero gross returns nil.
func (arPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(arAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}

	out := []Deduction{}

	// Social-security cap — period-prorated MOPRE.
	socialBase := gross
	monthlyCap := arMOPREMonthly.Mul(decimal.NewFromInt(int64(days))).Div(decimal.NewFromFloat(30.4167))
	if socialBase.GreaterThan(monthlyCap) {
		socialBase = monthlyCap
	}
	if jub := socialBase.Mul(arSocialJubilacionRate).Round(2); jub.IsPositive() {
		out = append(out, Deduction{Code: "AR_JUBILACION", Name: "Jubilación SIPA (empleado)", Amount: jub})
	}
	if inssjp := socialBase.Mul(arSocialINSSJPRate).Round(2); inssjp.IsPositive() {
		out = append(out, Deduction{Code: "AR_INSSJP", Name: "INSSJP PAMI (empleado)", Amount: inssjp})
	}
	if os := socialBase.Mul(arSocialObraSocialRate).Round(2); os.IsPositive() {
		out = append(out, Deduction{Code: "AR_OBRA_SOCIAL", Name: "Obra Social (empleado)", Amount: os})
	}

	// Ganancias 4ta categoría — annualise then bracket-walk.
	annualGross := gross.Div(periodFraction)
	deductions := arMNIAnnual.Add(arDeduccionEspAnnual)
	if e.NumDependents > 0 {
		deductions = deductions.Add(arPerDependentAnnual.Mul(decimal.NewFromInt(int64(e.NumDependents))))
	}
	netAnnual := annualGross.Sub(deductions)
	if netAnnual.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	annualTax := walkARBrackets(netAnnual, arGananciasBrackets)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "AR_GANANCIAS",
			Name:   "Impuesto a las Ganancias 4ta categoría",
			Amount: periodTax,
		})
	}
	return out, nil
}

// walkARBrackets walks the Ganancias annual schedule. Same
// contract as walkCABrackets — Floor-matched Base + (income -
// Floor) × Rate.
func walkARBrackets(annual decimal.Decimal, scale []arBracket) decimal.Decimal {
	if annual.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match arBracket
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
