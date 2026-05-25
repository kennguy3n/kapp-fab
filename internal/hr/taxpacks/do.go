package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// doPack implements the Dominican Republic's monthly payroll
// withholding: ISR (Impuesto Sobre la Renta, DGII Art. 296 CT),
// AFP (Sistema Dominicano de Pensiones, 2.87% employee), and SFS
// (Seguro Familiar de Salud, 3.04% employee).
//
// ISR — annual progressive schedule (Código Tributario Art. 296),
// indexed annually for inflation. The schedule as of 2025:
//   0 – 416,220.00          → 0%
//   416,220.01 – 624,329.00 → 15% on excess
//   624,329.01 – 867,123.00 → 20% on excess + RD$31,216.35
//   > 867,123.00            → 25% on excess + RD$79,776.15
// Monthly withholding = annual / 12. AFP / SFS are subtracted
// from gross before the ISR bracket walk because they are
// statutory exclusions (Norma 02-2024).
//
// AFP (Ley 87-01 Art. 18): 2.87% employee share. Capped at
// 20× SMN — currently RD$25,800 × 20 = 516,000/mo.
// SFS (Ley 87-01 Art. 105): 3.04% employee share. Capped at
// 10× SMN = 258,000/mo.
//
// References:
//
//	Código Tributario Art. 296 (escala ISR):
//	  https://dgii.gov.do/legislacion/codigoTributario
//	DGII Norma General 02-2024 (escala ISR 2025):
//	  https://dgii.gov.do/legislacion/normas
//	SIPEN — Tablas de cotización 2025:
//	  https://www.sipen.gob.do/transparencia/normativa
type doPack struct{}

func init() { Register(&doPack{}) }

func (doPack) Country() string  { return "DO" }
func (doPack) EffectiveYear() int { return 2025 }

var (
	doISRThreshold0 = dec("416220")
	doISRThreshold1 = dec("624329")
	doISRThreshold2 = dec("867123")

	doISRRate1 = dec("0.15")
	doISRRate2 = dec("0.20")
	doISRRate3 = dec("0.25")

	doISRBase2 = dec("31216.35") // 15% × (624329 - 416220)
	doISRBase3 = dec("79775.15") // base2 + 20% × (867123 - 624329) = 31216.35 + 48558.80

	doAFPRate    = dec("0.0287")
	doSFSRate    = dec("0.0304")
	doSMN        = dec("25800") // 2025 salario mínimo nacional (DR public sector reference)
	doAFPCapMult = decimal.NewFromInt(20)
	doSFSCapMult = decimal.NewFromInt(10)
	doAnnualDays = decimal.NewFromFloat(365.25)
)

func (doPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	afpBase := gross
	afpCap := doSMN.Mul(doAFPCapMult)
	if afpBase.GreaterThan(afpCap) {
		afpBase = afpCap
	}
	afp := afpBase.Mul(doAFPRate).Round(2)
	if afp.IsPositive() {
		out = append(out, Deduction{Code: "DO_AFP", Name: "AFP — Aporte personal a pensiones", Amount: afp})
	}

	sfsBase := gross
	sfsCap := doSMN.Mul(doSFSCapMult)
	if sfsBase.GreaterThan(sfsCap) {
		sfsBase = sfsCap
	}
	sfs := sfsBase.Mul(doSFSRate).Round(2)
	if sfs.IsPositive() {
		out = append(out, Deduction{Code: "DO_SFS", Name: "SFS — Seguro Familiar de Salud", Amount: sfs})
	}

	// ISR — gross less AFP / SFS, annualised, walked, prorated.
	periodFraction := decimal.NewFromInt(int64(days)).Div(doAnnualDays)
	if !periodFraction.IsPositive() {
		return out, nil
	}
	netMonthly := gross.Sub(afp).Sub(sfs)
	if netMonthly.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	annualNet := netMonthly.Div(periodFraction)
	annualTax := decimal.Zero
	switch {
	case annualNet.LessThanOrEqual(doISRThreshold0):
		// exempt
	case annualNet.LessThanOrEqual(doISRThreshold1):
		annualTax = annualNet.Sub(doISRThreshold0).Mul(doISRRate1)
	case annualNet.LessThanOrEqual(doISRThreshold2):
		annualTax = doISRBase2.Add(annualNet.Sub(doISRThreshold1).Mul(doISRRate2))
	default:
		annualTax = doISRBase3.Add(annualNet.Sub(doISRThreshold2).Mul(doISRRate3))
	}
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{Code: "DO_ISR", Name: "ISR (Impuesto Sobre la Renta, retención)", Amount: periodTax})
	}
	return out, nil
}
