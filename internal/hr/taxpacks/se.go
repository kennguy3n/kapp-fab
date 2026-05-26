package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// sePack implements Sweden's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - Kommunalskatt (municipal income tax). The 2025 weighted
//     national average is 32.41% but it varies by kommun
//     (Stockholm 30.38% → some rural municipalities 35.15%).
//     This pack uses 32.00% as a conservative single-rate
//     approximation; tenants needing per-kommun precision feed
//     EmployeeInfo.Canton (re-used as the kommun ID) and the
//     pack overrides the rate from the seKommunRates table —
//     today only the four largest municipalities are wired in
//     (Stockholm / Göteborg / Malmö / Uppsala) with the rest
//     falling back to 32.00%.
//
//   - Statlig inkomstskatt (state income tax). 20% surcharge
//     on annual taxable income above the brytpunkt (state-tax
//     threshold) — SEK 643,100 for 2025 (taxable income, i.e.
//     after the grundavdrag personal allowance which this
//     pack simplifies to a flat SEK 15,400 / yr).
//
//   - Allmän pensionsavgift (general pension fee). 7% of gross
//     earnings, capped at 8.07 × inkomstbasbeloppet (IBB).
//     IBB 2025 = SEK 80,600 → cap = SEK 650,442 / yr. This
//     contribution is creditable in full against income tax
//     liability (the skattereduktion för allmän
//     pensionsavgift) so for most employees the net cost is
//     zero — but the gross deduction is still shown on the
//     slip per Swedish payroll convention.
//
// References:
//
//	Skatteverket — kommunalskatt 2025:
//	  https://www.skatteverket.se/privat/skatter/arbeteochinkomst/skatteavdragiloneutbetalning
//	Skatteverket — statlig inkomstskatt 2025:
//	  https://www.skatteverket.se/privat/skatter/arbeteochinkomst/inkomster/statliginkomstskatt
//	Försäkringskassan — IBB 2025:
//	  https://www.forsakringskassan.se/privatperson/coronaviruset-covid-19/sjukpenninggrundande-inkomst
type sePack struct{}

func init() { Register(&sePack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (sePack) Country() string { return "SE" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (Skatteverket 2025 års tabeller).
func (sePack) EffectiveYear() int { return 2025 }

var (
	seDefaultKommunalRate = dec("0.32")
	// Hand-picked kommun rates for the four largest by population.
	// The map is keyed by uppercased kommun code; an empty or
	// unknown code falls back to seDefaultKommunalRate.
	seKommunRates = map[string]decimal.Decimal{
		"STOCKHOLM": dec("0.3038"),
		"GOTEBORG":  dec("0.3215"),
		"MALMO":     dec("0.3220"),
		"UPPSALA":   dec("0.3185"),
	}
	seStatligRate          = dec("0.20")
	seStatligThreshold     = dec("643100")
	seGrundavdrag          = dec("15400") // simplified flat allowance

	seAllmanPensionRate = dec("0.07")
	seAllmanPensionCap  = dec("650442") // 8.07 × IBB 2025 (80,600)

	seAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to three lines:
//
//   - SE_PENSION_AVGIFT (allmän pensionsavgift, capped at IBB ×
//     8.07)
//   - SE_KOMMUNALSKATT  (municipal tax, kommun-specific or 32%
//     average)
//   - SE_STATLIG_SKATT  (state surcharge 20% above SEK 643,100
//     annual taxable income)
//
// Negative or zero gross returns nil. The pack reads YTDGross
// to enforce the annual cap on allmän pensionsavgift.
func (sePack) ComputeWithholding(_ context.Context, employee EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(seAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}

	out := []Deduction{}

	// Allmän pensionsavgift — 7% of gross, capped on the annual
	// pensionable base via YTD.
	pensionBase := gross
	if employee.YTDGross.GreaterThanOrEqual(seAllmanPensionCap) {
		pensionBase = decimal.Zero
	} else if employee.YTDGross.Add(gross).GreaterThan(seAllmanPensionCap) {
		pensionBase = seAllmanPensionCap.Sub(employee.YTDGross)
	}
	pension := pensionBase.Mul(seAllmanPensionRate).Round(2)
	if pension.IsPositive() {
		out = append(out, Deduction{
			Code:   "SE_PENSION_AVGIFT",
			Name:   "Allmän pensionsavgift (SE)",
			Amount: pension,
		})
	}

	// Taxable annual income = annual gross - grundavdrag.
	annualGross := gross.Div(periodFraction)
	taxableAnnual := annualGross.Sub(seGrundavdrag)
	if taxableAnnual.LessThan(decimal.Zero) {
		taxableAnnual = decimal.Zero
	}

	// Kommunalskatt — flat per-kommun rate on annual taxable
	// income, prorated back via periodFraction.
	rate := seDefaultKommunalRate
	if r, ok := seKommunRates[employee.Canton]; ok {
		rate = r
	}
	annualKommunal := taxableAnnual.Mul(rate)
	periodKommunal := annualKommunal.Mul(periodFraction).Round(2)
	if periodKommunal.IsPositive() {
		out = append(out, Deduction{
			Code:   "SE_KOMMUNALSKATT",
			Name:   "Kommunalskatt (SE)",
			Amount: periodKommunal,
		})
	}

	// Statlig inkomstskatt — 20% on income above SEK 643,100.
	if taxableAnnual.GreaterThan(seStatligThreshold) {
		annualStatlig := taxableAnnual.Sub(seStatligThreshold).Mul(seStatligRate)
		periodStatlig := annualStatlig.Mul(periodFraction).Round(2)
		if periodStatlig.IsPositive() {
			out = append(out, Deduction{
				Code:   "SE_STATLIG_SKATT",
				Name:   "Statlig inkomstskatt (SE)",
				Amount: periodStatlig,
			})
		}
	}

	return out, nil
}
