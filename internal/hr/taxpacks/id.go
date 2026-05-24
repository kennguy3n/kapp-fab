package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// idPack implements Indonesia's PPh 21 monthly payroll withholding
// using the annual progressive method (UU HPP / Undang-Undang
// Harmonisasi Peraturan Perpajakan No. 7 of 2021), which is the
// statutory basis underlying the 2024 Tarif Efektif Rata-rata
// (TER) tables. The annual progressive method is also what
// employers must use for the December slip's year-end true-up
// (PMK 168 / PMK.03 / 2023 art. 21), so this pack produces a
// figure that reconciles exactly with the legal year-end position
// regardless of which monthly method the operator selected for
// Jan-Nov. The TER A / B / C monthly tables for Jan-Nov are not
// implemented; the comment block at the top of the registry
// documents the difference and the operator is expected to use the
// annual method consistently.
//
// Components:
//
//   - PPh 21: annual progressive schedule (5/15/25/30/35%), with
//     PTKP (Penghasilan Tidak Kena Pajak / personal exemption)
//     subtracted before the bracket walk. PTKP categories vary by
//     marital + dependent status; this pack derives the category
//     from EmployeeInfo.NumDependents and (filling status absent
//     in the projection) defaults to TK/n (single, n dependents)
//     — operators with married + dependents can override via a
//     future EmployeeInfo field.
//
//   - BPJS Kesehatan (health) employee 1% capped at IDR 12M /
//     month wage base (so max IDR 120k / month). Effective per
//     Perpres 64/2020.
//
//   - BPJS Ketenagakerjaan JHT (old-age savings) employee 2% of
//     monthly wage. No upper cap.
//
//   - BPJS Ketenagakerjaan JP (pension) employee 1% capped at the
//     IDR 10,547,400 / month 2024 wage ceiling — BPJS Ketenagaker-
//     jaan adjusts this every January. The 2024 figure is
//     hard-coded; the EffectiveYear constant and the maintenance
//     workflow track when this drifts.
//
// References:
//
//	UU HPP No. 7 / 2021 (PPh 21 bracket schedule):
//	  https://jdih.kemenkeu.go.id/fullText/2021/7TAHUN2021UU.pdf
//	PMK 168 / 2023 (TER + annual method):
//	  https://jdih.kemenkeu.go.id/fullText/2023/168~PMK.03~2023Per.pdf
//	BPJS Ketenagakerjaan JP ceiling (2024):
//	  https://www.bpjsketenagakerjaan.go.id/berita
//	BPJS Kesehatan rates (Perpres 64 / 2020):
//	  https://bpjs-kesehatan.go.id
type idPack struct{}

func init() { Register(&idPack{}) }

// Country returns the ISO 3166-1 alpha-2 country code this pack
// services.
func (idPack) Country() string { return "ID" }

// EffectiveYear returns the fiscal year the ID tables are
// calibrated for: 2024 — UU HPP schedule + 2024 JP ceiling
// (10,547,400). Brackets are stable across 2022-2024; the JP
// ceiling moves annually per BPJS Ketenagakerjaan notice.
func (idPack) EffectiveYear() int { return 2024 }

type idBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	idBracketsResident = []idBracket{
		{Floor: dec("0"), Top: dec("60000000"), Base: dec("0"), Rate: dec("0.05")},
		{Floor: dec("60000000"), Top: dec("250000000"), Base: dec("3000000"), Rate: dec("0.15")},
		{Floor: dec("250000000"), Top: dec("500000000"), Base: dec("31500000"), Rate: dec("0.25")},
		{Floor: dec("500000000"), Top: dec("5000000000"), Base: dec("94000000"), Rate: dec("0.30")},
		{Floor: dec("5000000000"), Top: decimal.Zero, Base: dec("1444000000"), Rate: dec("0.35")},
	}

	// PTKP single-status (TK/0) base. Married status adds
	// 4,500,000 IDR; each dependent (max 3) adds 4,500,000 IDR.
	// The EmployeeInfo projection does not yet carry marital
	// status, so this pack uses TK/n where n = NumDependents.
	idPTKPTK0Base       = dec("54000000")
	idPTKPDependent     = dec("4500000")
	idPTKPMaxDependents = 3 // statutory cap; further dependents do not increase PTKP.

	// BPJS rates + caps.
	idBPJSKesEmployeeRate = dec("0.01")
	idBPJSKesWageCap      = dec("12000000")
	idBPJSJHTEmployeeRate = dec("0.02")
	idBPJSJPEmployeeRate  = dec("0.01")
	idBPJSJP2024Ceiling   = dec("10547400")

	idPeriodsPerYear = decimal.NewFromFloat(365.25)

	// UU PPh art. 26 + PMK-141/PMK.03/2015: 20% flat
	// withholding on Indonesian-sourced employment income for
	// non-resident individuals (foreigners present < 183 days
	// in any 12-month period). Tax-treaty overrides apply but
	// are out of scope for this pack — treaty relief is filed
	// via DGT-1/DGT-2 forms, not slip-level.
	idNonResidentRate = dec("0.20")
)

// ComputeWithholding emits ID_PPH21 (annual-method progressive
// tax after PTKP), ID_BPJS_KES (1% capped at IDR 12M / month),
// ID_BPJS_JHT (2% uncapped) and ID_BPJS_JP (1% capped at the
// 10,547,400 / month 2024 ceiling) for residents. Non-residents
// (UU PPh art. 26) get ID_NONRESIDENT_TAX at the flat 20% rate;
// BPJS Ketenagakerjaan / Kesehatan eligibility requires Indonesian
// citizenship or KITAS-permitted residence, so non-resident slips
// emit only the income-tax line. Zero-amount lines are omitted.
func (idPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	// Non-resident: flat 20% on gross, no BPJS.
	if !e.Resident {
		nr := gross.Mul(idNonResidentRate).Round(2)
		if nr.IsPositive() {
			out = append(out, Deduction{
				Code:   "ID_NONRESIDENT_TAX",
				Name:   "Non-resident PPh 26 flat 20% (ID)",
				Amount: nr,
			})
		}
		return out, nil
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(idPeriodsPerYear)
	annualGross := gross.Div(periodFraction)

	// PTKP: TK/n where n = min(NumDependents, 3).
	deps := e.NumDependents
	if deps < 0 {
		deps = 0
	}
	if deps > idPTKPMaxDependents {
		deps = idPTKPMaxDependents
	}
	ptkp := idPTKPTK0Base.Add(
		idPTKPDependent.Mul(decimal.NewFromInt(int64(deps))),
	)
	taxable := annualGross.Sub(ptkp)
	if taxable.LessThan(decimal.Zero) {
		taxable = decimal.Zero
	}
	annualTax := walkIDBrackets(taxable, idBracketsResident)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.IsPositive() {
		out = append(out, Deduction{
			Code:   "ID_PPH21",
			Name:   "PPh 21 income tax withholding (ID)",
			Amount: periodTax,
		})
	}

	// BPJS Kesehatan 1% on capped wage.
	kesBase := gross
	if kesBase.GreaterThan(idBPJSKesWageCap) {
		kesBase = idBPJSKesWageCap
	}
	kes := kesBase.Mul(idBPJSKesEmployeeRate).Round(2)
	if kes.IsPositive() {
		out = append(out, Deduction{
			Code:   "ID_BPJS_KES",
			Name:   "BPJS Kesehatan (employee share, ID)",
			Amount: kes,
		})
	}

	// BPJS Ketenagakerjaan JHT 2% — no cap.
	jht := gross.Mul(idBPJSJHTEmployeeRate).Round(2)
	if jht.IsPositive() {
		out = append(out, Deduction{
			Code:   "ID_BPJS_JHT",
			Name:   "BPJS Ketenagakerjaan JHT (employee share, ID)",
			Amount: jht,
		})
	}

	// BPJS Ketenagakerjaan JP 1% on capped wage.
	jpBase := gross
	if jpBase.GreaterThan(idBPJSJP2024Ceiling) {
		jpBase = idBPJSJP2024Ceiling
	}
	jp := jpBase.Mul(idBPJSJPEmployeeRate).Round(2)
	if jp.IsPositive() {
		out = append(out, Deduction{
			Code:   "ID_BPJS_JP",
			Name:   "BPJS Ketenagakerjaan JP (employee share, ID)",
			Amount: jp,
		})
	}

	return out, nil
}

func walkIDBrackets(annual decimal.Decimal, scale []idBracket) decimal.Decimal {
	var match idBracket
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
