package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// coPack implements Colombia's monthly payroll withholding:
// Retención en la fuente sobre rentas de trabajo (DIAN Art. 383
// ET), the employee shares of Pensión (4%) and Salud (4%), and
// the Fondo de Solidaridad Pensional surtax (FSP, 1% / 2%) for
// high earners.
//
// Retención — Art. 383 ET. The withholding schedule is denominated
// in UVT (Unidad de Valor Tributario). The 2025 UVT = COP 49,799
// (DIAN Resolución 000084 de 28/11/2024). Floors / Tops below are
// expressed in monthly COP via `uvt × <count>`. The schedule
// applies after subtracting:
//   - Aportes obligatorios a salud y pensión (4% + 4%).
//   - Renta exenta del 25% del ingreso laboral, capped at 790
//     UVT annually (≈ 65.83 UVT/month).
//
// Pensión + Salud (Ley 100/1993, Decreto 758/1990):
//   - Pensión empleado: 4% sobre IBC (Ingreso Base de Cotización),
//     floored a 1 SMLMV, capped a 25 SMLMV.
//   - Salud empleado:   4% sobre IBC.
//   - FSP: 1% over 4 SMLMV, +1% over 16 SMLMV (max 2%).
//
// SMLMV 2025 = COP 1,423,500 (Decreto 1572 de 26-12-2024).
// SMLMV cap (25× SMLMV) = COP 35,587,500.
//
// References:
//
//	DIAN Resolución 000084/2024 (UVT 2025):
//	  https://www.dian.gov.co/normatividad/Documents/Resolucion-000084-de-2024.pdf
//	Estatuto Tributario Art. 383 (Retención mensual):
//	  https://estatuto.co/?e=485
//	Ministerio del Trabajo — Decreto 1572/2024 (SMLMV 2025):
//	  https://www.mintrabajo.gov.co/web/guest/prensa/comunicados/2024/diciembre/decreto-1572-2024
//	Ley 100/1993 (Sistema Integral de Seguridad Social):
//	  https://www.minsalud.gov.co/Normatividad_Nuevo/LEY%200100%20DE%201993.pdf
type coPack struct{}

func init() { Register(&coPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code.
func (coPack) Country() string { return "CO" }

// EffectiveYear pins the fiscal year the tables track: 2025.
func (coPack) EffectiveYear() int { return 2025 }

type coBracket struct {
	FloorUVT decimal.Decimal // bracket floor in UVT
	TopUVT   decimal.Decimal // bracket top in UVT (0 = open-ended)
	BaseUVT  decimal.Decimal // cumulative tax at floor in UVT
	Rate     decimal.Decimal
}

var (
	// UVT 2025 — DIAN Resolución 000084/2024.
	coUVT2025 = dec("49799")

	// SMLMV 2025 — Decreto 1572/2024.
	coSMLMV2025 = dec("1423500")

	// Retención mensual — Art. 383 ET. Brackets expressed in UVT.
	// The standard published schedule:
	//   0 - 95 UVT      → 0%
	//   95 - 150 UVT    → 19% (parcela: 95 UVT)
	//   150 - 360 UVT   → 28% (parcela: 10.45 UVT)
	//   360 - 640 UVT   → 33% (parcela: 69.25 UVT)
	//   640 - 945 UVT   → 35% (parcela: 162.25 UVT)
	//   945 - 2300 UVT  → 37% (parcela: 268.75 UVT)
	//   > 2300 UVT      → 39% (parcela: 770.10 UVT)
	coRetencionBrackets = []coBracket{
		{FloorUVT: dec("0"), TopUVT: dec("95"), BaseUVT: dec("0"), Rate: dec("0")},
		{FloorUVT: dec("95"), TopUVT: dec("150"), BaseUVT: dec("0"), Rate: dec("0.19")},
		{FloorUVT: dec("150"), TopUVT: dec("360"), BaseUVT: dec("10.45"), Rate: dec("0.28")},
		{FloorUVT: dec("360"), TopUVT: dec("640"), BaseUVT: dec("69.25"), Rate: dec("0.33")},
		{FloorUVT: dec("640"), TopUVT: dec("945"), BaseUVT: dec("161.65"), Rate: dec("0.35")},
		{FloorUVT: dec("945"), TopUVT: dec("2300"), BaseUVT: dec("268.4"), Rate: dec("0.37")},
		{FloorUVT: dec("2300"), TopUVT: decimal.Zero, BaseUVT: dec("769.75"), Rate: dec("0.39")},
	}

	coPensionRate = dec("0.04")
	coSaludRate   = dec("0.04")
	coFSPMinSMLMV = decimal.NewFromInt(4)  // FSP 1% activates ≥ 4 SMLMV
	coFSPMaxSMLMV = decimal.NewFromInt(16) // FSP +1% over 16 SMLMV
	coFSPRate1    = dec("0.01")
	coFSPRate2    = dec("0.01")
)

// ComputeWithholding emits CO_PENSION, CO_SALUD, CO_FSP (when
// activated by salary), and CO_RETENCION. Pensión / Salud apply
// to the IBC capped at 25 SMLMV; the retención base is gross
// minus pensión/salud minus the 25% renta exenta.
//
// Negative / zero gross returns nil.
func (coPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// IBC for Pensión / Salud — capped at 25 SMLMV.
	ibc := gross
	maxIBC := coSMLMV2025.Mul(decimal.NewFromInt(25))
	if ibc.GreaterThan(maxIBC) {
		ibc = maxIBC
	}
	pension := ibc.Mul(coPensionRate).Round(2)
	salud := ibc.Mul(coSaludRate).Round(2)
	if pension.IsPositive() {
		out = append(out, Deduction{Code: "CO_PENSION", Name: "Aporte a Pensión (empleado)", Amount: pension})
	}
	if salud.IsPositive() {
		out = append(out, Deduction{Code: "CO_SALUD", Name: "Aporte a Salud (empleado)", Amount: salud})
	}

	// FSP — 1% over 4 SMLMV, +1% over 16 SMLMV.
	salaryInSMLMV := gross.Div(coSMLMV2025)
	fsp := decimal.Zero
	if salaryInSMLMV.GreaterThanOrEqual(coFSPMinSMLMV) {
		fsp = fsp.Add(ibc.Mul(coFSPRate1))
	}
	if salaryInSMLMV.GreaterThanOrEqual(coFSPMaxSMLMV) {
		fsp = fsp.Add(ibc.Mul(coFSPRate2))
	}
	fsp = fsp.Round(2)
	if fsp.IsPositive() {
		out = append(out, Deduction{Code: "CO_FSP", Name: "Fondo de Solidaridad Pensional", Amount: fsp})
	}

	// Retención en la fuente — Art. 383 ET.
	// Base = gross - aportes obligatorios - 25% renta exenta
	//        (with a cap at 790 UVT/year ≈ 65.83 UVT/month).
	rentaExenta := gross.Sub(pension).Sub(salud).Sub(fsp).Mul(dec("0.25"))
	exemptCapMonthly := coUVT2025.Mul(dec("65.83"))
	if rentaExenta.GreaterThan(exemptCapMonthly) {
		rentaExenta = exemptCapMonthly
	}
	taxableCOP := gross.Sub(pension).Sub(salud).Sub(fsp).Sub(rentaExenta)
	if taxableCOP.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	taxableUVT := taxableCOP.Div(coUVT2025)
	taxUVT := walkCOBrackets(taxableUVT, coRetencionBrackets)
	retencion := taxUVT.Mul(coUVT2025).Round(2)
	if retencion.IsPositive() {
		out = append(out, Deduction{
			Code:   "CO_RETENCION",
			Name:   "Retención en la fuente (rentas de trabajo)",
			Amount: retencion,
		})
	}
	return out, nil
}

// walkCOBrackets walks the Art. 383 ET schedule expressed in UVT.
func walkCOBrackets(uvt decimal.Decimal, scale []coBracket) decimal.Decimal {
	if uvt.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match coBracket
	matched := false
	for _, b := range scale {
		if uvt.LessThanOrEqual(b.FloorUVT) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.BaseUVT.Add(uvt.Sub(match.FloorUVT).Mul(match.Rate))
}
