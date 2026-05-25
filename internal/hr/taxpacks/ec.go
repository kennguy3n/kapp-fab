package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// ecPack implements Ecuador's monthly payroll withholding:
// Impuesto a la Renta (SRI Resolución NAC-DGERCGC23, annual
// schedule) and the employee share of IESS (9.45% on Aportes
// Personales — Sistema de Seguridad Social).
//
// Impuesto a la Renta — annual progressive schedule (Ley de
// Régimen Tributario Interno Art. 36). 9 brackets:
//   0 – 11,902      → 0%
//   11,902 – 15,159 → 5%
//   15,159 – 19,682 → 10%
//   19,682 – 26,031 → 12%
//   26,031 – 34,255 → 15%
//   34,255 – 45,407 → 20%
//   45,407 – 60,450 → 25%
//   60,450 – 80,605 → 30%
//   80,605 – 107,473→ 35%
//   > 107,473       → 37%
// Annual base = annual remuneración - personal-expense
// deductions (gastos personales, currently up to 1.3x the basic
// fraction). Monthly withholding = annual / 12.
//
// IESS — Aporte Personal 9.45% on imponible salary (no statutory
// ceiling; the cap is benefits-based not contributions-based).
//
// Ecuador uses USD; no currency conversion needed.
//
// References:
//
//	LRTI Art. 36 (escala IR personas naturales):
//	  https://www.sri.gob.ec/normativa/categoria/16
//	SRI Resolución NAC-DGERCGC23-...-IR 2025:
//	  https://www.sri.gob.ec/web/intersri/inicio
//	IESS — Aportes:
//	  https://www.iess.gob.ec/web/afiliado/aportes
type ecPack struct{}

func init() { Register(&ecPack{}) }

func (ecPack) Country() string  { return "EC" }
func (ecPack) EffectiveYear() int { return 2025 }

type ecBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	ecImpuestoBrackets = []ecBracket{
		{Floor: dec("0"), Top: dec("11902"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("11902"), Top: dec("15159"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("15159"), Top: dec("19682"), Base: dec("162.85"), Rate: dec("0.10")},
		{Floor: dec("19682"), Top: dec("26031"), Base: dec("615.15"), Rate: dec("0.12")},
		{Floor: dec("26031"), Top: dec("34255"), Base: dec("1377.03"), Rate: dec("0.15")},
		{Floor: dec("34255"), Top: dec("45407"), Base: dec("2610.63"), Rate: dec("0.20")},
		{Floor: dec("45407"), Top: dec("60450"), Base: dec("4841.03"), Rate: dec("0.25")},
		{Floor: dec("60450"), Top: dec("80605"), Base: dec("8601.78"), Rate: dec("0.30")},
		{Floor: dec("80605"), Top: dec("107473"), Base: dec("14648.28"), Rate: dec("0.35")},
		{Floor: dec("107473"), Top: decimal.Zero, Base: dec("24052.08"), Rate: dec("0.37")},
	}

	ecIESSRate   = dec("0.0945")
	ecAnnualDays = decimal.NewFromFloat(365.25)
)

func (ecPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	iess := gross.Mul(ecIESSRate).Round(2)
	if iess.IsPositive() {
		out = append(out, Deduction{Code: "EC_IESS", Name: "IESS Aporte Personal (empleado, 9.45%)", Amount: iess})
	}

	// Renta — annualise post-IESS, walk brackets, prorate.
	periodFraction := decimal.NewFromInt(int64(days)).Div(ecAnnualDays)
	if !periodFraction.IsPositive() {
		return out, nil
	}
	annualNet := gross.Sub(iess).Div(periodFraction)
	if annualNet.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	annualTax := walkECBrackets(annualNet, ecImpuestoBrackets)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{Code: "EC_IMPUESTO_RENTA", Name: "Impuesto a la Renta (empleado)", Amount: periodTax})
	}
	return out, nil
}

func walkECBrackets(annual decimal.Decimal, scale []ecBracket) decimal.Decimal {
	if annual.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match ecBracket
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
