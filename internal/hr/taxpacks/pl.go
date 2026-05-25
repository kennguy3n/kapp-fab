package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// plPack implements Poland's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - PIT (Podatek dochodowy od osób fizycznych): two-bracket
//     progressive tax with a PLN 30,000 / yr tax-free allowance
//     (kwota wolna od podatku). The lower bracket is 12% on
//     income up to PLN 120,000 / yr; income above PLN 120,000
//     is taxed at 32%. The PLN 30,000 allowance is converted
//     into the kwota zmniejszająca podatek (PLN 3,600 / yr)
//     applied as a credit against the gross liability rather
//     than as a deduction from income.
//
//   - ZUS employee contributions (Zakład Ubezpieczeń
//     Społecznych) — the social-insurance bloc deducted from
//     the employee share of gross pay:
//
//       Emerytalne (pension)     9.76%
//       Rentowe (disability)     1.50%
//       Chorobowe (sickness)     2.45%
//       ----------------------------- 13.71% total
//
//     The 2025 annual cap for pension + disability is 30 × the
//     forecast average monthly wage (PLN 260,190 for 2025);
//     this pack honours the cap via YTD-aware accumulation
//     against EmployeeInfo.YTDGross. Sickness has no cap.
//
//   - NFZ (Narodowy Fundusz Zdrowia, health insurance): 9% of
//     the contribution base, which is (gross − ZUS employee
//     contributions). PIT deductibility of the NFZ contribution
//     was abolished from 2022 onwards (Polski Ład), so the
//     9% is a pure employee cost.
//
// References:
//
//	PIT-11 / PIT-37 2025:
//	  https://www.podatki.gov.pl/pit/
//	ZUS contribution rates 2025:
//	  https://www.zus.pl/baza-wiedzy/skladki-wskazniki-odsetki
//	NFZ rates:
//	  https://www.nfz.gov.pl/
type plPack struct{}

func init() { Register(&plPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (plPack) Country() string { return "PL" }

// EffectiveYear returns the fiscal year the PL tables are calibrated
// for: 2025 (Polski Ład 2.0, Ustawa o PIT z 2024-10-30, ZUS i NFZ
// stawki 2025).
func (plPack) EffectiveYear() int { return 2025 }

var (
	plPITLowRate         = dec("0.12")
	plPITHighRate        = dec("0.32")
	plPITBracketCutoff   = dec("120000")
	plPITTaxFreeAllowance = dec("30000")
	plPITTaxFreeCredit   = plPITTaxFreeAllowance.Mul(plPITLowRate) // PLN 3,600 / yr

	plZUSEmerytalneRate = dec("0.0976")
	plZUSRentoweRate    = dec("0.015")
	plZUSChorobowRate   = dec("0.0245")
	// Annual cap for pension+disability contributions (2025
	// forecast: PLN 260,190). Sickness has no cap.
	plZUSAnnualCap = dec("260190")

	plNFZRate = dec("0.09")

	plAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to three lines:
//
//   - PL_ZUS  (social insurance employee share, capped)
//   - PL_NFZ  (health insurance, 9% on gross-after-ZUS)
//   - PL_PIT  (income tax with progressive bracket + tax-free
//     allowance credit)
//
// Negative or zero gross returns nil. The pack reads YTDGross
// (year-to-date gross before this slip) to enforce the ZUS
// annual cap on pension+disability.
func (plPack) ComputeWithholding(_ context.Context, employee EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(plAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}

	out := []Deduction{}

	// ZUS pension + disability are capped via YTD. Compute the
	// capped pension+disability base for this slip.
	pensionDisabilityBase := gross
	if employee.YTDGross.GreaterThanOrEqual(plZUSAnnualCap) {
		pensionDisabilityBase = decimal.Zero
	} else if employee.YTDGross.Add(gross).GreaterThan(plZUSAnnualCap) {
		pensionDisabilityBase = plZUSAnnualCap.Sub(employee.YTDGross)
	}
	zusEmerytalne := pensionDisabilityBase.Mul(plZUSEmerytalneRate)
	zusRentowe := pensionDisabilityBase.Mul(plZUSRentoweRate)
	zusChorobowe := gross.Mul(plZUSChorobowRate) // never capped
	zusTotal := zusEmerytalne.Add(zusRentowe).Add(zusChorobowe).Round(2)
	if zusTotal.IsPositive() {
		out = append(out, Deduction{
			Code:   "PL_ZUS",
			Name:   "ZUS social insurance (employee, PL)",
			Amount: zusTotal,
		})
	}

	// NFZ base = gross − ZUS (the contribution base post-2022
	// Polski Ład; not deductible from PIT).
	nfzBase := gross.Sub(zusTotal)
	if nfzBase.LessThan(decimal.Zero) {
		nfzBase = decimal.Zero
	}
	nfz := nfzBase.Mul(plNFZRate).Round(2)
	if nfz.IsPositive() {
		out = append(out, Deduction{
			Code:   "PL_NFZ",
			Name:   "NFZ health insurance (PL)",
			Amount: nfz,
		})
	}

	// PIT base = (gross − ZUS); progressive bracket walk on
	// annualised base, then prorated back by periodFraction,
	// with the PLN 3,600 / yr credit also prorated.
	pitBase := gross.Sub(zusTotal)
	if pitBase.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	annualBase := pitBase.Div(periodFraction)

	var annualPIT decimal.Decimal
	if annualBase.LessThanOrEqual(plPITBracketCutoff) {
		annualPIT = annualBase.Mul(plPITLowRate)
	} else {
		annualPIT = plPITBracketCutoff.Mul(plPITLowRate).Add(
			annualBase.Sub(plPITBracketCutoff).Mul(plPITHighRate),
		)
	}
	annualPIT = annualPIT.Sub(plPITTaxFreeCredit)
	if annualPIT.LessThan(decimal.Zero) {
		annualPIT = decimal.Zero
	}
	periodPIT := annualPIT.Mul(periodFraction).Round(2)
	if periodPIT.IsPositive() {
		out = append(out, Deduction{
			Code:   "PL_PIT",
			Name:   "PIT income tax (PL)",
			Amount: periodPIT,
		})
	}

	return out, nil
}
