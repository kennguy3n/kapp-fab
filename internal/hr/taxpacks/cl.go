package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// clPack implements Chile's monthly payroll withholding:
// Impuesto Único de Segunda Categoría (income tax, SII monthly
// schedule), AFP (10% obligatory contribution + ~0.69%
// commission), Salud (7% to Fonasa / Isapre), and Seguro de
// Cesantía (0.6% indefinite contract).
//
// Impuesto Único — DL 824/1974 Art. 43º. Monthly progressive
// schedule denominated in UTM (Unidad Tributaria Mensual).
// UTM updates monthly with the IPC; this pack pins the
// January-2025 UTM (CLP 67,294, SII Resolución Exenta 9/2025).
// Operators carry small mid-month variances on the year-end
// reliquidación (Reliquidación del Impuesto Único anual).
//
// AFP — DL 3.500/1980. 10% obligatory + administrator fee
// (varies by AFP; PlanVital lowest at 1.16% in 2025 — pack uses
// the lowest commission as default; operators should override
// per employee via a future "AFPRate" field if they want to
// reflect a specific administrator). Above the topePrevisional
// (84.6 UF/month — CLP ~3,289,800 at Jan-2025 UF) the AFP base
// is capped.
//
// Salud — Fonasa (7% statutory) or Isapre (≥7%; difference is
// the "GES" additional plan paid out of pocket). Pack always
// computes the 7% statutory floor; Isapre top-ups are not part
// of source withholding.
//
// Seguro de Cesantía — Ley 19.728. 0.6% employee for indefinite
// contracts; 0% for fixed-term (employer pays 3%). The pack
// defaults to indefinite (the common case); a future
// "ContractType" field could gate this.
//
// References:
//
//	DL 824/1974 Art. 43 (Impuesto Único de Segunda Categoría):
//	  https://www.bcn.cl/leychile/navegar?idNorma=6368
//	SII — Tabla de cálculo Impuesto Único 2025:
//	  https://www.sii.cl/valores_y_fechas/utm/utm2025.htm
//	Superintendencia de Pensiones — Tope Imponible 2025:
//	  https://www.spensiones.cl/portal/institucional/594/w3-article-15066.html
//	Ley 19.728 (Seguro de Cesantía):
//	  https://www.bcn.cl/leychile/navegar?idNorma=183920
type clPack struct{}

func init() { Register(&clPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code.
func (clPack) Country() string { return "CL" }

// EffectiveYear pins the fiscal year (2025).
func (clPack) EffectiveYear() int { return 2025 }

type clBracket struct {
	FloorUTM decimal.Decimal
	TopUTM   decimal.Decimal // 0 = open-ended
	Rate     decimal.Decimal
	// "Cantidad a Rebajar" published by SII — equivalent to
	// (Rate × Floor) − Base. We store it directly because the
	// SII table publishes it; the bracket-walk uses BaseCLP
	// derived from CantidadRebajar at slip time.
	CantidadRebajarUTM decimal.Decimal
}

var (
	// UTM January-2025 (SII).
	clUTM2025Jan = dec("67294")

	// UF January-2025 (SII), used for the AFP / Salud
	// tope imponible (84.6 UF).
	clUF2025Jan      = dec("38891.55")
	clTopeImponibleUF = dec("84.6")

	// Impuesto Único de Segunda Categoría — DL 824 Art. 43.
	// SII publishes both Rate and "Cantidad a Rebajar" so the
	// bracket-walk simplifies to: tax = Rate × monthly - CantidadRebajar.
	// Floors / Tops in UTM.
	clImpuestoUnicoBrackets = []clBracket{
		{FloorUTM: dec("0"), TopUTM: dec("13.5"), Rate: dec("0"), CantidadRebajarUTM: dec("0")},
		{FloorUTM: dec("13.5"), TopUTM: dec("30"), Rate: dec("0.04"), CantidadRebajarUTM: dec("0.54")},
		{FloorUTM: dec("30"), TopUTM: dec("50"), Rate: dec("0.08"), CantidadRebajarUTM: dec("1.74")},
		{FloorUTM: dec("50"), TopUTM: dec("70"), Rate: dec("0.135"), CantidadRebajarUTM: dec("4.49")},
		{FloorUTM: dec("70"), TopUTM: dec("90"), Rate: dec("0.23"), CantidadRebajarUTM: dec("11.14")},
		{FloorUTM: dec("90"), TopUTM: dec("120"), Rate: dec("0.304"), CantidadRebajarUTM: dec("17.80")},
		{FloorUTM: dec("120"), TopUTM: dec("310"), Rate: dec("0.35"), CantidadRebajarUTM: dec("23.32")},
		{FloorUTM: dec("310"), TopUTM: decimal.Zero, Rate: dec("0.40"), CantidadRebajarUTM: dec("38.82")},
	}

	clAFPObligatoryRate = dec("0.10")
	clAFPCommissionRate = dec("0.0116") // PlanVital 2025 (lowest commission)
	clSaludRate         = dec("0.07")
	clCesantiaRate      = dec("0.006") // Indefinite contract default
)

// ComputeWithholding emits CL_AFP, CL_SALUD, CL_SEGURO_CESANTIA,
// CL_IMPUESTO_UNICO. AFP / Salud / Cesantía apply on remuneration
// capped at the tope imponible (UF-denominated); Impuesto Único
// walks the UTM schedule.
//
// Negative / zero gross returns nil.
func (clPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Tope imponible — 84.6 UF × UF-CLP.
	tope := clTopeImponibleUF.Mul(clUF2025Jan)
	base := gross
	if base.GreaterThan(tope) {
		base = tope
	}

	if afp := base.Mul(clAFPObligatoryRate.Add(clAFPCommissionRate)).Round(2); afp.IsPositive() {
		out = append(out, Deduction{Code: "CL_AFP", Name: "Cotización AFP (empleado, obligatorio + comisión)", Amount: afp})
	}
	if salud := base.Mul(clSaludRate).Round(2); salud.IsPositive() {
		out = append(out, Deduction{Code: "CL_SALUD", Name: "Cotización Salud (empleado, 7% Fonasa/Isapre)", Amount: salud})
	}
	if ces := base.Mul(clCesantiaRate).Round(2); ces.IsPositive() {
		out = append(out, Deduction{Code: "CL_SEGURO_CESANTIA", Name: "Seguro de Cesantía (empleado)", Amount: ces})
	}

	// Impuesto Único — base = gross - cotizaciones obligatorias.
	// SII's monthly schedule applies to the after-cotizaciones
	// taxable income.
	totalCotizaciones := base.Mul(clAFPObligatoryRate.Add(clAFPCommissionRate)).
		Add(base.Mul(clSaludRate)).
		Add(base.Mul(clCesantiaRate))
	taxableCLP := gross.Sub(totalCotizaciones)
	if taxableCLP.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	taxableUTM := taxableCLP.Div(clUTM2025Jan)
	for _, b := range clImpuestoUnicoBrackets {
		hi := b.TopUTM
		if hi.IsZero() {
			hi = taxableUTM.Add(decimal.NewFromInt(1)) // ensure match for top
		}
		if taxableUTM.GreaterThan(b.FloorUTM) && taxableUTM.LessThanOrEqual(hi) {
			// tax (UTM) = rate × taxableUTM − cantidadRebajar
			taxUTM := taxableUTM.Mul(b.Rate).Sub(b.CantidadRebajarUTM)
			if taxUTM.LessThan(decimal.Zero) {
				taxUTM = decimal.Zero
			}
			impuesto := taxUTM.Mul(clUTM2025Jan).Round(2)
			if impuesto.IsPositive() {
				out = append(out, Deduction{
					Code:   "CL_IMPUESTO_UNICO",
					Name:   "Impuesto Único de Segunda Categoría",
					Amount: impuesto,
				})
			}
			break
		}
	}
	return out, nil
}
