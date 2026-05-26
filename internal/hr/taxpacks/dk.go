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
//     average kommuneskat 25% rounded) on the post-personfradrag
//     taxable base, plus a 15% topskat surcharge on the slice
//     of personlig indkomst (PI = gross − AM-bidrag) above
//     DKK 588,900 / yr. Real payroll engines read skatte­
//     kortet via eIndkomst; this pack uses the composite for
//     ledger-correctness without requiring the skattekort-
//     register integration.
//
//     Per Personskatteloven §§ 6–7 and Skatteministeriets
//     skattesatser 2025, the topskattegrænse is measured
//     against personlig indkomst (gross − AM-bidrag) — NOT
//     against the post-personfradrag base. The bundskat /
//     kommune slice and the topskat slice therefore use
//     different bases: bundskat + kommune apply to
//     PI − personfradrag (this pack's composite simplification
//     of personfradrag-as-tax-credit), topskat applies to PI
//     above the threshold directly.
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
//   - DK_A_SKAT   (composite income tax: bundskat + kommune‐
//     average on PI − personfradrag, plus a 15% topskat
//     surcharge on the slice of PI above DKK 588,900 / yr;
//     PI = gross − AM-bidrag, annualised)
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

	// Personlig indkomst (PI) = gross − AM-bidrag. This is the
	// base both the topskat threshold and the bottom-bracket
	// allowance are measured against — the personfradrag is
	// applied later as a composite base reduction for the bundskat
	// + kommune slice only, while topskat tests PI directly.
	aSkatBase := gross.Sub(amBidrag)
	annualPI := aSkatBase.Div(periodFraction)
	taxableAnnual := annualPI.Sub(dkPersonfradrag)
	if taxableAnnual.LessThan(decimal.Zero) {
		taxableAnnual = decimal.Zero
	}

	// Composite bottom-bracket rate = bundskat + kommune average.
	// Bundskat + kommune apply to taxableAnnual on the full slice
	// (the topskat surcharge does not displace the bottom rate —
	// it stacks on top of it for income above the threshold).
	bottomRate := dkBundskatRate.Add(dkKommuneRate)
	annualAskat := taxableAnnual.Mul(bottomRate)

	// Topskat — 15% on the slice of personlig indkomst above the
	// topskattegrænse. Measured against PI directly per
	// Personskatteloven § 7, NOT against PI − personfradrag.
	if annualPI.GreaterThan(dkTopskatThreshold) {
		topskat := annualPI.Sub(dkTopskatThreshold).Mul(dkTopskatRate)
		annualAskat = annualAskat.Add(topskat)
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
