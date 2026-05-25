package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// bePack implements Belgium's payroll-side statutory
// withholdings for the 2025 fiscal year:
//
//   - Précompte Professionnel / Bedrijfsvoorheffing (PP/BV):
//     the at-source income tax. The SPF Finances publishes
//     annual barèmes; for 2025 the standard schedule
//     (résident, isolé sans personne à charge) is progressive
//     in five bands:
//       0      → 15,820     25%
//       15,820 → 27,920     40%
//       27,920 → 48,320     45%
//       48,320 → open       50%
//     A €11,170 personal allowance (quotité exemptée 2025) is
//     deducted before the bracket walk. The pack uses the
//     income-tax barème — which the precompte professionnel
//     tables approximate — rather than reproducing all 200+
//     scheduled monthly figures.
//
//   - ONSS / RSZ employee share: 13.07% of monthly gross,
//     uncapped. This is the standard rate for ordinary
//     employees (ouvriers + employés). Belgium does not
//     apply an annual ceiling like Germany or France — the
//     contribution accrues on every euro.
//
//   - Special social-security contribution (cotisation
//     spéciale): progressive surcharge on annual net taxable
//     income above €18,592, peaking at €731/yr at €60,162.
//     This pack does not implement the special contribution
//     today (it requires household income, not just employee
//     wage) and emits no line for it.
//
// References:
//
//	SPF Finances — Précompte professionnel 2025:
//	  https://finances.belgium.be/fr/entreprises/personnel-et-remuneration/precompte-professionnel
//	ONSS / RSZ cotisations 2025:
//	  https://www.onss.be/employeurs/cotisations
type bePack struct{}

func init() { Register(&bePack{}) }

func (bePack) Country() string { return "BE" }

// EffectiveYear returns the fiscal year the BE tables are
// calibrated for: 2025 (SPF Finances barème 2025 + ONSS 2025).
func (bePack) EffectiveYear() int { return 2025 }

type beBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	bePPBrackets = []beBracket{
		{Floor: dec("0"), Top: dec("15820"), Base: dec("0"), Rate: dec("0.25")},
		{Floor: dec("15820"), Top: dec("27920"), Base: dec("3955"), Rate: dec("0.40")},
		{Floor: dec("27920"), Top: dec("48320"), Base: dec("8795"), Rate: dec("0.45")},
		{Floor: dec("48320"), Top: decimal.Zero, Base: dec("17975"), Rate: dec("0.50")},
	}

	bePersonalAllowance = dec("11170") // quotité exemptée 2025
	beONSSRate          = dec("0.1307")

	beAnnualDays = decimal.NewFromFloat(365.25)
)

func (bePack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(beAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// ONSS employee share — 13.07% on every euro of gross.
	// Belgium computes ONSS BEFORE the income tax base, so we
	// emit it first; the bracket walk uses gross MINUS ONSS as
	// the IRPP base (matches the SPF Finances barème logic).
	onss := gross.Mul(beONSSRate).Round(2)
	if onss.IsPositive() {
		out = append(out, Deduction{
			Code:   "BE_ONSS",
			Name:   "ONSS / RSZ (employee share, BE)",
			Amount: onss,
		})
	}
	annualONSS := annualGross.Mul(beONSSRate)

	// Précompte professionnel — bracket walk on annualGross
	// minus ONSS minus personal allowance.
	taxableBase := annualGross.Sub(annualONSS).Sub(bePersonalAllowance)
	if taxableBase.LessThan(decimal.Zero) {
		taxableBase = decimal.Zero
	}
	annualTax := walkBEBrackets(taxableBase, bePPBrackets)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "BE_PP",
			Name:   "Précompte professionnel (BE)",
			Amount: periodTax,
		})
	}

	return out, nil
}

// walkBEBrackets walks the précompte professionnel schedule.
func walkBEBrackets(taxable decimal.Decimal, brackets []beBracket) decimal.Decimal {
	var match beBracket
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
