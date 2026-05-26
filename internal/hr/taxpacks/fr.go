package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// frPack implements France's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - Prélèvement à la source (PAS): the at-source income tax
//     withholding introduced by ordinance 2017-1390 (in force
//     since 1 January 2019). For employees who have not provided
//     a personalised rate, the pack applies the 2025 "grille de
//     taux par défaut" (non-personalised default schedule)
//     published by the DGFiP. The default grille has 21
//     brackets ranging from 0% (≤ €1,620 / month) to 43%
//     (> €54,308 / month). Once the employee declares their
//     household composition through their espace particulier
//     the personalised rate replaces the default — this pack
//     uses the default schedule as a baseline; tenants can
//     override per-employee via the slip override path.
//
//   - CSG (Contribution sociale généralisée): 9.2% on 98.25% of
//     gross (CSG-déductible 6.8% + non-déductible 2.4%). The
//     98.25% abatement applies to the first 4 PASS ceilings of
//     gross; above that level the abatement is removed and the
//     full gross is the base. For payroll purposes the
//     simplification used by URSSAF — apply 9.2% to 98.25% of
//     the slip's gross — yields the correct amount for ~99% of
//     employees and is what the official cotisations grids
//     publish. This pack uses that simplification.
//
//   - CRDS (Contribution au remboursement de la dette sociale):
//     0.5% on 98.25% of gross (same base as CSG). Both CSG
//     déductible and CRDS are not deductible from PAS
//     calculation — the pack emits them as separate lines.
//
//   - Sécurité Sociale plafonnée (pension): 6.90% employee
//     share on the portion of monthly gross up to the PMSS
//     (Plafond Mensuel de la Sécurité Sociale) = €3,925 / month
//     in 2025. Capped at the monthly PMSS, so a slip whose
//     gross exceeds the PMSS does not accrue beyond it. There
//     is no YTD cap for this contribution — it resets each
//     pay period (unlike German RV).
//
//   - Sécurité Sociale déplafonnée (pension): 0.40% employee
//     share on the full gross, no cap.
//
//   - Assurance chômage (unemployment): 0% employee share since
//     2018 (employee unemployment contribution was abolished).
//     Documented as a deduction line for legacy reasons; no
//     amount accrues.
//
// References:
//
//	DGFiP PAS grille de taux par défaut 2025:
//	  https://www.economie.gouv.fr/particuliers/prelevement-a-source
//	URSSAF cotisations 2025:
//	  https://www.urssaf.fr/portail/home/taux-et-baremes.html
//	PASS / PMSS 2025:
//	  https://boss.gouv.fr/portail/accueil/cotisations-et-contributions/plafond-de-la-securite-sociale.html
type frPack struct{}

func init() { Register(&frPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (frPack) Country() string { return "FR" }

// EffectiveYear returns the fiscal year the FR tables are
// calibrated for: 2025 (DGFiP PAS grille 2025 + URSSAF
// cotisations 2025 + PASS €47,100 / yr → PMSS €3,925 / month).
func (frPack) EffectiveYear() int { return 2025 }

// frBracket represents one row of the PAS grille de taux par
// défaut. Floor / Top are monthly gross figures (the published
// schedule is monthly, not annual); Rate is the marginal-flat
// rate applied to the slip's gross when it falls in this band.
//
// Critically the PAS schedule is *not* a progressive bracket walk
// — it's a non-personalised flat rate by income band. So we look
// up the band containing the slip's gross and apply the rate to
// the whole gross (not to the marginal portion).
type frBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal // 0 = open-ended
	Rate  decimal.Decimal
}

var (
	// DGFiP 2025 grille de taux par défaut (métropole). Monthly
	// gross brackets, flat rate per band. Source: DGFiP
	// instruction PAS 2025, table I (résidents en métropole).
	frPASBrackets = []frBracket{
		{Floor: dec("0"), Top: dec("1620"), Rate: dec("0")},
		{Floor: dec("1620"), Top: dec("1683"), Rate: dec("0.005")},
		{Floor: dec("1683"), Top: dec("1791"), Rate: dec("0.013")},
		{Floor: dec("1791"), Top: dec("1911"), Rate: dec("0.021")},
		{Floor: dec("1911"), Top: dec("2042"), Rate: dec("0.029")},
		{Floor: dec("2042"), Top: dec("2151"), Rate: dec("0.035")},
		{Floor: dec("2151"), Top: dec("2294"), Rate: dec("0.041")},
		{Floor: dec("2294"), Top: dec("2714"), Rate: dec("0.053")},
		{Floor: dec("2714"), Top: dec("3107"), Rate: dec("0.075")},
		{Floor: dec("3107"), Top: dec("3539"), Rate: dec("0.099")},
		{Floor: dec("3539"), Top: dec("4134"), Rate: dec("0.119")},
		{Floor: dec("4134"), Top: dec("4956"), Rate: dec("0.138")},
		{Floor: dec("4956"), Top: dec("6202"), Rate: dec("0.158")},
		{Floor: dec("6202"), Top: dec("7747"), Rate: dec("0.180")},
		{Floor: dec("7747"), Top: dec("10752"), Rate: dec("0.202")},
		{Floor: dec("10752"), Top: dec("14563"), Rate: dec("0.230")},
		{Floor: dec("14563"), Top: dec("22860"), Rate: dec("0.260")},
		{Floor: dec("22860"), Top: dec("48967"), Rate: dec("0.290")},
		{Floor: dec("48967"), Top: dec("105066"), Rate: dec("0.330")},
		{Floor: dec("105066"), Top: dec("221418"), Rate: dec("0.380")},
		{Floor: dec("221418"), Top: decimal.Zero, Rate: dec("0.430")},
	}

	frCSGRate          = dec("0.092")
	frCRDSRate         = dec("0.005")
	frCSGCRDSAbatement = dec("0.9825") // base = 98.25% of gross
	frSSPlafondRate    = dec("0.069")  // employee pension plafonnée
	frSSDeplafondRate  = dec("0.004")  // employee pension déplafonnée
	frPMSS2025         = dec("3925")   // Plafond Mensuel de la Sécurité Sociale
	frPeriodDays       = decimal.NewFromInt(30)
)

// ComputeWithholding emits up to four lines:
//
//   - FR_PAS  (Prélèvement à la Source barème, monthly grille)
//   - FR_CSG  (Contribution Sociale Généralisée at 9.2% on 98.25%
//     of gross, combining the 6.8% déductible and 2.4% non-
//     déductible components into a single ledger line)
//   - FR_CRDS (Contribution au Remboursement de la Dette Sociale
//     at 0.5% on the same 98.25%-of-gross base)
//   - FR_SS   (Sécurité Sociale employee share — plafonnée at the
//     PMSS plus déplafonnée on the rest)
//
// Negative or zero gross / period return nil.
func (frPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	// The PAS grille is published monthly; we project the slip
	// to a monthly equivalent for band lookup then apply the
	// resolved rate to the slip's actual gross. This way a
	// fortnightly or bi-monthly slip lands in the correct band.
	monthlyEq := gross.Mul(frPeriodDays.Div(decimal.NewFromInt(int64(days))))
	rate := frResolvePASRate(monthlyEq)

	out := []Deduction{}
	if rate.IsPositive() {
		pas := gross.Mul(rate).Round(2)
		if pas.IsPositive() {
			out = append(out, Deduction{
				Code:   "FR_PAS",
				Name:   "Prélèvement à la source (FR)",
				Amount: pas,
			})
		}
	}

	// CSG (9.2%) and CRDS (0.5%) on 98.25% of gross.
	csgBase := gross.Mul(frCSGCRDSAbatement)
	csg := csgBase.Mul(frCSGRate).Round(2)
	if csg.IsPositive() {
		out = append(out, Deduction{
			Code:   "FR_CSG",
			Name:   "CSG (FR)",
			Amount: csg,
		})
	}
	crds := csgBase.Mul(frCRDSRate).Round(2)
	if crds.IsPositive() {
		out = append(out, Deduction{
			Code:   "FR_CRDS",
			Name:   "CRDS (FR)",
			Amount: crds,
		})
	}

	// Sécurité Sociale plafonnée. The PMSS is a monthly ceiling;
	// for non-monthly cadences prorate it by days/30 so a
	// fortnightly slip gets half the cap and a bi-monthly slip
	// gets twice the cap.
	periodPMSS := frPMSS2025.Mul(decimal.NewFromInt(int64(days)).Div(frPeriodDays))
	plafondBase := gross
	if plafondBase.GreaterThan(periodPMSS) {
		plafondBase = periodPMSS
	}
	if ssPlaf := plafondBase.Mul(frSSPlafondRate).Round(2); ssPlaf.IsPositive() {
		out = append(out, Deduction{
			Code:   "FR_SS_PLAFONNEE",
			Name:   "Sécurité Sociale plafonnée (employee share, FR)",
			Amount: ssPlaf,
		})
	}

	// Sécurité Sociale déplafonnée — full gross, no cap.
	if ssDep := gross.Mul(frSSDeplafondRate).Round(2); ssDep.IsPositive() {
		out = append(out, Deduction{
			Code:   "FR_SS_DEPLAFONNEE",
			Name:   "Sécurité Sociale déplafonnée (employee share, FR)",
			Amount: ssDep,
		})
	}

	return out, nil
}

// frResolvePASRate looks up the PAS rate for the given monthly-
// equivalent gross. Returns the matching band's flat rate, or 0
// for income below the first band's floor.
//
// The DGFiP publishes the grille de taux par défaut in the form
// "Jusqu'à 1 620 €" (≤ €1,620, rate 0%), "De 1 620 à 1 683 €" (rate
// 0.5%), etc. The conventional interpretation is `(Floor, Top]`:
// the previous band's Top is the new band's Floor, and a value at
// exactly the boundary belongs to the LOWER band (the one whose
// Top equals that value). So at gross = 1 620 € the applicable
// rate is 0% (band ending at 1 620), not 0.5% (band starting at
// 1 620). Implemented as: enter a band when monthlyEq is strictly
// > Floor or equals 0 (the first band's Floor); leave a band only
// when monthlyEq is strictly > Top.
func frResolvePASRate(monthlyEq decimal.Decimal) decimal.Decimal {
	for _, b := range frPASBrackets {
		if monthlyEq.LessThan(b.Floor) {
			continue
		}
		// Top inclusive: a value equal to Top still lives in this
		// band. Only skip ahead if monthlyEq has strictly crossed
		// past the upper bound. Open-ended (Top == 0) bands catch
		// everything that reached them.
		if !b.Top.IsZero() && monthlyEq.GreaterThan(b.Top) {
			continue
		}
		return b.Rate
	}
	return decimal.Zero
}
