package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// dkPack implements Denmark's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - AM-bidrag (Arbejdsmarkedsbidrag, labour-market
//     contribution): 8% on gross income, computed before
//     A-skat. The base of all subsequent withholdings is
//     gross × (1 − 0.08).
//
//   - A-skat (income tax) — Denmark uses a skattekort
//     (e-skattekort B-Income) per employee with three
//     individual variables: trækprocent (withholding rate),
//     fradrag (monthly allowance), and frikort balance. This
//     pack uses a representative composite for the average
//     wage earner in 2025:
//
//       Personfradrag (annual)          DKK 51,600
//       Bundskat (bottom tax)              12.01%
//       Topskat (top tax above 588,900)    15.00%
//       Kommuneskat (average)              ~25%
//
//     The composite trækprocent is 37% (bundskat 12.01% +
//     average kommuneskat 25% rounded) below the topskat
//     threshold, and 52% above (composite + topskat). Real
//     payroll engines read skatte­kortet via eIndkomst; this
//     pack uses the composite for ledger-correctness without
//     requiring the skattekortregister integration.
//
// References:
//
//	Skattestyrelsen — Skattesatser 2025:
//	  https://www.skat.dk/borger/skattekort-personfradrag-og-trækprocent
//	Skatteministeriet — Bundskat, topskat, kommunal 2025:
//	  https://www.skm.dk/skattetal/satser
type dkPack struct{}

func init() { Register(&dkPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (dkPack) Country() string { return "DK" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (Skattestyrelsen, 2025 satser).
func (dkPack) EffectiveYear() int { return 2025 }

var (
	dkAMBidragRate = dec("0.08")

	dkPersonfradrag    = dec("51600")
	dkBundskatRate     = dec("0.1201")
	dkKommuneRate      = dec("0.25") // composite national average
	dkTopskatRate      = dec("0.15")
	dkTopskatThreshold = dec("588900") // 2025 topskattegrænse

	dkAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits two lines:
//
//   - DK_AM_BIDRAG (8% labour-market contribution on raw gross)
//   - DK_A_SKAT (composite income tax: bundskat + kommune-
//     average + topskat above DKK 588,900 / yr, applied to
//     gross net of AM-bidrag and personfradrag)
//
// Negative or zero gross returns nil.
func (dkPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(dkAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}

	out := []Deduction{}

	// AM-bidrag — 8% on raw gross, no allowance.
	amBidrag := gross.Mul(dkAMBidragRate).Round(2)
	if amBidrag.IsPositive() {
		out = append(out, Deduction{
			Code:   "DK_AM_BIDRAG",
			Name:   "Arbejdsmarkedsbidrag (DK)",
			Amount: amBidrag,
		})
	}

	// Base for A-skat = gross − AM-bidrag.
	aSkatBase := gross.Sub(amBidrag)
	annualBase := aSkatBase.Div(periodFraction)
	taxableAnnual := annualBase.Sub(dkPersonfradrag)
	if taxableAnnual.LessThan(decimal.Zero) {
		taxableAnnual = decimal.Zero
	}

	// Composite bottom-bracket rate = bundskat + kommune average.
	bottomRate := dkBundskatRate.Add(dkKommuneRate)
	var annualAskat decimal.Decimal
	if taxableAnnual.LessThanOrEqual(dkTopskatThreshold) {
		annualAskat = taxableAnnual.Mul(bottomRate)
	} else {
		annualAskat = dkTopskatThreshold.Mul(bottomRate).Add(
			taxableAnnual.Sub(dkTopskatThreshold).Mul(bottomRate.Add(dkTopskatRate)),
		)
	}
	periodAskat := annualAskat.Mul(periodFraction).Round(2)
	if periodAskat.IsPositive() {
		out = append(out, Deduction{
			Code:   "DK_A_SKAT",
			Name:   "A-skat (DK)",
			Amount: periodAskat,
		})
	}

	return out, nil
}
