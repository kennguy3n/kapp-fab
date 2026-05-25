package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// ptPack implements Portugal's payroll-side statutory
// withholdings for the 2025 fiscal year:
//
//   - IRS (Imposto sobre o Rendimento das Pessoas Singulares)
//     withholding (retenção na fonte) using the 2025 tabelas de
//     retenção. The published tables are by monthly gross
//     range; this pack uses the underlying progressive nine-
//     band IRS schedule (the State Budget Law 2025 retained the
//     post-2024 nine-band structure introduced in OE2024) and
//     applies a personal credit equivalent to the dedução
//     específica of €4,350 / yr. Marginal rates 2025 (single):
//     0       → 8,059    13.00%
//     8,059   → 12,160   16.50%
//     12,160 → 17,233    22.00%
//     17,233 → 22,306    25.00%
//     22,306 → 28,400    32.00%
//     28,400 → 41,629    35.50%
//     41,629 → 44,987    43.50%
//     44,987 → 83,696    45.00%
//     > 83,696           48.00%
//
//   - Segurança Social employee share: 11.0% on the gross. No
//     cap (Portugal does not apply an annual ceiling to the
//     employee contribution).
//
//   - Sobretaxa de solidariedade: an additional 2.5% / 5% on
//     taxable income above €80,000 / €250,000 respectively.
//     Implemented as marginal surcharges on annualised gross.
//
// References:
//
//	Autoridade Tributária — Tabelas IRS 2025:
//	  https://info.portaldasfinancas.gov.pt/pt/informacao_fiscal/codigos_tributarios/cirs_rep/Pages/cirs_rep.aspx
//	Segurança Social cotizações 2025:
//	  https://www.seg-social.pt/taxas-contributivas
type ptPack struct{}

func init() { Register(&ptPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (ptPack) Country() string { return "PT" }

// EffectiveYear returns the fiscal year the PT tables are
// calibrated for: 2025 (OE 2025 IRS brackets + Segurança Social
// 2025 + sobretaxa).
func (ptPack) EffectiveYear() int { return 2025 }

type ptBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	ptIRSBrackets = []ptBracket{
		{Floor: dec("0"), Top: dec("8059"), Base: dec("0"), Rate: dec("0.13")},
		{Floor: dec("8059"), Top: dec("12160"), Base: dec("1047.67"), Rate: dec("0.165")},
		{Floor: dec("12160"), Top: dec("17233"), Base: dec("1724.34"), Rate: dec("0.22")},
		{Floor: dec("17233"), Top: dec("22306"), Base: dec("2840.40"), Rate: dec("0.25")},
		{Floor: dec("22306"), Top: dec("28400"), Base: dec("4108.65"), Rate: dec("0.32")},
		{Floor: dec("28400"), Top: dec("41629"), Base: dec("6058.73"), Rate: dec("0.355")},
		{Floor: dec("41629"), Top: dec("44987"), Base: dec("10755.02"), Rate: dec("0.435")},
		{Floor: dec("44987"), Top: dec("83696"), Base: dec("12215.75"), Rate: dec("0.45")},
		{Floor: dec("83696"), Top: decimal.Zero, Base: dec("29634.80"), Rate: dec("0.48")},
	}

	// Dedução específica (fixed employment-income credit, 2025).
	ptDeducaoEspecifica = dec("4350")

	ptSSRate = dec("0.11") // employee Segurança Social share

	// Sobretaxa de solidariedade (annual income thresholds).
	ptSobretaxaT1 = dec("80000")
	ptSobretaxaT2 = dec("250000")
	ptSobretaxaR1 = dec("0.025")
	ptSobretaxaR2 = dec("0.05")

	ptAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to three lines:
//
//   - PT_IRS           (IRS retention per AT tabela mensal, monthly
//     brackets with the base + marginal-rate-on-excess pattern)
//   - PT_SOBRETAXA     (Sobretaxa de Solidariedade above the €80k
//     annual threshold, at 2.5% / 5% per band)
//   - PT_SEG_SOCIAL    (Segurança Social employee share at 11%)
//
// Negative or zero gross / period return nil.
func (ptPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(ptAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// IRS — bracket walk on annual gross minus dedução
	// específica.
	taxable := annualGross.Sub(ptDeducaoEspecifica)
	if taxable.LessThan(decimal.Zero) {
		taxable = decimal.Zero
	}
	annualIRS := walkPTBrackets(taxable, ptIRSBrackets)

	// Sobretaxa de solidariedade — marginal surcharge above
	// €80k / €250k.
	annualIRS = annualIRS.Add(ptComputeSobretaxa(annualGross))

	periodIRS := annualIRS.Mul(periodFraction).Round(2)
	if periodIRS.IsPositive() {
		out = append(out, Deduction{
			Code:   "PT_IRS",
			Name:   "IRS (PT)",
			Amount: periodIRS,
		})
	}

	// Segurança Social — 11% on gross, no cap.
	if ss := gross.Mul(ptSSRate).Round(2); ss.IsPositive() {
		out = append(out, Deduction{
			Code:   "PT_SS",
			Name:   "Segurança Social (employee, PT)",
			Amount: ss,
		})
	}

	return out, nil
}

// ptComputeSobretaxa applies the 2.5% / 5% solidarity surcharge.
func ptComputeSobretaxa(annual decimal.Decimal) decimal.Decimal {
	if annual.LessThanOrEqual(ptSobretaxaT1) {
		return decimal.Zero
	}
	if annual.LessThanOrEqual(ptSobretaxaT2) {
		return annual.Sub(ptSobretaxaT1).Mul(ptSobretaxaR1)
	}
	first := ptSobretaxaT2.Sub(ptSobretaxaT1).Mul(ptSobretaxaR1)
	second := annual.Sub(ptSobretaxaT2).Mul(ptSobretaxaR2)
	return first.Add(second)
}

// walkPTBrackets walks the IRS schedule.
func walkPTBrackets(taxable decimal.Decimal, brackets []ptBracket) decimal.Decimal {
	var match ptBracket
	matched := false
	for _, b := range brackets {
		if taxable.LessThanOrEqual(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.Base.Add(taxable.Sub(match.Floor).Mul(match.Rate))
}
