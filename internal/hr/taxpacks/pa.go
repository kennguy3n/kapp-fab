package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// paPack implements Panama's monthly payroll withholding: ISR
// (Impuesto Sobre la Renta — Ley 8/2010 Art. 700), CSS (Caja de
// Seguro Social), and the Seguro Educativo (1.25% employee).
//
// ISR — Ley 8/2010 Art. 700, ANNUAL progressive schedule:
//   0 – 11,000     → 0%
//   11,000 – 50,000→ 15% on excess over 11,000
//   > 50,000       → 25% on excess + flat USD 5,850
// Monthly withholding = annual / 12 using a "proyección" of slip
// gross × periods/year. This pack uses annualise-via-days/365.25.
//
// CSS — Ley 51/2005 Art. 70 (employee share = 9.75% of gross,
// no annual ceiling; the social-security ceiling for benefits is
// USD 8,500/month but employee contributions continue above it).
// Seguro Educativo — Ley 21/1986 Art. 5 (employee share = 1.25%).
//
// Panama uses PAB (balboa), pegged 1:1 to USD; the pack returns
// amounts in the slip's currency (PAB or USD interchangeably).
//
// References:
//
//	Ley 8/2010 — texto único Código Fiscal:
//	  https://docs.panama.justia.com/federales/leyes/8-de-2010-mar-16-2010.pdf
//	DGI — Tabla de retenciones ISR (vigente):
//	  https://dgi.mef.gob.pa/empleados/calculadora-impuesto-sobre-la-renta
//	CSS — Cuotas obrero-patronales:
//	  https://www.css.gob.pa/empleadores/cuotas
type paPack struct{}

func init() { Register(&paPack{}) }

func (paPack) Country() string  { return "PA" }
func (paPack) EffectiveYear() int { return 2025 }

var (
	paISRThreshold1 = dec("11000")
	paISRThreshold2 = dec("50000")
	paISRRate1      = dec("0.15")
	paISRRate2      = dec("0.25")
	paISRBaseTier2  = dec("5850") // 39000 × 15%

	paCSSEmployeeRate    = dec("0.0975")
	paSeguroEducativoRate = dec("0.0125")
	paAnnualDays         = decimal.NewFromFloat(365.25)
)

func (paPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	if css := gross.Mul(paCSSEmployeeRate).Round(2); css.IsPositive() {
		out = append(out, Deduction{Code: "PA_CSS", Name: "CSS (cuota obrera, seguridad social)", Amount: css})
	}
	if seduc := gross.Mul(paSeguroEducativoRate).Round(2); seduc.IsPositive() {
		out = append(out, Deduction{Code: "PA_SEGURO_EDUCATIVO", Name: "Seguro Educativo (empleado)", Amount: seduc})
	}

	// ISR annual = max(0, (annualGross - 11000) × 15%) up to 50000,
	// then add 25% × (annualGross - 50000) for the upper band.
	periodFraction := decimal.NewFromInt(int64(days)).Div(paAnnualDays)
	if !periodFraction.IsPositive() {
		return out, nil
	}
	annualGross := gross.Div(periodFraction)
	annualTax := decimal.Zero
	switch {
	case annualGross.LessThanOrEqual(paISRThreshold1):
		// exempt
	case annualGross.LessThanOrEqual(paISRThreshold2):
		annualTax = annualGross.Sub(paISRThreshold1).Mul(paISRRate1)
	default:
		annualTax = paISRBaseTier2.Add(annualGross.Sub(paISRThreshold2).Mul(paISRRate2))
	}
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{Code: "PA_ISR", Name: "Impuesto Sobre la Renta (empleado)", Amount: periodTax})
	}
	return out, nil
}
