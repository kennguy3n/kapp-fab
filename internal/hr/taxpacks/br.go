package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// brPack implements Brazil's monthly federal payroll withholding:
// IRRF (income tax) and INSS (social security, employee share).
// FGTS (8% employer-side) is explicitly out of scope for the
// employee withholding pipeline — it is not deducted from the
// employee's gross.
//
// IRRF (Imposto de Renda Retido na Fonte) — Lei 11.482/2007 with
// the May/2023 reform (Medida Provisória 1.171/2023, converted by
// Lei 14.663/2023). The schedule is monthly, progressive across
// five brackets, with a "parcela a deduzir" (fixed deduction) per
// bracket that linearises the cumulative-tax expression. The
// statutory subtraction order is:
//
//	taxable_irrf = gross - INSS_employee - (R$189.59 × dependents)
//
// The pack subtracts INSS *before* the IRRF bracket walk so the
// withholding base matches Receita Federal's official IR-Fonte
// table. The post-2023 schedule additionally allows a "desconto
// simplificado" of 25% of gross capped at R$564.80/month for
// employees who do not itemise — this pack does NOT apply the
// simplified discount automatically because it is an employee
// election (declared on the tax form) rather than a payroll-side
// default; the year-end DIRPF reconciles either way.
//
// INSS — Decreto 3.048/1999 with the 2024 / 2025 SGT bulletins.
// Four progressive bands with a hard ceiling at R$8,157.41 (2025,
// "teto previdenciário") — once gross exceeds the ceiling the
// employee INSS is capped at the band-walk value at the ceiling
// (≈ R$951.62 / month at the 2024 schedule; the 2025 ceiling +
// rates update the cap in lock-step).
//
// References:
//
//	Lei 14.663/2023 (IRRF table effective from May/2023):
//	  https://www.planalto.gov.br/ccivil_03/_ato2023-2026/2023/lei/L14663.htm
//	IN RFB 2.141/2023 (current IRRF practitioner guidance):
//	  https://normas.receita.fazenda.gov.br/sijut2consulta/link.action?idAto=132340
//	Receita Federal "Tabela do IRRF" current schedule:
//	  https://www.gov.br/receitafederal/pt-br/assuntos/meu-imposto-de-renda/tabelas/2024
//	INSS 2025 employee contribution table (Portaria
//	  Interministerial MPS/MF nº 6, de 10/01/2025):
//	  https://www.gov.br/inss/pt-br/assuntos/tabela-de-contribuicao-mensal
//
// EffectiveYear pins the fiscal year the tables track: 2025.
type brPack struct{}

func init() { Register(&brPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code.
func (brPack) Country() string { return "BR" }

// EffectiveYear pins the fiscal year of the bracket tables (2025).
func (brPack) EffectiveYear() int { return 2025 }

// brBracket is one row of an IRRF (or INSS) progressive table.
// Floor / Top are monthly BRL income; Base is the cumulative
// withholding at Floor (the published "parcela a deduzir" is
// equivalent — see brIRRFParcela below); Rate is the marginal
// rate inside the band. Top == 0 marks the open-ended top band.
type brBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// IRRF 2024/2025 monthly schedule (Lei 14.663/2023). Floors
	// reflect the May/2024 Medida Provisória bump that raised the
	// exempt threshold to R$2,259.20.
	//
	// The Receita Federal table publishes "parcela a deduzir"
	// (the fixed amount subtracted from rate × income to land on
	// the cumulative tax) — those values are equivalent to the
	// Base field here under the identity:
	//
	//   tax = Rate × income - Parcela
	//        = Base + Rate × (income - Floor)
	//   ⇒ Parcela = Rate × Floor - Base
	//
	// So the published parcelas (0 / 169.44 / 381.44 / 662.77 /
	// 896.00) match the Base values below. The bracket-walk uses
	// Base directly; the parcela cross-check pins the schedule.
	brIRRFBrackets = []brBracket{
		{Floor: dec("0"), Top: dec("2259.20"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("2259.20"), Top: dec("2826.65"), Base: dec("0"), Rate: dec("0.075")},
		{Floor: dec("2826.65"), Top: dec("3751.05"), Base: dec("42.55875"), Rate: dec("0.15")},
		{Floor: dec("3751.05"), Top: dec("4664.68"), Base: dec("181.21875"), Rate: dec("0.225")},
		{Floor: dec("4664.68"), Top: decimal.Zero, Base: dec("386.78550"), Rate: dec("0.275")},
	}

	// Per-dependent monthly deduction subtracted from taxable
	// gross before the IRRF bracket walk. Lei 9.250/1995 art. 8º
	// + IN RFB 2.141/2023; updated annually.
	brIRRFDependentDeduction = dec("189.59")

	// INSS 2025 monthly progressive schedule (Portaria MPS/MF nº
	// 6/2025). Four progressive bands. The "Top" of the last
	// band is the teto previdenciário (R$8,157.41); above the
	// ceiling no further INSS is owed, the employee's INSS is
	// capped at the band-walk value at the ceiling.
	brINSSBrackets = []brBracket{
		{Floor: dec("0"), Top: dec("1518"), Base: dec("0"), Rate: dec("0.075")},
		{Floor: dec("1518"), Top: dec("2793.88"), Base: dec("113.85"), Rate: dec("0.09")},
		{Floor: dec("2793.88"), Top: dec("4190.83"), Base: dec("228.6792"), Rate: dec("0.12")},
		{Floor: dec("4190.83"), Top: dec("8157.41"), Base: dec("396.3132"), Rate: dec("0.14")},
	}

	// INSS ceiling — monthly gross above this point pays no
	// further employee INSS (a contribution above the ceiling is
	// "facultativa" and not part of CLT payroll). The pack
	// computes the ceiling-cap value as the band walk at the
	// ceiling itself (≈ R$951.6353), so a slip with gross above
	// the ceiling withholds exactly that amount.
	brINSSCeiling = dec("8157.41")
)

// ComputeWithholding emits BR_INSS and BR_IRRF. INSS is computed
// first; IRRF base is gross - INSS - per-dependent allowance.
//
// Negative / zero gross returns nil. The schedule is monthly so
// the engine's days/365.25 prorate is not applied — Receita
// Federal's tables are monthly fixed-step amounts.
func (brPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// INSS first — its result feeds the IRRF base.
	inss := brComputeINSS(gross).Round(2)
	if inss.IsPositive() {
		out = append(out, Deduction{
			Code:   "BR_INSS",
			Name:   "INSS (previdência social, empregado)",
			Amount: inss,
		})
	}

	// IRRF — subtract INSS and the per-dependent allowance, then
	// run the bracket walk.
	dependents := decimal.NewFromInt(int64(e.NumDependents))
	base := gross.Sub(inss).Sub(dependents.Mul(brIRRFDependentDeduction))
	if base.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	irrf := walkBRBrackets(base, brIRRFBrackets).Round(2)
	if irrf.IsPositive() {
		out = append(out, Deduction{
			Code:   "BR_IRRF",
			Name:   "IRRF (imposto de renda retido na fonte)",
			Amount: irrf,
		})
	}
	return out, nil
}

// brComputeINSS computes the employee INSS on monthly gross. The
// schedule is progressive (each band of gross is taxed at its
// own rate); above the ceiling the value is fixed at the
// band-walk evaluated at the ceiling.
func brComputeINSS(gross decimal.Decimal) decimal.Decimal {
	if gross.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	subject := gross
	if subject.GreaterThan(brINSSCeiling) {
		subject = brINSSCeiling
	}
	return walkBRBrackets(subject, brINSSBrackets)
}

// walkBRBrackets walks a brBracket schedule. Same contract as
// walkCABrackets — Floor-matched base + (income - Floor) × Rate.
// Below the first floor yields zero. Last bracket with Top == 0
// is open-ended (used by IRRF only; INSS is hard-capped via
// brComputeINSS's pre-clamp).
func walkBRBrackets(monthly decimal.Decimal, scale []brBracket) decimal.Decimal {
	if monthly.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match brBracket
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
