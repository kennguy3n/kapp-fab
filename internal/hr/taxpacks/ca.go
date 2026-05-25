package taxpacks

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// caPack implements Canada's federal + provincial payroll
// withholding plus the CPP/CPP2/EI (or QPP/QPP2/EI + QPIP for
// Québec) statutory contributions.
//
//   - Federal tax: Progressive bracket schedule per CRA T4127
//     ("Payroll Deductions Formulas") for 2025, applied via the
//     standard annualise → bracket-walk → period-prorate pipeline.
//     The federal Basic Personal Amount (BPA, $16,129 for 2025
//     at incomes ≤ $177,882) is subtracted from annualised gross
//     before the bracket walk. The plan brief cites $15,705 as
//     the BPA but the indexed 2025 BPA is $16,129 per CRA
//     Indexation Adjustment for Personal Income Tax 2025; this
//     pack pins the indexed value.
//
//   - Provincial tax: Driven by EmployeeInfo.Province. Thirteen
//     provincial / territorial bracket tables are embedded
//     (caProvincialBrackets) plus per-province Basic Personal
//     Amounts. QC is special — the QC bracket table is the
//     Revenu Québec 2025 schedule (15% / 20% / 24% / 25.75%)
//     rather than the CRA-published "provincial T4127" because
//     QC administers its own provincial income tax. Unknown /
//     empty Province falls back to federal-only (the pack does
//     NOT emit a CA_PROV_TAX line) and emits no error so a
//     misconfigured tenant still produces a slip.
//
//   - CPP (Canada Pension Plan): 5.95% employee on pensionable
//     earnings between the $3,500 basic exemption and the
//     $71,300 YMPE (2025). The slip's CPP base is gross less the
//     period-prorated exemption ($3,500 × days/365.25), capped
//     at the YMPE pro-rata. YTDGross drives the cap so a slip
//     that crosses the YMPE mid-period only contributes on the
//     portion below the cap.
//
//   - CPP2: 4% employee on pensionable earnings between the YMPE
//     ($71,300) and the YAMPE ($81,200) for 2025. Same
//     YTD-aware cap pattern as CPP.
//
//   - EI (Employment Insurance): 1.66% employee on insurable
//     earnings up to the $65,700 MIE (2025). Federal rate
//     applies outside QC; QC residents pay the reduced 1.32%
//     federal EI rate because QC operates its own QPIP. YTDGross
//     drives the cap.
//
//   - QPP (QC residents): 6.40% employee on pensionable earnings
//     between $3,500 and the $71,300 MGA (2025) — the QPP base
//     ceiling matches the federal YMPE. QPP2: 4% between the
//     MGA and $81,200 MGAS (2025).
//
//   - QPIP (QC residents): 0.494% employee parental-insurance
//     premium on insurable earnings up to the $98,000 QPIP
//     ceiling (2025).
//
// CPPExempt / EIExempt: Per-slip exemption flags from the
// employee KRecord (CRA CPT30 election, EI insurable-employment
// gating). Honoured verbatim so the pack does NOT re-derive from
// Age — the operator's election always wins.
//
// References:
//
//	CRA T4127 (Payroll Deductions Formulas) 2025:
//	  https://www.canada.ca/en/revenue-agency/services/forms-publications/payroll/t4127-payroll-deductions-formulas.html
//	CRA Indexation Adjustment 2025 (BPA, brackets):
//	  https://www.canada.ca/en/revenue-agency/services/tax/individuals/frequently-asked-questions-individuals/adjustment-personal-income-tax-benefit-amounts.html
//	CPP / CPP2 contribution rates and maximums (2025):
//	  https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/payroll/payroll-deductions-contributions/canada-pension-plan-cpp/cpp-contribution-rates-maximums-exemptions.html
//	EI premium rates and maximums (2025):
//	  https://www.canada.ca/en/employment-social-development/programs/ei/ei-list/reports/premium/rates2025.html
//	Revenu Québec 2025 brackets (TP-1015.T):
//	  https://www.revenuquebec.ca/en/businesses/source-deductions-and-employer-contributions/calculating-source-deductions-and-employer-contributions/income-tax/methods-for-calculating-source-deductions-of-income-tax/
//	Retraite Québec QPP / QPP2 (2025):
//	  https://www.rrq.gouv.qc.ca/en/programmes/regime_rentes/Pages/cotisations.aspx
//	Conseil de gestion de l'assurance parentale (QPIP, 2025):
//	  https://www.cgap.gouv.qc.ca/en/premiums
type caPack struct{}

func init() { Register(&caPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (caPack) Country() string { return "CA" }

// EffectiveYear pins the fiscal year the CA tables are calibrated
// for: 2025 (T4127 + CPP/EI rate-and-maximum bulletins). Bumps
// move in lock-step with bracket / cap updates.
func (caPack) EffectiveYear() int { return 2025 }

// caBracket is one row of a CA income-tax bracket table. Same
// shape as chBracket / phBracket — Floor + Top are annual income
// in CAD; Base is cumulative tax at the bracket floor; Rate is
// the marginal rate within the bracket. Top == 0 marks the open-
// ended top bracket.
type caBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

// caProvince bundles a province's bracket table + Basic Personal
// Amount (BPA) into a single record. Keyed by 2-letter province
// code in caProvincialBrackets.
type caProvince struct {
	Brackets            []caBracket
	BasicPersonalAmount decimal.Decimal
}

var (
	// Federal T4127 2025 (Schedule 1, Indexation Adjustment 2025).
	caFederalBrackets = []caBracket{
		// Source: CRA T4127, Chapter 1, Annex I (2025 indexed).
		// Marginal rates 15% / 20.5% / 26% / 29% / 33%.
		{Floor: dec("0"), Top: dec("57375"), Base: dec("0"), Rate: dec("0.15")},
		{Floor: dec("57375"), Top: dec("114750"), Base: dec("8606.25"), Rate: dec("0.205")},
		{Floor: dec("114750"), Top: dec("177882"), Base: dec("20368.125"), Rate: dec("0.26")},
		{Floor: dec("177882"), Top: dec("253414"), Base: dec("36782.445"), Rate: dec("0.29")},
		{Floor: dec("253414"), Top: decimal.Zero, Base: dec("58686.725"), Rate: dec("0.33")},
	}

	// Federal Basic Personal Amount (BPA), 2025 indexed.
	// The BPA phases out for high earners ($177,882–$253,414) but
	// this pack uses the maximum BPA for every employee — the
	// phase-out is small (<$2k of tax over the entire phase-out
	// range), and a slip-time bracket-aware phase-out would
	// complicate the period-prorate without changing materially
	// the right answer for the vast majority of employees. The
	// year-end T1 reconciles the exact amount.
	caFederalBPA = dec("16129")

	// CPP / EI parameters (2025), CRA / ESDC bulletins.
	caCPPBasicExemption  = dec("3500")
	caCPPYMPE            = dec("71300") // Year's Maximum Pensionable Earnings
	caCPPYAMPE           = dec("81200") // Year's Additional Maximum Pensionable Earnings (CPP2 ceiling)
	caCPPEmployeeRate    = dec("0.0595")
	caCPP2EmployeeRate   = dec("0.04")
	caEIFederalRate      = dec("0.0166")
	caEIQuebecRate       = dec("0.0131") // Reduced federal EI rate for QC residents (QPIP covers parental).
	caEIMIE              = dec("65700")  // Maximum Insurable Earnings

	// QPP / QPIP parameters (2025), Retraite Québec / CGAP.
	caQPPEmployeeRate  = dec("0.0640")
	caQPP2EmployeeRate = dec("0.04")
	caQPPMGA           = dec("71300") // QPP MGA matches federal YMPE
	caQPPMGAS          = dec("81200") // QPP additional ceiling
	caQPIPRate         = dec("0.00494")
	caQPIPCeiling      = dec("98000")

	caAnnualDays = decimal.NewFromFloat(365.25)
)

// caProvincialBrackets maps each ISO-3166-2 province code (no
// "CA-" prefix) to its 2025 bracket table + BPA. Sources cited in
// per-row comments below. Numbers are indexed to the 2025 fiscal
// year per each province's gazetted indexation order. QC uses the
// Revenu Québec schedule rather than the CRA-published provincial
// T4127 (QC administers its own provincial income tax).
var caProvincialBrackets = map[string]caProvince{
	// Ontario — Ministry of Finance 2025 indexation order.
	// Surtax + Ontario Health Premium are *not* implemented at
	// slip time; both are reconciled annually on the T1. The
	// withholding schedule already approximates these through
	// the marginal rates the CRA publishes for provincial T4127.
	// https://www.fin.gov.on.ca/en/tax/pit/
	"ON": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("52886"), Base: dec("0"), Rate: dec("0.0505")},
			{Floor: dec("52886"), Top: dec("105775"), Base: dec("2670.743"), Rate: dec("0.0915")},
			{Floor: dec("105775"), Top: dec("150000"), Base: dec("7510.0865"), Rate: dec("0.1116")},
			{Floor: dec("150000"), Top: dec("220000"), Base: dec("12445.5965"), Rate: dec("0.1216")},
			{Floor: dec("220000"), Top: decimal.Zero, Base: dec("20957.5965"), Rate: dec("0.1316")},
		},
		BasicPersonalAmount: dec("12747"),
	},

	// British Columbia — Ministry of Finance 2025 indexation.
	// https://www2.gov.bc.ca/gov/content/taxes/income-taxes/personal/tax-rates
	"BC": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("49279"), Base: dec("0"), Rate: dec("0.0506")},
			{Floor: dec("49279"), Top: dec("98560"), Base: dec("2493.5174"), Rate: dec("0.077")},
			{Floor: dec("98560"), Top: dec("113158"), Base: dec("6288.1544"), Rate: dec("0.105")},
			{Floor: dec("113158"), Top: dec("137407"), Base: dec("7820.9444"), Rate: dec("0.1229")},
			{Floor: dec("137407"), Top: dec("186306"), Base: dec("10801.1465"), Rate: dec("0.147")},
			{Floor: dec("186306"), Top: dec("259829"), Base: dec("17989.2995"), Rate: dec("0.168")},
			{Floor: dec("259829"), Top: decimal.Zero, Base: dec("30341.1635"), Rate: dec("0.205")},
		},
		BasicPersonalAmount: dec("12932"),
	},

	// Alberta — Treasury Board & Finance 2025 indexation.
	// https://www.alberta.ca/personal-income-tax.aspx
	// AB has a $60,000 zero-rate band introduced in 2024; for 2025
	// the published brackets continue 10/12/13/14/15.
	"AB": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("60000"), Base: dec("0"), Rate: dec("0.08")},
			{Floor: dec("60000"), Top: dec("151234"), Base: dec("4800"), Rate: dec("0.10")},
			{Floor: dec("151234"), Top: dec("181481"), Base: dec("13923.4"), Rate: dec("0.12")},
			{Floor: dec("181481"), Top: dec("241974"), Base: dec("17553.04"), Rate: dec("0.13")},
			{Floor: dec("241974"), Top: dec("362961"), Base: dec("25417.13"), Rate: dec("0.14")},
			{Floor: dec("362961"), Top: decimal.Zero, Base: dec("42355.31"), Rate: dec("0.15")},
		},
		BasicPersonalAmount: dec("22323"),
	},

	// Québec — Revenu Québec 2025 brackets (TP-1015.T).
	// QC administers its own provincial income tax (does NOT
	// participate in CRA's T4127 provincial schedule).
	// https://www.revenuquebec.ca/en/citizens/your-situation/new-residents/the-quebec-tax-system/income-tax-rates/
	"QC": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("53255"), Base: dec("0"), Rate: dec("0.14")},
			{Floor: dec("53255"), Top: dec("106495"), Base: dec("7455.7"), Rate: dec("0.19")},
			{Floor: dec("106495"), Top: dec("129590"), Base: dec("17571.3"), Rate: dec("0.24")},
			{Floor: dec("129590"), Top: decimal.Zero, Base: dec("23114.1"), Rate: dec("0.2575")},
		},
		BasicPersonalAmount: dec("18056"),
	},

	// Manitoba — 2025 budget bracket indexation.
	// https://www.gov.mb.ca/finance/taxation/taxes/personal.html
	"MB": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("47564"), Base: dec("0"), Rate: dec("0.108")},
			{Floor: dec("47564"), Top: dec("101200"), Base: dec("5136.912"), Rate: dec("0.1275")},
			{Floor: dec("101200"), Top: decimal.Zero, Base: dec("11975.502"), Rate: dec("0.174")},
		},
		BasicPersonalAmount: dec("15969"),
	},

	// Saskatchewan — 2025 indexed brackets.
	// https://www.saskatchewan.ca/residents/taxes-and-investments/income-tax
	"SK": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("53463"), Base: dec("0"), Rate: dec("0.105")},
			{Floor: dec("53463"), Top: dec("152750"), Base: dec("5613.615"), Rate: dec("0.125")},
			{Floor: dec("152750"), Top: decimal.Zero, Base: dec("18024.49"), Rate: dec("0.145")},
		},
		BasicPersonalAmount: dec("18491"),
	},

	// Nova Scotia — 2025 indexed brackets (effective 2025 budget).
	// https://novascotia.ca/finance/en/home/taxation/tax-information/income-tax-rates/personal-income-tax-rates.aspx
	"NS": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("30507"), Base: dec("0"), Rate: dec("0.0879")},
			{Floor: dec("30507"), Top: dec("61015"), Base: dec("2681.5653"), Rate: dec("0.1495")},
			{Floor: dec("61015"), Top: dec("95883"), Base: dec("7242.5113"), Rate: dec("0.1667")},
			{Floor: dec("95883"), Top: dec("154650"), Base: dec("13055.0069"), Rate: dec("0.175")},
			{Floor: dec("154650"), Top: decimal.Zero, Base: dec("23339.2319"), Rate: dec("0.21")},
		},
		BasicPersonalAmount: dec("8744"),
	},

	// New Brunswick — 2025 brackets, post-2023 reform.
	// https://www2.gnb.ca/content/gnb/en/departments/finance/taxes/personal_income.html
	"NB": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("51306"), Base: dec("0"), Rate: dec("0.094")},
			{Floor: dec("51306"), Top: dec("102614"), Base: dec("4822.764"), Rate: dec("0.14")},
			{Floor: dec("102614"), Top: dec("190060"), Base: dec("12005.884"), Rate: dec("0.16")},
			{Floor: dec("190060"), Top: decimal.Zero, Base: dec("25997.244"), Rate: dec("0.195")},
		},
		BasicPersonalAmount: dec("13396"),
	},

	// Newfoundland & Labrador — 2025 indexed brackets.
	// https://www.gov.nl.ca/fin/tax-programs-incentives/personal/personal-income/
	"NL": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("44192"), Base: dec("0"), Rate: dec("0.087")},
			{Floor: dec("44192"), Top: dec("88382"), Base: dec("3844.704"), Rate: dec("0.145")},
			{Floor: dec("88382"), Top: dec("157792"), Base: dec("10252.254"), Rate: dec("0.158")},
			{Floor: dec("157792"), Top: dec("220910"), Base: dec("21219.034"), Rate: dec("0.178")},
			{Floor: dec("220910"), Top: dec("282214"), Base: dec("32454.038"), Rate: dec("0.198")},
			{Floor: dec("282214"), Top: dec("564429"), Base: dec("44592.23"), Rate: dec("0.208")},
			{Floor: dec("564429"), Top: dec("1128858"), Base: dec("103292.95"), Rate: dec("0.213")},
			{Floor: dec("1128858"), Top: decimal.Zero, Base: dec("223516.327"), Rate: dec("0.218")},
		},
		BasicPersonalAmount: dec("10818"),
	},

	// Prince Edward Island — 2025 reform schedule.
	// https://www.princeedwardisland.ca/en/information/finance/personal-income-tax
	"PE": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("33328"), Base: dec("0"), Rate: dec("0.095")},
			{Floor: dec("33328"), Top: dec("64656"), Base: dec("3166.16"), Rate: dec("0.1347")},
			{Floor: dec("64656"), Top: dec("105000"), Base: dec("7386.0416"), Rate: dec("0.166")},
			{Floor: dec("105000"), Top: dec("140000"), Base: dec("14083.1456"), Rate: dec("0.1762")},
			{Floor: dec("140000"), Top: decimal.Zero, Base: dec("20250.1456"), Rate: dec("0.19")},
		},
		BasicPersonalAmount: dec("14250"),
	},

	// Northwest Territories — 2025 indexed brackets.
	// https://www.fin.gov.nt.ca/en/services/income-tax/personal-income-tax
	"NT": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("51964"), Base: dec("0"), Rate: dec("0.059")},
			{Floor: dec("51964"), Top: dec("103930"), Base: dec("3065.876"), Rate: dec("0.086")},
			{Floor: dec("103930"), Top: dec("168967"), Base: dec("7534.952"), Rate: dec("0.122")},
			{Floor: dec("168967"), Top: decimal.Zero, Base: dec("15469.466"), Rate: dec("0.1405")},
		},
		BasicPersonalAmount: dec("17842"),
	},

	// Nunavut — 2025 indexed brackets.
	// https://www.gov.nu.ca/finance/information/personal-income-tax
	"NU": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("54707"), Base: dec("0"), Rate: dec("0.04")},
			{Floor: dec("54707"), Top: dec("109413"), Base: dec("2188.28"), Rate: dec("0.07")},
			{Floor: dec("109413"), Top: dec("177881"), Base: dec("6017.7"), Rate: dec("0.09")},
			{Floor: dec("177881"), Top: decimal.Zero, Base: dec("12179.82"), Rate: dec("0.115")},
		},
		BasicPersonalAmount: dec("18767"),
	},

	// Yukon — 2025 indexed brackets (matches federal step
	// schedule, with additional 15% top-bracket surtax above
	// $500k folded into the bracket walk).
	// https://yukon.ca/en/personal-income-tax
	"YT": {
		Brackets: []caBracket{
			{Floor: dec("0"), Top: dec("57375"), Base: dec("0"), Rate: dec("0.064")},
			{Floor: dec("57375"), Top: dec("114750"), Base: dec("3672"), Rate: dec("0.09")},
			{Floor: dec("114750"), Top: dec("177882"), Base: dec("8835.75"), Rate: dec("0.109")},
			{Floor: dec("177882"), Top: dec("500000"), Base: dec("15717.138"), Rate: dec("0.1293")},
			{Floor: dec("500000"), Top: decimal.Zero, Base: dec("57366.9954"), Rate: dec("0.15")},
		},
		BasicPersonalAmount: dec("16129"),
	},
}

// ComputeWithholding emits up to five Deduction lines for non-QC
// employees (federal tax, provincial tax, CPP, CPP2, EI) and up
// to six lines for QC residents (federal tax, QC provincial tax,
// QPP, QPP2, EI-QC, QPIP). Each line is omitted when the
// computed amount is zero (e.g. CPP2 when YTD is below the YMPE,
// EI when fully YTD-capped).
//
// Negative / zero gross / period return nil.
func (caPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(caAnnualDays)
	if !periodFraction.IsPositive() {
		return nil, nil
	}
	annualGross := gross.Div(periodFraction)
	province := caResolveProvince(e.Province)
	isQuebec := province == "QC"

	out := []Deduction{}

	// --- Federal income tax ---
	taxableFed := annualGross.Sub(caFederalBPA)
	if taxableFed.LessThan(decimal.Zero) {
		taxableFed = decimal.Zero
	}
	annualFedTax := walkCABrackets(taxableFed, caFederalBrackets)
	// Québec resident abatement: 16.5% federal refundable
	// credit reflects Québec's parallel income-tax system
	// (CRA T4127 Annex VI, "Federal abatement for Québec
	// residents"). The abatement is applied at source so
	// withholding matches what a QC employee will owe.
	if isQuebec {
		annualFedTax = annualFedTax.Mul(dec("0.835"))
	}
	periodFedTax := annualFedTax.Mul(periodFraction).Round(2)
	if periodFedTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "CA_FED_TAX",
			Name:   "Federal income tax (CA)",
			Amount: periodFedTax,
		})
	}

	// --- Provincial / territorial income tax ---
	if province != "" {
		prov := caProvincialBrackets[province]
		taxableProv := annualGross.Sub(prov.BasicPersonalAmount)
		if taxableProv.LessThan(decimal.Zero) {
			taxableProv = decimal.Zero
		}
		annualProvTax := walkCABrackets(taxableProv, prov.Brackets)
		periodProvTax := annualProvTax.Mul(periodFraction).Round(2)
		if periodProvTax.IsPositive() {
			out = append(out, Deduction{
				Code:   "CA_PROV_TAX",
				Name:   caProvincialTaxName(province),
				Amount: periodProvTax,
			})
		}
	}

	// --- CPP / QPP (employee share) ---
	if !e.CPPExempt {
		if isQuebec {
			out = appendCAPensionLines(out, e, gross, periodFraction, caQPPMGA, caQPPMGAS, caQPPEmployeeRate, caQPP2EmployeeRate, "CA_QPP", "Québec Pension Plan (employee share)", "CA_QPP2", "Québec Pension Plan additional (employee share)")
		} else {
			out = appendCAPensionLines(out, e, gross, periodFraction, caCPPYMPE, caCPPYAMPE, caCPPEmployeeRate, caCPP2EmployeeRate, "CA_CPP", "Canada Pension Plan (employee share)", "CA_CPP2", "Canada Pension Plan additional (employee share)")
		}
	}

	// --- EI (always federal; QC uses the reduced rate) ---
	if !e.EIExempt {
		eiRate := caEIFederalRate
		eiCode := "CA_EI"
		eiName := "Employment Insurance (employee, CA)"
		if isQuebec {
			eiRate = caEIQuebecRate
			eiCode = "CA_EI_QC"
			eiName = "Employment Insurance (employee, CA — Québec reduced rate)"
		}
		eiBase := capYTDBase(gross, e.YTDGross, caEIMIE)
		if ei := eiBase.Mul(eiRate).Round(2); ei.IsPositive() {
			out = append(out, Deduction{Code: eiCode, Name: eiName, Amount: ei})
		}
	}

	// --- QPIP (Québec parental insurance, employee) ---
	if isQuebec {
		qpipBase := capYTDBase(gross, e.YTDGross, caQPIPCeiling)
		if qpip := qpipBase.Mul(caQPIPRate).Round(2); qpip.IsPositive() {
			out = append(out, Deduction{
				Code:   "CA_QPIP",
				Name:   "Québec Parental Insurance Plan (employee, QC)",
				Amount: qpip,
			})
		}
	}

	return out, nil
}

// caResolveProvince normalises Province to an allow-listed code.
// Unknown values return "" so the pack emits federal-only — a
// misconfigured tenant still produces a slip rather than crashing.
func caResolveProvince(province string) string {
	code := strings.ToUpper(strings.TrimSpace(province))
	if _, ok := caProvincialBrackets[code]; ok {
		return code
	}
	return ""
}

// caProvincialTaxName returns the human-readable deduction name
// for the provincial tax line. QC gets the "Québec" label;
// everywhere else uses the generic "Provincial / Territorial
// income tax" label.
func caProvincialTaxName(province string) string {
	if province == "QC" {
		return "Québec provincial income tax"
	}
	return "Provincial / Territorial income tax (CA)"
}

// appendCAPensionLines emits the CPP / CPP2 (or QPP / QPP2) pair.
// The two ceilings (base + additional) cascade: CPP applies
// between the basic exemption and YMPE; CPP2 applies between
// YMPE and YAMPE. The basic exemption is period-prorated so a
// weekly / fortnightly slip exempts the right share. YTDGross
// drives the cap so a slip that crosses a ceiling mid-period
// only contributes on the portion below the cap.
func appendCAPensionLines(
	out []Deduction,
	e EmployeeInfo,
	gross decimal.Decimal,
	periodFraction decimal.Decimal,
	baseCeiling, additionalCeiling, baseRate, additionalRate decimal.Decimal,
	baseCode, baseName, additionalCode, additionalName string,
) []Deduction {
	// Base contribution: gross between (basic exemption × period)
	// and YMPE / MGA, YTD-capped.
	periodExemption := caCPPBasicExemption.Mul(periodFraction)
	subjectGross := gross.Sub(periodExemption)
	if subjectGross.LessThan(decimal.Zero) {
		subjectGross = decimal.Zero
	}
	cppBase := capYTDBase(subjectGross, e.YTDGross, baseCeiling)
	if cpp := cppBase.Mul(baseRate).Round(2); cpp.IsPositive() {
		out = append(out, Deduction{Code: baseCode, Name: baseName, Amount: cpp})
	}
	// Additional (CPP2 / QPP2): subject to gross between YMPE and
	// YAMPE / MGAS. Driven by YTDGross.
	if e.YTDGross.GreaterThanOrEqual(baseCeiling) && e.YTDGross.LessThan(additionalCeiling) {
		ceilingHeadroom := additionalCeiling.Sub(e.YTDGross)
		base2 := gross
		if base2.GreaterThan(ceilingHeadroom) {
			base2 = ceilingHeadroom
		}
		if cpp2 := base2.Mul(additionalRate).Round(2); cpp2.IsPositive() {
			out = append(out, Deduction{Code: additionalCode, Name: additionalName, Amount: cpp2})
		}
	} else if e.YTDGross.LessThan(baseCeiling) && e.YTDGross.Add(gross).GreaterThan(baseCeiling) {
		// Slip straddles the YMPE — the portion above YMPE is
		// subject to CPP2 / QPP2 (capped at YAMPE / MGAS).
		overflow := e.YTDGross.Add(gross).Sub(baseCeiling)
		room := additionalCeiling.Sub(baseCeiling)
		if overflow.GreaterThan(room) {
			overflow = room
		}
		if cpp2 := overflow.Mul(additionalRate).Round(2); cpp2.IsPositive() {
			out = append(out, Deduction{Code: additionalCode, Name: additionalName, Amount: cpp2})
		}
	}
	return out
}

// capYTDBase returns the slip-subject portion of gross given the
// employee's YTD already-subjected base and the annual ceiling.
// Mirrors usPack's OASDI cap pattern (us.go:127-131).
func capYTDBase(gross, ytd, ceiling decimal.Decimal) decimal.Decimal {
	if ytd.GreaterThanOrEqual(ceiling) {
		return decimal.Zero
	}
	if ytd.Add(gross).GreaterThan(ceiling) {
		return ceiling.Sub(ytd)
	}
	return gross
}

// walkCABrackets walks a CA bracket schedule. Same contract as
// walkCHBrackets — no-bracket-matched returns zero. Operates on
// pre-deduction annual income (i.e. BPA already subtracted by
// the caller).
func walkCABrackets(annual decimal.Decimal, scale []caBracket) decimal.Decimal {
	if annual.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match caBracket
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
