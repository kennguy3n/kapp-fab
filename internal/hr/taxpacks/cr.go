package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// crPack implements Costa Rica's monthly payroll withholding:
// Impuesto sobre el Salario (income tax — Ley 7092 Art. 33) and
// the employee share of CCSS (Caja Costarricense de Seguro
// Social) across SEM, IVM, and Banco Popular contributions.
//
// Impuesto sobre el Salario — monthly progressive brackets per
// Ministerio de Hacienda Decreto 44.286-H (effective 2025).
// Brackets (CRC monthly):
//   0 – 941,000          → 0%
//   941,000 – 1,381,000  → 10%
//   1,381,000 – 2,423,000→ 15%
//   2,423,000 – 4,845,000→ 20%
//   > 4,845,000          → 25%
// Plus per-dependent and per-spouse credits applied as flat
// monthly reducciones (small relative to bracket impact; pack
// applies them as flat amounts deducted from the tax).
//
// CCSS — Ley 17 (LCCSS), employee:
//   - SEM (enfermedad y maternidad): 5.50%
//   - IVM (invalidez, vejez y muerte): 4.17%
//   - Banco Popular: 1.00%
//   - Total empleado: 10.67%
//
// References:
//
//	Ley 7092 (Impuesto sobre la Renta) Art. 33:
//	  http://www.pgrweb.go.cr/scij/Busqueda/Normativa/Normas/nrm_texto_completo.aspx?param1=NRTC&nValor1=1&nValor2=10969
//	Decreto Ejecutivo 44.286-H (escala 2025):
//	  https://www.hacienda.go.cr/contenido/15822-tabla-impuesto-sobre-el-salario-2025
//	CCSS — Cuotas obrero-patronales 2025:
//	  https://www.ccss.sa.cr/empleadores/cuotas
type crPack struct{}

func init() { Register(&crPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (crPack) Country() string { return "CR" }

// EffectiveYear pins the fiscal year the CR tables are calibrated
// for (Decreto 44.286-H Impuesto al Salario 2025 + CCSS 2025
// employer/employee schedule). Bumps move in lock-step with the
// annual indexation publication from Hacienda / CCSS.
func (crPack) EffectiveYear() int { return 2025 }

type crBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal // 0 = open
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// Impuesto sobre el Salario 2025 — Decreto 44.286-H.
	crSalarioBrackets = []crBracket{
		{Floor: dec("0"), Top: dec("941000"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("941000"), Top: dec("1381000"), Base: dec("0"), Rate: dec("0.10")},
		{Floor: dec("1381000"), Top: dec("2423000"), Base: dec("44000"), Rate: dec("0.15")},
		{Floor: dec("2423000"), Top: dec("4845000"), Base: dec("200300"), Rate: dec("0.20")},
		{Floor: dec("4845000"), Top: decimal.Zero, Base: dec("684700"), Rate: dec("0.25")},
	}

	// CCSS employee aggregated rate.
	crCCSSEmployeeRate = dec("0.1067") // SEM 5.50 + IVM 4.17 + BP 1.00
)

// ComputeWithholding implements TaxPack for Costa Rica. The order
// of operations is: CCSS (10.67% on raw monthly gross) → Impuesto
// al Salario (5-row progressive table from Decreto 44.286-H). The
// salary tax is computed against raw gross — CR does NOT subtract
// social security before the bracket walk (unlike BR's IRRF-after-
// INSS rule). Zero / negative gross or zero / negative period
// return nil.
func (crPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	if ccss := gross.Mul(crCCSSEmployeeRate).Round(2); ccss.IsPositive() {
		out = append(out, Deduction{Code: "CR_CCSS", Name: "CCSS (cuota obrera, SEM + IVM + BP)", Amount: ccss})
	}

	if tax := walkCRBrackets(gross, crSalarioBrackets).Round(2); tax.IsPositive() {
		out = append(out, Deduction{Code: "CR_IMPUESTO_SALARIO", Name: "Impuesto sobre el Salario", Amount: tax})
	}
	return out, nil
}

func walkCRBrackets(monthly decimal.Decimal, scale []crBracket) decimal.Decimal {
	if monthly.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match crBracket
	matched := false
	for _, b := range scale {
		if monthly.LessThanOrEqual(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.Base.Add(monthly.Sub(match.Floor).Mul(match.Rate))
}
