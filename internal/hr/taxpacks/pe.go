package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// pePack implements Peru's payroll withholding: Renta de Quinta
// Categoría (income tax on labour earnings, SUNAT) and ONP
// (Sistema Nacional de Pensiones, 13% employee). AFP (~12.6%
// total) is an alternative the worker may elect at hire — the
// pack defaults to ONP because it is the statutory default; a
// future EmployeeInfo field could surface "AFP" with the
// administrator commission table.
//
// Renta 5ta — TUO LIR Art. 53. ANNUAL progressive schedule
// expressed in UIT (Unidad Impositiva Tributaria). UIT 2025
// = PEN 5,350 (DS 260-2024-EF). First 7 UIT (≈ PEN 37,450) are
// exempt. Brackets:
//   - 0 – 5 UIT       → 8%
//   - 5 – 20 UIT      → 14%
//   - 20 – 35 UIT     → 17%
//   - 35 – 45 UIT     → 20%
//   - > 45 UIT        → 30%
// Monthly withholding is annual_tax / 12, with the SUNAT
// "proyección anual" mechanism — gross_month × (12 - month) +
// previous accumulated income. This pack uses the simpler
// "annualise via days/365.25" approach which gives the same
// total over the year for a constant salary; year-end PDT 601
// reconciles small variances.
//
// ONP — DL 19.990 / Ley 25.967. 13% employee.
// EsSalud (9%) is employer-only.
//
// References:
//
//	TUO LIR (DS 179-2004-EF) Art. 53:
//	  https://www.sunat.gob.pe/legislacion/renta/tuo.html
//	DS 260-2024-EF (UIT 2025):
//	  https://www.gob.pe/institucion/mef/normas-legales/6193180-260-2024-ef
//	DL 19.990 (Sistema Nacional de Pensiones):
//	  https://www.peru.gob.pe/docs/PLANES/14306/PLAN_14306_2014_Decreto_Ley_19990.pdf
type pePack struct{}

func init() { Register(&pePack{}) }

func (pePack) Country() string  { return "PE" }
func (pePack) EffectiveYear() int { return 2025 }

type peBracket struct {
	FloorUIT decimal.Decimal
	TopUIT   decimal.Decimal // 0 = open-ended
	BaseUIT  decimal.Decimal // cumulative tax at floor, in UIT
	Rate     decimal.Decimal
}

var (
	// UIT 2025 — DS 260-2024-EF.
	peUIT2025 = dec("5350")

	// Renta 5ta annual schedule. After the 7-UIT exempt deduction.
	peRentaBrackets = []peBracket{
		{FloorUIT: dec("0"), TopUIT: dec("5"), BaseUIT: dec("0"), Rate: dec("0.08")},
		{FloorUIT: dec("5"), TopUIT: dec("20"), BaseUIT: dec("0.4"), Rate: dec("0.14")},
		{FloorUIT: dec("20"), TopUIT: dec("35"), BaseUIT: dec("2.5"), Rate: dec("0.17")},
		{FloorUIT: dec("35"), TopUIT: dec("45"), BaseUIT: dec("5.05"), Rate: dec("0.20")},
		{FloorUIT: dec("45"), TopUIT: decimal.Zero, BaseUIT: dec("7.05"), Rate: dec("0.30")},
	}

	peONPRate    = dec("0.13")
	peAnnualDays = decimal.NewFromFloat(365.25)
	peExemptUIT  = decimal.NewFromInt(7)
)

func (pePack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// ONP — 13% on gross.
	if onp := gross.Mul(peONPRate).Round(2); onp.IsPositive() {
		out = append(out, Deduction{Code: "PE_ONP", Name: "ONP (Sistema Nacional de Pensiones)", Amount: onp})
	}

	// Renta 5ta — annualise, subtract 7-UIT exempt, walk brackets,
	// prorate back to the slip.
	periodFraction := decimal.NewFromInt(int64(days)).Div(peAnnualDays)
	if !periodFraction.IsPositive() {
		return out, nil
	}
	annualGross := gross.Div(periodFraction)
	annualGrossUIT := annualGross.Div(peUIT2025)
	taxableUIT := annualGrossUIT.Sub(peExemptUIT)
	if taxableUIT.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	annualTaxUIT := walkPEBrackets(taxableUIT, peRentaBrackets)
	annualTax := annualTaxUIT.Mul(peUIT2025)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{Code: "PE_RENTA_5TA", Name: "Renta de Quinta Categoría (SUNAT)", Amount: periodTax})
	}
	return out, nil
}

func walkPEBrackets(uit decimal.Decimal, scale []peBracket) decimal.Decimal {
	if uit.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match peBracket
	matched := false
	for _, b := range scale {
		if uit.LessThanOrEqual(b.FloorUIT) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.BaseUIT.Add(uit.Sub(match.FloorUIT).Mul(match.Rate))
}
