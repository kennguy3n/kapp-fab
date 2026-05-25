package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// pyPack implements Paraguay's monthly payroll withholding:
// IRP (Impuesto a la Renta Personal — Ley 6.380/2019 Libro V)
// and IPS (Instituto de Previsión Social, 9% employee).
//
// IRP — payable only when annual taxable income exceeds the
// statutory threshold (80 jornales mínimos legales ≈ PYG 9.4M
// for 2025; updated each year by Subsecretaría de Estado de
// Tributación / DNIT). Above the threshold the marginal rates
// are 8% and 10% across two annual bands. Most employees fall
// below the threshold and owe no IRP.
//
// IPS — Ley 98/92 Art. 3 + Ley 1885/02: 9% employee on
// remuneración computable. No annual ceiling on contributions.
//
// 2025 jornales mínimos legales = PYG 117,991 × 30 = PYG
// 3,539,730 / mes (Resolución 600/2024 — MTESS). The IRP
// threshold (80 jornales) ≈ PYG 9,439,280/year ≈ PYG
// 786,607/mes.
//
// References:
//
//	Ley 6.380/2019 (Modernización Tributaria) Art. 47–52:
//	  https://www.dnit.gov.py/legislacion/ley-6380-de-2019
//	DNIT — Tablas IRP 2025:
//	  https://www.dnit.gov.py/web/portal-institucional/imp-personal
//	Resolución MTESS 600/2024 (salario mínimo 2025):
//	  https://www.mtess.gov.py/
type pyPack struct{}

func init() { Register(&pyPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (pyPack) Country() string { return "PY" }

// EffectiveYear pins the fiscal year the PY tables are calibrated
// for: 2025 (DNIT IRP scale + jornal mínimo PYG 117,991 from
// Ministerio de Trabajo + IPS 9% cuota laboral). Bumps move on
// each jornal mínimo revision (typically April).
func (pyPack) EffectiveYear() int { return 2025 }

var (
	pyJornalMinimo  = dec("117991") // 2025 jornal mínimo legal
	pyIRPThreshold0 = decimal.NewFromInt(80)
	pyIRPThreshold1 = decimal.NewFromInt(120)

	pyIRPRate1 = dec("0.08")
	pyIRPRate2 = dec("0.10")

	pyIPSEmployeeRate = dec("0.09")
	pyAnnualDays      = decimal.NewFromFloat(365.25)
)

// ComputeWithholding implements TaxPack for Paraguay. Order: IPS
// (9% on raw gross) → IRP on annualised gross when it exceeds 80
// jornales mínimos mensuales (8% on 80–120 jornales, 10% above
// 120), prorated back to the pay-period (365.25-day year).
// Below the 80-jornal threshold (most employees) only IPS is
// withheld. Multi-tier is closed-form (no bracket walk).
func (pyPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	ips := gross.Mul(pyIPSEmployeeRate).Round(2)
	if ips.IsPositive() {
		out = append(out, Deduction{Code: "PY_IPS", Name: "IPS (cuota laboral, 9%)", Amount: ips})
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(pyAnnualDays)
	if !periodFraction.IsPositive() {
		return out, nil
	}
	annualGross := gross.Div(periodFraction)
	// IRP thresholds in PYG.
	thr0 := pyJornalMinimo.Mul(pyIRPThreshold0).Mul(decimal.NewFromInt(30))
	thr1 := pyJornalMinimo.Mul(pyIRPThreshold1).Mul(decimal.NewFromInt(30))

	if annualGross.LessThanOrEqual(thr0) {
		return out, nil
	}
	// Tier-1 (80–120 jornales, 8%) is the default; tier-2 swaps in
	// the closed-form sum of the saturated tier-1 segment plus 10%
	// on the excess above 120 jornales.
	annualTax := annualGross.Sub(thr0).Mul(pyIRPRate1)
	if annualGross.GreaterThan(thr1) {
		annualTax = thr1.Sub(thr0).Mul(pyIRPRate1).Add(annualGross.Sub(thr1).Mul(pyIRPRate2))
	}
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{Code: "PY_IRP", Name: "IRP (Impuesto a la Renta Personal)", Amount: periodTax})
	}
	return out, nil
}
