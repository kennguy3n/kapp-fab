package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// esPack implements Spain's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - IRPF (Impuesto sobre la Renta de las Personas Físicas):
//     progressive withholding on annual salary, applying the
//     2025 state + average autonomic brackets. The state-only
//     statutory scale runs from 9.5% (≤ €12,450) to 24.5% (>
//     €300,000); the autonomic communities add their own scale
//     on top, with the average national autonomic burden also
//     adding 9.5%–24.5%. The combined "tipo total" published by
//     the AEAT for the state-base calculation is what most
//     payroll engines apply at source: 19% / 24% / 30% / 37% /
//     45% / 47% across six brackets, mirroring the AEAT 2025
//     tabla unificada. Tenants in autonomies with materially
//     different scales (Madrid, Catalunya) can override per-
//     employee.
//
//   - Personal-allowance basics: a €5,550 personal minimum
//     (Mínimo Personal del Contribuyente) is deducted from the
//     IRPF base before brackets — though for monthly payroll
//     purposes the AEAT publishes a separate retention rate
//     table that already folds the minimum in. This pack uses
//     the bracket method against gross MINUS the personal
//     minimum, which gives the AEAT-aligned result for a
//     single, no-dependents employee.
//
//   - Seguridad Social employee share: 6.47% total in 2025,
//     composed of:
//       Contingencias comunes  4.70%
//       Desempleo             1.55% (1.60% for temporary contracts)
//       Formación profesional 0.10%
//       MEI (Mecanismo de Equidad Intergeneracional) 0.12%
//     Capped at the monthly base máxima (€4,909.50 in 2025).
//
// References:
//
//	AEAT IRPF tipos 2025:
//	  https://sede.agenciatributaria.gob.es/Sede/ayuda/manuales-videos-folletos/manuales-practicos/manual-renta-2024.html
//	Seguridad Social cuotas 2025:
//	  https://www.seg-social.es/wps/portal/wss/internet/PortalEducativo/Profesores/Unidad4/U44306
type esPack struct{}

func init() { Register(&esPack{}) }

func (esPack) Country() string { return "ES" }

// EffectiveYear returns the fiscal year the ES tables are
// calibrated for: 2025 (AEAT tabla unificada 2025 + TGSS cuotas
// 2025).
func (esPack) EffectiveYear() int { return 2025 }

type esBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// AEAT 2025 tabla unificada (estado + autonómico medio).
	// Annual brackets in EUR; Rate is the marginal rate applied
	// to (taxable - Floor) up to Top.
	esIRPFBrackets = []esBracket{
		{Floor: dec("0"), Top: dec("12450"), Base: dec("0"), Rate: dec("0.19")},
		{Floor: dec("12450"), Top: dec("20200"), Base: dec("2365.50"), Rate: dec("0.24")},
		{Floor: dec("20200"), Top: dec("35200"), Base: dec("4225.50"), Rate: dec("0.30")},
		{Floor: dec("35200"), Top: dec("60000"), Base: dec("8725.50"), Rate: dec("0.37")},
		{Floor: dec("60000"), Top: dec("300000"), Base: dec("17901.50"), Rate: dec("0.45")},
		{Floor: dec("300000"), Top: decimal.Zero, Base: dec("125901.50"), Rate: dec("0.47")},
	}

	esPersonalMinimum  = dec("5550") // Mínimo personal del contribuyente

	esSSContingencias    = dec("0.047")
	esSSDesempleo        = dec("0.0155") // permanent contract
	esSSFormacion        = dec("0.001")
	esSSMEI              = dec("0.0012")
	esSSEmployeeRate     = esSSContingencias.
				Add(esSSDesempleo).
				Add(esSSFormacion).
				Add(esSSMEI)
	esSSBaseMaximaMonth = dec("4909.50") // 2025 base máxima mensual

	esPeriodMonth = decimal.NewFromInt(30)
	esAnnualDays  = decimal.NewFromFloat(365.25)
)

func (esPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(esAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// IRPF — annualised gross minus the personal minimum, then
	// bracket walk, then prorate back.
	taxable := annualGross.Sub(esPersonalMinimum)
	if taxable.LessThan(decimal.Zero) {
		taxable = decimal.Zero
	}
	annualTax := walkESBrackets(taxable, esIRPFBrackets)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "ES_IRPF",
			Name:   "IRPF (ES)",
			Amount: periodTax,
		})
	}

	// Seguridad Social — apply each employee component
	// separately for ledger clarity. Cap each at the prorated
	// monthly base máxima.
	periodCap := esSSBaseMaximaMonth.Mul(decimal.NewFromInt(int64(days)).Div(esPeriodMonth))
	ssBase := gross
	if ssBase.GreaterThan(periodCap) {
		ssBase = periodCap
	}
	if cc := ssBase.Mul(esSSContingencias).Round(2); cc.IsPositive() {
		out = append(out, Deduction{
			Code:   "ES_SS_CONTINGENCIAS",
			Name:   "Seguridad Social — contingencias comunes (ES)",
			Amount: cc,
		})
	}
	if des := ssBase.Mul(esSSDesempleo).Round(2); des.IsPositive() {
		out = append(out, Deduction{
			Code:   "ES_SS_DESEMPLEO",
			Name:   "Seguridad Social — desempleo (ES)",
			Amount: des,
		})
	}
	if fp := ssBase.Mul(esSSFormacion).Round(2); fp.IsPositive() {
		out = append(out, Deduction{
			Code:   "ES_SS_FORMACION",
			Name:   "Seguridad Social — formación profesional (ES)",
			Amount: fp,
		})
	}
	if mei := ssBase.Mul(esSSMEI).Round(2); mei.IsPositive() {
		out = append(out, Deduction{
			Code:   "ES_SS_MEI",
			Name:   "Seguridad Social — MEI (ES)",
			Amount: mei,
		})
	}

	return out, nil
}

// walkESBrackets walks the IRPF schedule.
func walkESBrackets(taxable decimal.Decimal, brackets []esBracket) decimal.Decimal {
	var match esBracket
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
