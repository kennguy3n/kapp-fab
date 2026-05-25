package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// mxPack implements Mexico's monthly federal payroll withholding:
// ISR (Impuesto Sobre la Renta — income tax retention per LISR
// Art. 96), the Subsidio para el Empleo (monthly employment
// subsidy that offsets ISR for low earners — Decreto del Ejecutivo
// of 11/05/2024), and IMSS (the employee share of social security
// across the three insured branches that withhold from the worker).
//
// SAR/Infonavit are employer-only and intentionally out of scope.
//
// The schedule is monthly, progressive across 11 brackets, with a
// "cuota fija" (fixed amount) per bracket plus a marginal rate on
// the excess over the bracket floor. After computing ISR the
// employment subsidy is subtracted; the result is clamped at zero
// (a net subsidy returns to the worker on the year-end ajuste
// anual or via the patron's payroll, but at slip time it shows as
// zero withholding rather than a negative line).
//
// IMSS employee share is calibrated from LSS Arts. 25, 106, 147,
// 168, 199 using the 2025 UMA daily floor (UMA = MXN 113.07/day,
// updated annually by INEGI). The employee total is approximately
// 2.775% of integrated daily wage (SDI) for the most common
// regimes; capped at 25× UMA daily (the maximum SDI). The pack
// applies the aggregated employee rate as a single line — IMSS
// patron filings break the line into per-branch sub-amounts but
// at the payroll-slip level the worker sees one IMSS row.
//
// References:
//
//	LISR Art. 96 (ISR mensual schedule):
//	  https://www.diputados.gob.mx/LeyesBiblio/pdf/LISR.pdf
//	Decreto Subsidio para el Empleo (DOF 11-05-2024):
//	  https://www.dof.gob.mx/nota_detalle.php?codigo=5723847
//	SAT — Tablas mensuales 2024/2025 (ISR + subsidio):
//	  https://www.sat.gob.mx/articulo/05783/tablas-mensuales-aplicables-en-2024
//	IMSS — Cuotas obrero-patronales (2025):
//	  https://www.imss.gob.mx/patrones/incorporacion/cuotas-y-aportaciones
//	UMA 2025 (INEGI):
//	  https://www.inegi.org.mx/temas/uma/
type mxPack struct{}

func init() { Register(&mxPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code.
func (mxPack) Country() string { return "MX" }

// EffectiveYear pins the fiscal year the tables track: 2025.
func (mxPack) EffectiveYear() int { return 2025 }

type mxBracket struct {
	Floor    decimal.Decimal
	Top      decimal.Decimal
	CuotaFij decimal.Decimal // "cuota fija" (cumulative tax at floor)
	Rate     decimal.Decimal
}

var (
	// LISR Art. 96 (DOF 27-12-2021 + 2025 indexation). 11
	// progressive brackets. Floors / Tops in monthly MXN; CuotaFij
	// is the fixed cumulative tax at the bracket floor; Rate is
	// the marginal rate over the excess.
	mxISRBrackets = []mxBracket{
		{Floor: dec("0.01"), Top: dec("746.04"), CuotaFij: dec("0"), Rate: dec("0.0192")},
		{Floor: dec("746.04"), Top: dec("6332.05"), CuotaFij: dec("14.32"), Rate: dec("0.0640")},
		{Floor: dec("6332.05"), Top: dec("11128.01"), CuotaFij: dec("371.83"), Rate: dec("0.1088")},
		{Floor: dec("11128.01"), Top: dec("12935.82"), CuotaFij: dec("893.63"), Rate: dec("0.16")},
		{Floor: dec("12935.82"), Top: dec("15487.71"), CuotaFij: dec("1182.88"), Rate: dec("0.1792")},
		{Floor: dec("15487.71"), Top: dec("31236.49"), CuotaFij: dec("1640.18"), Rate: dec("0.2136")},
		{Floor: dec("31236.49"), Top: dec("49233.00"), CuotaFij: dec("5004.12"), Rate: dec("0.2352")},
		{Floor: dec("49233.00"), Top: dec("93993.90"), CuotaFij: dec("9236.89"), Rate: dec("0.30")},
		{Floor: dec("93993.90"), Top: dec("125325.20"), CuotaFij: dec("22665.17"), Rate: dec("0.32")},
		{Floor: dec("125325.20"), Top: dec("375975.61"), CuotaFij: dec("32691.18"), Rate: dec("0.34")},
		{Floor: dec("375975.61"), Top: decimal.Zero, CuotaFij: dec("117912.32"), Rate: dec("0.35")},
	}

	// Subsidio para el Empleo (Decreto DOF 11-05-2024). Single-
	// table monthly: 13.8% of one UMA mensual (≈ MXN 3,439 in
	// 2025), credited per slip; phases out above MXN 10,171/mo.
	// The pack uses the published "tabla unificada":
	//   - gross ≤ 10,171.00 → subsidy = 475.00
	//   - gross > 10,171.00 → subsidy = 0
	// The simple step covers >95% of subsidy-eligible workers;
	// the older multi-tier schedule was collapsed into this
	// single-step formulation by the 2024 decreto.
	mxSubsidioStepThreshold = dec("10171.00")
	mxSubsidioStepAmount    = dec("475.00")

	// IMSS employee aggregated rate (LSS Arts. 106 / 168 / 199):
	//   Enfermedad y maternidad — fixed cuota on excess >3× UMA:
	//     0.40% (employee, gastos médicos pensionados)
	//   Invalidez y vida: 0.625%
	//   Cesantía y vejez: 1.125%
	//   Total employee:   ≈ 2.775%
	// The pack uses the aggregated rate to avoid four per-branch
	// lines on the slip; IMSS SUA reporting (employer side)
	// breaks the cuota by branch.
	mxIMSSEmployeeRate = dec("0.02775")

	// IMSS upper-cap base: 25× UMA daily (the maximum SDI). 25 ×
	// 113.07 = 2826.75/day; monthly cap ≈ 2826.75 × 30.4 ≈
	// 85,933.20. Above this gross, IMSS is capped at the rate ×
	// cap product.
	mxIMSSMonthlyCap = dec("85933.20")
)

// ComputeWithholding emits MX_ISR (post-subsidio, clamped) and
// MX_IMSS for monthly slips. Negative / zero gross returns nil.
func (mxPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// ISR — bracket walk.
	isr := walkMXBrackets(gross, mxISRBrackets)
	// Subsidio para el Empleo — applied as a credit against ISR.
	subsidio := decimal.Zero
	if gross.LessThanOrEqual(mxSubsidioStepThreshold) {
		subsidio = mxSubsidioStepAmount
	}
	netISR := isr.Sub(subsidio).Round(2)
	if netISR.LessThan(decimal.Zero) {
		// Statutory: when subsidy exceeds ISR the difference is
		// paid back to the worker (cash on payroll). Most ERP
		// systems emit a separate "subsidio entregado" line; this
		// pack keeps the slip simple by clamping at zero — the
		// year-end ajuste anual reconciles the small refund. A
		// future "MX_SUBSIDIO_NETO" line could expose the credit
		// explicitly when DEDUCTION_ACCOUNT_MAP wiring lands.
		netISR = decimal.Zero
	}
	if netISR.IsPositive() {
		out = append(out, Deduction{
			Code:   "MX_ISR",
			Name:   "ISR (impuesto sobre la renta, retención mensual)",
			Amount: netISR,
		})
	}

	// IMSS — employee share, capped at 25× UMA daily.
	imssBase := gross
	if imssBase.GreaterThan(mxIMSSMonthlyCap) {
		imssBase = mxIMSSMonthlyCap
	}
	if imss := imssBase.Mul(mxIMSSEmployeeRate).Round(2); imss.IsPositive() {
		out = append(out, Deduction{
			Code:   "MX_IMSS",
			Name:   "IMSS (cuota obrera, seguridad social)",
			Amount: imss,
		})
	}

	return out, nil
}

// walkMXBrackets walks the LISR Art. 96 monthly schedule. Returns
// CuotaFija + (gross - Floor) × Rate for the matched bracket;
// below the first floor yields zero.
func walkMXBrackets(gross decimal.Decimal, scale []mxBracket) decimal.Decimal {
	if gross.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match mxBracket
	matched := false
	for _, b := range scale {
		if gross.LessThan(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.CuotaFij.Add(gross.Sub(match.Floor).Mul(match.Rate))
}
