package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// gtPack implements Guatemala's monthly payroll withholding:
// ISR (Impuesto Sobre la Renta — Decreto 10-2012 / Libro I "Renta
// del Trabajo en Relación de Dependencia") and IGSS (Instituto
// Guatemalteco de Seguridad Social, 4.83% employee).
//
// ISR — annual progressive schedule with two brackets:
//   0 – 300,000        → 5% on excess over GTQ 48,000 exempt
//                        deduction (single rate after 48k)
//   > 300,000          → 7% on excess + GTQ 15,000
// where 15,000 = (300,000 - 48,000) × 5% + 2,400 ≈ effective base
//
// Actually the published Decreto 10-2012 Art. 73 schedule is:
//   0 – 300,000        → 5% on Renta Imponible
//   > 300,000          → 7% on excess + GTQ 15,000
// where Renta Imponible = Renta Bruta − (GTQ 48,000 + IGSS).
//
// IGSS — Acuerdo 1431 JD: 4.83% empleado (IVS 4.83% — IVS
// 1.83% + EMA 3.00%). No annual ceiling on contributions.
//
// References:
//
//	Decreto 10-2012, Libro I Título IV:
//	  https://leyes.infile.com/index.php?id=181&id_publicacion=64498
//	SAT — Cálculo de ISR sobre Renta del Trabajo:
//	  https://portal.sat.gob.gt/portal/impuestos/impuesto-sobre-la-renta/
//	IGSS — Tabla de cotizaciones (Acuerdo 1431):
//	  https://www.igssgt.org/empleadores/cuotas
type gtPack struct{}

func init() { Register(&gtPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (gtPack) Country() string { return "GT" }

// EffectiveYear pins the fiscal year the GT tables are calibrated
// for: 2025 (Ley del ISR Decreto 10-2012, Régimen Sobre Utilidades
// + IGSS 4.83% cuota laboral). Rates here are very stable; bumps
// only happen if Congreso passes a reform.
func (gtPack) EffectiveYear() int { return 2025 }

var (
	gtISRExempt      = dec("48000")
	gtISRThreshold1  = dec("300000")
	gtISRRate1       = dec("0.05")
	gtISRRate2       = dec("0.07")
	gtISRBaseTier2   = dec("15000") // 300,000 × 5%

	gtIGSSRate   = dec("0.0483")
	gtAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding implements TaxPack for Guatemala. Order:
// IGSS (4.83% on raw gross, no cap) → ISR Régimen Sobre
// Utilidades on annualised (gross − IGSS − Q48,000 exempt),
// applied as 5% on the first Q300,000 of renta imponible and 7%
// above, then prorated back to the pay-period (365.25-day year).
// The lower-tier 5% case is the default; the high-tier branch
// reuses the Q15,000 pre-computed base (= Q300,000 × 5%).
func (gtPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	igss := gross.Mul(gtIGSSRate).Round(2)
	if igss.IsPositive() {
		out = append(out, Deduction{Code: "GT_IGSS", Name: "IGSS (cuota laboral, 4.83%)", Amount: igss})
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(gtAnnualDays)
	if !periodFraction.IsPositive() {
		return out, nil
	}
	// Annual renta bruta projected from slip.
	annualGross := gross.Div(periodFraction)
	annualIGSS := igss.Div(periodFraction)
	rentaImponible := annualGross.Sub(gtISRExempt).Sub(annualIGSS)
	if rentaImponible.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	// First-tier 5% is the default; high-tier branch swaps in the
	// pre-computed Q15,000 base + 7% on the excess above Q300,000.
	annualTax := rentaImponible.Mul(gtISRRate1)
	if rentaImponible.GreaterThan(gtISRThreshold1) {
		annualTax = gtISRBaseTier2.Add(rentaImponible.Sub(gtISRThreshold1).Mul(gtISRRate2))
	}
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{Code: "GT_ISR", Name: "ISR (Renta del Trabajo, dependencia)", Amount: periodTax})
	}
	return out, nil
}
