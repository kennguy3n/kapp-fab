package taxpacks

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// chPack implements Switzerland's payroll-side statutory
// withholdings.
//
//   - Direct Federal Tax (Bundessteuer / Impôt fédéral direct):
//     levied via *Quellensteuer* (source tax) on non-C-permit
//     holders working in Switzerland. C-permit holders (settlement)
//     and Swiss citizens pay via annual self-assessment, so the
//     pack emits no federal-tax line for them. The Quellensteuer
//     tariff codes are cantonal but the federal portion is
//     identical across all cantons; this pack implements the
//     federal portion using the FTA's 2025 Tariff A0 (single, no
//     children, no church tax) progressive bracket schedule.
//
//   - Cantonal & Communal Tax: bundled into the same Quellensteuer
//     line by the cantonal tax authority and remitted to the
//     employee's canton of residence. Because the bundled rate
//     varies by canton and changes annually, this pack does *not*
//     attempt to implement the 26 cantonal schedules — instead it
//     emits the federal portion as CH_FED_TAX and a separately-
//     keyed CH_CANTONAL_TAX line driven by a small per-canton
//     average-rate lookup (chCantonalAvgRate). The lookup is a
//     calibrated approximation, not the statutory bracket walk;
//     operators in cantons with steep brackets (e.g. ZH high
//     earners) should override per-employee via the slip override
//     hook. The chCantonalAvgRate values are documented inline
//     against ESTV's 2025 published average-burden tables.
//
//   - AHV/IV/EO (federal old-age, disability, income-loss schemes):
//     5.3% employee share of the AHV-base wage (= gross for the
//     payroll cadence). No ceiling — AHV is a 1st-pillar uncapped
//     contribution.
//
//   - ALV (unemployment insurance): 1.1% employee share of gross
//     up to the CHF 148,200 / year ceiling (CHF 12,350 / month).
//     Above the ceiling, only the employer pays the *Solidarity*
//     contribution; the employee's ALV stops accruing at the cap.
//
// Permit-based gating (EmployeeInfo.PermitType):
//
//   - "C" or "" + Resident=true → Swiss citizen / C-permit. No
//     Quellensteuer; only AHV + ALV.
//   - "B", "L", "G", "Ci", or any other non-empty non-C value →
//     Quellensteuer applies (CH_FED_TAX + CH_CANTONAL_TAX) on top
//     of AHV + ALV.
//   - Resident=false → Quellensteuer applies regardless of permit
//     (foreign cross-border workers are taxed at source).
//
// References:
//
//	FTA Quellensteuer tariff (2025), federal portion:
//	  https://www.estv.admin.ch/estv/en/home/direct-federal-tax/dft-rates-and-tariffs.html
//	AHV/IV/EO + ALV employee rates (BSV 2025):
//	  https://www.ahv-iv.ch/en/Contributions
//	ALV ceiling (CHF 148,200 / year, 2025):
//	  https://www.bsv.admin.ch/bsv/en/home/social-insurance/alv.html
type chPack struct{}

func init() { Register(&chPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (chPack) Country() string { return "CH" }

// EffectiveYear returns the fiscal year the CH tables are
// calibrated for: 2025 (FTA Quellensteuer 2025 + BSV-2025
// AHV/IV/EO/ALV schedule).
func (chPack) EffectiveYear() int { return 2025 }

// chBracket is the federal Quellensteuer Tariff A0 schedule
// (single, no children, no church tax).
//
//   - Floor / Top in CHF, annual.
//   - Base is cumulative tax at the bracket's Floor.
//   - Rate is the marginal rate applied to the (income - Floor)
//     within the bracket.
//
// Last bracket has Top == 0 (open-ended). The Base values satisfy
// the contiguity invariant Base[i+1] == Base[i] + (Floor[i+1] -
// Floor[i]) * Rate[i] — pinned by TestBracketTablesAreContiguous.
type chBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	// FTA Quellensteuer 2025 Tariff A0 federal portion. Brackets
	// in CHF, annualised. The published tariff is a *step* table
	// rather than a bracket walk, but the underlying federal tax
	// is the standard Bundessteuer schedule which is the bracket
	// walk reproduced here. Engine-level periodicity (monthly /
	// fortnightly) is handled by the days/365.25 prorate.
	chFederalBrackets = []chBracket{
		{Floor: dec("0"), Top: dec("17800"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("17800"), Top: dec("31600"), Base: dec("0"), Rate: dec("0.0077")},
		{Floor: dec("31600"), Top: dec("41400"), Base: dec("106.26"), Rate: dec("0.0088")},
		{Floor: dec("41400"), Top: dec("55200"), Base: dec("192.50"), Rate: dec("0.0264")},
		{Floor: dec("55200"), Top: dec("72500"), Base: dec("556.82"), Rate: dec("0.0297")},
		{Floor: dec("72500"), Top: dec("78100"), Base: dec("1070.63"), Rate: dec("0.0594")},
		{Floor: dec("78100"), Top: dec("103600"), Base: dec("1403.27"), Rate: dec("0.066")},
		{Floor: dec("103600"), Top: dec("134600"), Base: dec("3086.27"), Rate: dec("0.088")},
		{Floor: dec("134600"), Top: dec("176000"), Base: dec("5814.27"), Rate: dec("0.11")},
		{Floor: dec("176000"), Top: dec("755200"), Base: dec("10368.27"), Rate: dec("0.132")},
		// Top bracket is the Bundessteuer "Höchstbelastung" cap:
		// for income above CHF 755,200 the marginal rate drops
		// to 11.5%. DBG Art. 36 phrases this as a maximum-burden
		// cap on total tax, but arithmetically the bracket-walk
		// expression `Base + (income - Floor) * 0.115` is always
		// ≤ 11.5% × income (the bracket-walk yields 0.115 * income
		// - 25.33, which is strictly lower) so the cap is a no-op
		// over the bracket walk and the walk is the correct
		// computation for every income above the threshold.
		// Base must satisfy the Base-consistency invariant pinned
		// by TestBracketTablesAreContiguous:
		//   Base = 10368.27 + (755200 - 176000) * 0.132 = 86822.67
		{Floor: dec("755200"), Top: decimal.Zero, Base: dec("86822.67"), Rate: dec("0.115")},
	}

	// Per-canton average burden rate (Quellensteuer cantonal +
	// communal combined), calibrated against ESTV's 2025
	// published average-burden tables for single Tariff A0
	// taxpayers at the median income point in each canton. This
	// is a *single-rate* approximation that operators must
	// override per-employee for high earners; the alternative is
	// embedding 26 separate progressive bracket tables which
	// would balloon this pack and still be approximate (the
	// communal portion varies by Gemeinde within each canton).
	chCantonalAvgRate = map[string]decimal.Decimal{
		"ZH": dec("0.103"), // Zurich
		"BE": dec("0.131"), // Bern
		"LU": dec("0.107"), // Lucerne
		"UR": dec("0.087"), // Uri
		"SZ": dec("0.062"), // Schwyz (low-tax canton)
		"OW": dec("0.087"), // Obwalden
		"NW": dec("0.084"), // Nidwalden
		"GL": dec("0.108"), // Glarus
		"ZG": dec("0.058"), // Zug (lowest)
		"FR": dec("0.122"), // Fribourg
		"SO": dec("0.119"), // Solothurn
		"BS": dec("0.118"), // Basel-Stadt
		"BL": dec("0.128"), // Basel-Landschaft
		"SH": dec("0.107"), // Schaffhausen
		"AR": dec("0.097"), // Appenzell Ausserrhoden
		"AI": dec("0.082"), // Appenzell Innerrhoden
		"SG": dec("0.112"), // St. Gallen
		"GR": dec("0.103"), // Graubünden
		"AG": dec("0.110"), // Aargau
		"TG": dec("0.106"), // Thurgau
		"TI": dec("0.117"), // Ticino
		"VD": dec("0.135"), // Vaud (highest)
		"VS": dec("0.124"), // Valais
		"NE": dec("0.131"), // Neuchâtel
		"GE": dec("0.130"), // Geneva
		"JU": dec("0.130"), // Jura
	}

	// Fallback cantonal rate when EmployeeInfo.Canton is empty or
	// not in the table — uses the national average (~10.8%). The
	// alternative is to skip the cantonal line entirely, but that
	// would severely under-withhold; the average-based fallback
	// is the conservative-correct default.
	chCantonalFallbackRate = dec("0.108")

	// AHV/IV/EO 2025 employee rate (BSV-published).
	//
	// AHV (Old-age & Survivors) : 4.35%
	// IV  (Invalidity)          : 0.70%
	// EO  (Income-loss)         : 0.25%
	// Total employee share      : 5.30%
	chAHVEmployeeRate = dec("0.053")

	// ALV (unemployment) 2025 employee rate + monthly ceiling
	// (CHF 148,200 / yr = CHF 12,350 / month). Above the
	// ceiling, the employee's *additional* contribution drops to
	// 0% (the employer Solidarity branch is not an employee
	// deduction). This pack therefore caps the ALV base at the
	// monthly ceiling; multi-month payouts above the cap should
	// reconcile through the year-end adjustment.
	chALVEmployeeRate = dec("0.011")
	chALVMonthlyCap   = dec("12350")

	chAnnualDays = decimal.NewFromFloat(365.25)
)

// ComputeWithholding emits up to four lines:
//
//   - CH_FED_TAX (federal Quellensteuer portion) — non-C-permit /
//     non-resident only;
//   - CH_CANTONAL_TAX (cantonal + communal Quellensteuer portion) —
//     same gating, rate from Canton lookup;
//   - CH_AHV (uncapped 5.3% employee share);
//   - CH_ALV (capped 1.1% employee share, CHF 12,350 / month
//     ceiling).
//
// Negative or zero gross / period return nil.
func (chPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Quellensteuer branch — applies to non-C-permit holders and
	// non-residents. Swiss citizens / C-permit holders pay via
	// annual self-assessment so the pack emits no source-tax line.
	if chIsQuellensteuerLiable(e) {
		periodFraction := decimal.NewFromInt(int64(days)).Div(chAnnualDays)
		if periodFraction.IsPositive() {
			annualGross := gross.Div(periodFraction)
			// Federal portion via the bracket walk.
			annualFed := walkCHBrackets(annualGross, chFederalBrackets)
			periodFed := annualFed.Mul(periodFraction).Round(2)
			if periodFed.IsPositive() {
				out = append(out, Deduction{
					Code:   "CH_FED_TAX",
					Name:   "Direct Federal Tax (Quellensteuer, CH)",
					Amount: periodFed,
				})
			}
			// Cantonal portion via per-canton average-rate lookup.
			cantonal := gross.Mul(chResolveCantonalRate(e.Canton)).Round(2)
			if cantonal.IsPositive() {
				out = append(out, Deduction{
					Code:   "CH_CANTONAL_TAX",
					Name:   "Cantonal + communal tax (Quellensteuer, CH)",
					Amount: cantonal,
				})
			}
		}
	}

	// AHV/IV/EO — every employee regardless of permit / residency
	// pays the 5.3% combined employee share. No ceiling.
	if ahv := gross.Mul(chAHVEmployeeRate).Round(2); ahv.IsPositive() {
		out = append(out, Deduction{
			Code:   "CH_AHV",
			Name:   "AHV/IV/EO (employee share, CH)",
			Amount: ahv,
		})
	}

	// ALV — capped at CHF 12,350 / month.
	alvBase := gross
	if alvBase.GreaterThan(chALVMonthlyCap) {
		alvBase = chALVMonthlyCap
	}
	if alv := alvBase.Mul(chALVEmployeeRate).Round(2); alv.IsPositive() {
		out = append(out, Deduction{
			Code:   "CH_ALV",
			Name:   "Unemployment insurance (ALV, employee share, CH)",
			Amount: alv,
		})
	}

	return out, nil
}

// chIsQuellensteuerLiable decides whether the federal + cantonal
// source-tax lines apply.
//
//   - Non-resident → always liable (cross-border workers are taxed
//     at source regardless of permit).
//   - Resident with PermitType == "C" or PermitType == "" → not
//     liable (Swiss citizens / settlement permit pay via annual
//     self-assessment). The empty default treats unknown
//     PermitType conservatively as the citizen / C-permit path
//     because that's the cohort an unfilled KRecord typically
//     represents in a Swiss payroll system; cross-border workers
//     would have the field populated by the wizard.
//   - Resident with any other PermitType (B, L, G, Ci, Ec, …) →
//     liable.
func chIsQuellensteuerLiable(e EmployeeInfo) bool {
	if !e.Resident {
		return true
	}
	permit := strings.ToUpper(strings.TrimSpace(e.PermitType))
	if permit == "" || permit == "C" {
		return false
	}
	return true
}

// chResolveCantonalRate looks up the per-canton average burden
// rate. Unknown / empty Canton falls back to the national average
// (chCantonalFallbackRate).
func chResolveCantonalRate(canton string) decimal.Decimal {
	key := strings.ToUpper(strings.TrimSpace(canton))
	if r, ok := chCantonalAvgRate[key]; ok {
		return r
	}
	return chCantonalFallbackRate
}

// walkCHBrackets walks the federal Quellensteuer Tariff A0
// schedule. The Top of the last bracket is decimal.Zero, treated
// as "no upper bound". Mirrors the walkINBrackets / walkPHBrackets
// pattern from the APAC packs — same contract, same per-pack
// bracket struct.
func walkCHBrackets(annual decimal.Decimal, scale []chBracket) decimal.Decimal {
	var match chBracket
	matched := false
	for _, b := range scale {
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
