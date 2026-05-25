package taxpacks

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// dePack implements Germany's payroll-side statutory
// withholdings for the 2025 tax year:
//
//   - Lohnsteuer (wage tax): the federal income tax withheld at
//     source. Implements the official § 32a EStG progressive
//     formula for Steuerklasse I (single, no dependents). The
//     formula is a piecewise polynomial calibrated to the 2025
//     Grundtarif (basic tariff), and is reproduced here directly
//     rather than via a discretised bracket table because the
//     middle "linear-progressive" zones use quadratic
//     interpolation that brackets cannot represent exactly.
//
//   - Solidaritätszuschlag (Soli): 5.5% of Lohnsteuer, but only
//     above the 2025 Freigrenze of €19,950 / year of Lohnsteuer
//     (Steuerklasse I). Within the slide-in zone (€19,950 →
//     ~€39,194) the surcharge is capped to a milder amount
//     (the "Milderungszone"), which this pack approximates by
//     the statutory linear interpolation 11.9% * (Lohnsteuer -
//     Freigrenze) clamped above by 5.5% * Lohnsteuer. Above the
//     slide-in zone Soli is the full 5.5%.
//
//   - Kirchensteuer (church tax): 8% (Bavaria, Baden-Württemberg)
//     or 9% (rest of Germany) of Lohnsteuer, but only for
//     employees who have declared a church affiliation. The
//     wizard surfaces this via EmployeeInfo.PermitType — "KIRCHE"
//     ("8") or ("9") to elect church tax at the corresponding
//     rate. Default = no church tax.
//
//   - Rentenversicherung (statutory pension): 9.3% employee
//     share of monthly contributory wages (BBG-allotted),
//     capped at the 2025 Beitragsbemessungsgrenze of €96,600 /
//     year (unified East/West from 2025). YTD-aware cap matching
//     the US OASDI pattern.
//
//   - Krankenversicherung (statutory health): 7.3% base
//     employee share + ~0.85% average Zusatzbeitrag = 8.15%
//     employee total. Capped at €66,150 / year contribution
//     ceiling (BBG-KV 2025). YTD-aware.
//
//   - Pflegeversicherung (long-term care): 1.7% employee share
//     (2025 rate after 0.35% increase). Childless employees ≥23
//     pay an additional 0.6% surcharge. Capped at the same
//     BBG-KV ceiling. EmployeeInfo.NumDependents == 0 + Age >= 23
//     triggers the surcharge.
//
//   - Arbeitslosenversicherung (unemployment): 1.3% employee
//     share. Capped at the BBG-RV (€96,600) ceiling. YTD-aware.
//
// References:
//
//	§ 32a EStG Lohnsteuer 2025 (Programmablaufplan):
//	  https://www.bmf-steuerrechner.de/
//	Beitragssätze Sozialversicherung 2025 (GKV-SV):
//	  https://www.gkv-spitzenverband.de/krankenversicherung/kv_grundprinzipien/beitragsbemessung/beitragsbemessung.jsp
//	Beitragsbemessungsgrenzen 2025 (BMAS):
//	  https://www.bmas.de/DE/Soziales/Sozialversicherung/Sozialversicherungs-Rechengroessen/sozialversicherungs-rechengroessen.html
type dePack struct{}

func init() { Register(&dePack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (dePack) Country() string { return "DE" }

// EffectiveYear returns the fiscal year the DE tables are
// calibrated for: 2025 (BMF Programmablaufplan 2025 + GKV-SV
// 2025 Beitragssätze).
func (dePack) EffectiveYear() int { return 2025 }

var (
	// § 32a EStG 2025 Grundtarif zone boundaries. Used by the
	// piecewise polynomial in deComputeLohnsteuer.
	deGrundfreibetrag2025 = dec("12096")  // tax-free zone top
	deZone2Top2025        = dec("17443")  // first progressive zone top
	deZone3Top2025        = dec("68480")  // proportional 42% zone bottom
	deZone4Top2025        = dec("277825") // 42% → 45% boundary

	// Solidaritätszuschlag thresholds (Steuerklasse I).
	deSoliFreigrenze        = dec("19950")
	deSoliFullRate          = dec("0.055")
	deSoliMilderungsRate    = dec("0.119")

	// Social security rates (employee share, 2025).
	deRVRate    = dec("0.093")  // Rentenversicherung
	deKVRate    = dec("0.0815") // Krankenversicherung (incl. ~0.85% Zusatzbeitrag)
	dePVRate    = dec("0.017")  // Pflegeversicherung
	dePVChildless = dec("0.006") // additional surcharge for childless ≥23
	deALVRate   = dec("0.013")  // Arbeitslosenversicherung

	// Beitragsbemessungsgrenzen 2025 (annual, EUR).
	deBBGRV = dec("96600") // Rente + ALV
	deBBGKV = dec("66150") // KV + PV

	deAnnualDays = decimal.NewFromFloat(365.25)
)

func (dePack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(deAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)

	out := []Deduction{}

	// Lohnsteuer — § 32a EStG progressive formula on annualised
	// gross, then prorated back to the slip period.
	annualLohnsteuer := deComputeLohnsteuer(annualGross)
	periodLohnsteuer := annualLohnsteuer.Mul(periodFraction).Round(2)
	if periodLohnsteuer.IsPositive() {
		out = append(out, Deduction{
			Code:   "DE_LOHNSTEUER",
			Name:   "Lohnsteuer (DE)",
			Amount: periodLohnsteuer,
		})

		// Solidaritätszuschlag — 5.5% of Lohnsteuer above the
		// Freigrenze. Within the Milderungszone the rate slides
		// in linearly at 11.9% * (LSt - Freigrenze), capped at
		// the full 5.5% * LSt. Computed against ANNUAL
		// Lohnsteuer, then prorated.
		annualSoli := deComputeSoli(annualLohnsteuer)
		periodSoli := annualSoli.Mul(periodFraction).Round(2)
		if periodSoli.IsPositive() {
			out = append(out, Deduction{
				Code:   "DE_SOLI",
				Name:   "Solidaritätszuschlag (DE)",
				Amount: periodSoli,
			})
		}

		// Kirchensteuer — 8% (BY, BW) or 9% (rest), gated on
		// PermitType. Default = no church tax.
		if rate := deResolveKirchensteuerRate(e.PermitType); rate.IsPositive() {
			ks := periodLohnsteuer.Mul(rate).Round(2)
			if ks.IsPositive() {
				out = append(out, Deduction{
					Code:   "DE_KIRCHENSTEUER",
					Name:   "Kirchensteuer (DE)",
					Amount: ks,
				})
			}
		}
	}

	// Rentenversicherung — capped at BBG-RV. YTD-aware against
	// EmployeeInfo.YTDGross.
	if rv := deComputeCappedShare(gross, e.YTDGross, deRVRate, deBBGRV); rv.IsPositive() {
		out = append(out, Deduction{
			Code:   "DE_RV",
			Name:   "Rentenversicherung (employee share, DE)",
			Amount: rv,
		})
	}

	// Krankenversicherung — capped at BBG-KV.
	if kv := deComputeCappedShare(gross, e.YTDGross, deKVRate, deBBGKV); kv.IsPositive() {
		out = append(out, Deduction{
			Code:   "DE_KV",
			Name:   "Krankenversicherung (employee share, DE)",
			Amount: kv,
		})
	}

	// Pflegeversicherung — base 1.7% + 0.6% childless surcharge
	// for employees ≥23 with no children. Same BBG-KV cap.
	pvRate := dePVRate
	if e.NumDependents == 0 && e.Age >= 23 {
		pvRate = pvRate.Add(dePVChildless)
	}
	if pv := deComputeCappedShare(gross, e.YTDGross, pvRate, deBBGKV); pv.IsPositive() {
		out = append(out, Deduction{
			Code:   "DE_PV",
			Name:   "Pflegeversicherung (employee share, DE)",
			Amount: pv,
		})
	}

	// Arbeitslosenversicherung — capped at BBG-RV.
	if alv := deComputeCappedShare(gross, e.YTDGross, deALVRate, deBBGRV); alv.IsPositive() {
		out = append(out, Deduction{
			Code:   "DE_ALV",
			Name:   "Arbeitslosenversicherung (employee share, DE)",
			Amount: alv,
		})
	}

	return out, nil
}

// deComputeLohnsteuer implements § 32a EStG (2025 Grundtarif).
// The formula is piecewise:
//   Zone 1 (≤ 12,096):       0
//   Zone 2 (12,097-17,443):  (932.30 * y + 1400) * y         where y = (zvE - 12096) / 10000
//   Zone 3 (17,444-68,480):  (176.64 * z + 2397) * z + 1015.13 where z = (zvE - 17443) / 10000
//   Zone 4 (68,481-277,825): 0.42 * zvE - 10911.92
//   Zone 5 (> 277,825):      0.45 * zvE - 19246.67
// Coefficients come from the 2025 BMF Programmablaufplan.
func deComputeLohnsteuer(annual decimal.Decimal) decimal.Decimal {
	if annual.LessThanOrEqual(deGrundfreibetrag2025) {
		return decimal.Zero
	}
	if annual.LessThanOrEqual(deZone2Top2025) {
		// y = (annual - 12096) / 10000
		y := annual.Sub(deGrundfreibetrag2025).Div(decimal.NewFromInt(10000))
		// (932.30 * y + 1400) * y
		coeff := dec("932.30").Mul(y).Add(dec("1400"))
		return coeff.Mul(y).Round(2)
	}
	if annual.LessThanOrEqual(deZone3Top2025) {
		// z = (annual - 17443) / 10000
		z := annual.Sub(deZone2Top2025).Div(decimal.NewFromInt(10000))
		// (176.64 * z + 2397) * z + 1015.13
		coeff := dec("176.64").Mul(z).Add(dec("2397"))
		return coeff.Mul(z).Add(dec("1015.13")).Round(2)
	}
	if annual.LessThanOrEqual(deZone4Top2025) {
		return annual.Mul(dec("0.42")).Sub(dec("10911.92")).Round(2)
	}
	return annual.Mul(dec("0.45")).Sub(dec("19246.67")).Round(2)
}

// deComputeSoli implements the Solidaritätszuschlag Milderungszone.
// Below the Freigrenze: 0. Above the slide-in: 5.5% of LSt.
// Inside the slide-in: min(11.9% * (LSt - Freigrenze), 5.5% * LSt).
func deComputeSoli(annualLohnsteuer decimal.Decimal) decimal.Decimal {
	if annualLohnsteuer.LessThanOrEqual(deSoliFreigrenze) {
		return decimal.Zero
	}
	full := annualLohnsteuer.Mul(deSoliFullRate)
	slide := annualLohnsteuer.Sub(deSoliFreigrenze).Mul(deSoliMilderungsRate)
	if slide.LessThan(full) {
		return slide
	}
	return full
}

// deResolveKirchensteuerRate returns the church-tax rate or zero.
// EmployeeInfo.PermitType encoding:
//   - "KIRCHE8" or "8" → 8% (Bavaria, Baden-Württemberg)
//   - "KIRCHE9" or "9" → 9% (rest of Germany)
//   - empty / other    → 0% (no affiliation)
func deResolveKirchensteuerRate(permitType string) decimal.Decimal {
	switch strings.ToUpper(strings.TrimSpace(permitType)) {
	case "KIRCHE8", "8":
		return dec("0.08")
	case "KIRCHE9", "9":
		return dec("0.09")
	}
	return decimal.Zero
}

// deComputeCappedShare returns rate * min(gross, max(0, cap - YTD)).
// Mirrors the YTD-aware cap pattern from usPack OASDI / chPack ALV.
func deComputeCappedShare(gross, ytd, rate, annualCap decimal.Decimal) decimal.Decimal {
	base := gross
	if ytd.GreaterThanOrEqual(annualCap) {
		return decimal.Zero
	}
	if ytd.Add(gross).GreaterThan(annualCap) {
		base = annualCap.Sub(ytd)
	}
	if base.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return base.Mul(rate).Round(2)
}
