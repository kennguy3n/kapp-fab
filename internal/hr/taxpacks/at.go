package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// atPack implements Austria's payroll-side statutory
// withholdings for the 2025 fiscal year:
//
//   - Lohnsteuer (LSt): the federal income tax. Austria's 2025
//     bracket schedule is progressive in seven bands:
//       0       → 13,308     0%
//       13,308  → 21,617     20%
//       21,617  → 35,836     30%
//       35,836  → 69,166     40%
//       69,166  → 103,072    48%
//       103,072 → 1,000,000  50%
//       > 1,000,000          55%
//     The first band is the Steuerfreibetrag (tax-free zone)
//     which was indexed up from €12,816 in 2024 to €13,308 in
//     2025 under the inflation indexation law (Teuerungs-
//     Entlastungspaket III).
//
//   - SV-Beiträge (Sozialversicherung) employee shares — 2025:
//       Pensionsversicherung (PV)  10.25%
//       Krankenversicherung  (KV)  3.87%
//       Arbeitslosenversicherung (AV) 2.95%
//       Arbeiterkammer Umlage (AK) 0.50%
//       Wohnbauförderungsbeitrag    0.50%
//     Total employee share ~18.07%. Capped at the monthly
//     Höchstbemessungsgrundlage (HBGl) of €6,450 in 2025
//     (€86,400 / yr; "annual" cap is the monthly × 14 because
//     Austrian employees receive 13th/14th-month payments
//     taxed separately under the sondersteuer regime).
//
// References:
//
//	BMF Lohnsteuertarif 2025:
//	  https://www.bmf.gv.at/themen/steuern/fuer-arbeitnehmerinnen/lohnsteuer.html
//	ÖGK Beitragssätze 2025:
//	  https://www.gesundheitskasse.at/cdscontent/?contentid=10007.890244
//	HBGl 2025 (BMSVG):
//	  https://www.svs.at/cdscontent/?contentid=10007.749530
type atPack struct{}

func init() { Register(&atPack{}) }

func (atPack) Country() string { return "AT" }

// EffectiveYear returns the fiscal year the AT tables are
// calibrated for: 2025 (BMF Lohnsteuertarif 2025 + ÖGK
// Beitragssätze 2025).
func (atPack) EffectiveYear() int { return 2025 }

type atBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	atLohnsteuerBrackets = []atBracket{
		{Floor: dec("0"), Top: dec("13308"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("13308"), Top: dec("21617"), Base: dec("0"), Rate: dec("0.20")},
		{Floor: dec("21617"), Top: dec("35836"), Base: dec("1661.80"), Rate: dec("0.30")},
		{Floor: dec("35836"), Top: dec("69166"), Base: dec("5927.50"), Rate: dec("0.40")},
		{Floor: dec("69166"), Top: dec("103072"), Base: dec("19259.50"), Rate: dec("0.48")},
		{Floor: dec("103072"), Top: dec("1000000"), Base: dec("35534.38"), Rate: dec("0.50")},
		{Floor: dec("1000000"), Top: decimal.Zero, Base: dec("483998.38"), Rate: dec("0.55")},
	}

	// Employee SV-Beiträge (each emitted as a separate ledger
	// line for clarity).
	atPVRate      = dec("0.1025") // Pensionsversicherung
	atKVRate      = dec("0.0387") // Krankenversicherung
	atAVRate      = dec("0.0295") // Arbeitslosenversicherung
	atAKRate      = dec("0.0050") // Arbeiterkammer
	atWBFRate     = dec("0.0050") // Wohnbauförderung

	atHBGlMonthly = dec("6450")   // 2025 Höchstbemessungsgrundlage
	atMonthDays   = decimal.NewFromInt(30)
	atAnnualDays  = decimal.NewFromFloat(365.25)
)

func (atPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(atAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// Lohnsteuer — bracket walk on annualised gross.
	annualLSt := walkATBrackets(annualGross, atLohnsteuerBrackets)
	periodLSt := annualLSt.Mul(periodFraction).Round(2)
	if periodLSt.IsPositive() {
		out = append(out, Deduction{
			Code:   "AT_LOHNSTEUER",
			Name:   "Lohnsteuer (AT)",
			Amount: periodLSt,
		})
	}

	// SV-Beiträge — capped per-period at the prorated HBGl.
	periodCap := atHBGlMonthly.Mul(decimal.NewFromInt(int64(days)).Div(atMonthDays))
	svBase := gross
	if svBase.GreaterThan(periodCap) {
		svBase = periodCap
	}

	if pv := svBase.Mul(atPVRate).Round(2); pv.IsPositive() {
		out = append(out, Deduction{Code: "AT_PV", Name: "Pensionsversicherung (employee, AT)", Amount: pv})
	}
	if kv := svBase.Mul(atKVRate).Round(2); kv.IsPositive() {
		out = append(out, Deduction{Code: "AT_KV", Name: "Krankenversicherung (employee, AT)", Amount: kv})
	}
	if av := svBase.Mul(atAVRate).Round(2); av.IsPositive() {
		out = append(out, Deduction{Code: "AT_AV", Name: "Arbeitslosenversicherung (employee, AT)", Amount: av})
	}
	if ak := svBase.Mul(atAKRate).Round(2); ak.IsPositive() {
		out = append(out, Deduction{Code: "AT_AK", Name: "Arbeiterkammer Umlage (AT)", Amount: ak})
	}
	if wbf := svBase.Mul(atWBFRate).Round(2); wbf.IsPositive() {
		out = append(out, Deduction{Code: "AT_WBF", Name: "Wohnbauförderungsbeitrag (employee, AT)", Amount: wbf})
	}

	return out, nil
}

// walkATBrackets walks the Lohnsteuer schedule.
func walkATBrackets(annual decimal.Decimal, brackets []atBracket) decimal.Decimal {
	var match atBracket
	matched := false
	for _, b := range brackets {
		if annual.LessThanOrEqual(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.Base.Add(annual.Sub(match.Floor).Mul(match.Rate))
}
