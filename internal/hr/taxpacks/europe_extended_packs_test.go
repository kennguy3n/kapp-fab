package taxpacks

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// zeroDayPeriod returns a PayPeriod whose End strictly precedes
// Start so PayPeriod.Days() returns 0. Used for the zero-period
// edge case across all Phase-N2 packs.
func zeroDayPeriod() PayPeriod {
	return PayPeriod{
		Start: time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// europe_extended_packs_test.go — regression matrices for the 9
// Phase-N2 European packs (PL, SE, NO, DK, FI, CZ, HU, RO, GR).
// Each pack gets:
//
//   - a nominal-salary case with hand-derived expected values
//     and a tolerance band wide enough to absorb bracket-walk
//     + period-fraction precision drift but tight enough to
//     fail a bracket miscoding or wrong rate;
//
//   - a threshold / cap crossing case where the pack exposes a
//     discontinuity (PL 32% upper bracket, SE statlig threshold,
//     NO trinnskatt T2 boundary, DK topskat, FI 4th bracket,
//     CZ 23% upper, RO personal-deduction floor, GR 44% top,
//     EFKA cap);
//
//   - YTD-aware behaviour for packs whose social-security
//     contributions are gated against an annual ceiling
//     (PL ZUS, SE allmän pensionsavgift, CZ SP, GR EFKA);
//
//   - an empty-input edge case so a zero / negative gross or a
//     zero-day period zeroes the slip rather than throwing.
//
// Tolerances follow the same convention as the Phase-N1 packs:
// hand-derived expected values, ±1–2 currency units on individual
// lines for packs that walk a bracket table. Tighter assertions
// (Equal) are used on flat-rate withholdings.

// ===== Poland =====

// TestPLPackNominalSalary: PLN 8,000 / month, single filer with
// no children, 31-day Jan period.
//
//	periodFraction = 31 / 365.25 ≈ 0.0848734
//	annualGross    = 8000 / periodFraction ≈ PLN 94,258
//	ZUS            = 8000 × 13.71% = 1096.80
//	NFZ base       = 8000 - 1096.80 = 6903.20
//	NFZ            = 6903.20 × 9%   = 621.29
//	PIT base       = 6903.20
//	annualBase     ≈ 6903.20 / 0.0848734 ≈ 81,335 (below 120k bracket)
//	annualPIT      = 81,335 × 12% - 3600 ≈ 6,160
//	periodPIT      ≈ 6,160 × 0.0848734 ≈ 522.81
func TestPLPackNominalSalary(t *testing.T) {
	pack, err := Lookup("PL")
	if err != nil {
		t.Fatalf("lookup PL: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(8000), monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	codes := indexByCode(out)
	if len(codes) != 3 {
		t.Fatalf("expected 3 deductions (ZUS, NFZ, PIT); got %v", codes)
	}
	if zus := codes["PL_ZUS"]; !zus.Equal(dec("1096.80")) {
		t.Errorf("PL_ZUS = %s; want 1096.80 (8000 × 13.71%%)", zus)
	}
	if nfz := codes["PL_NFZ"]; nfz.LessThan(dec("620")) || nfz.GreaterThan(dec("623")) {
		t.Errorf("PL_NFZ = %s; want band 620-623 (≈621.29)", nfz)
	}
	if pit := codes["PL_PIT"]; pit.LessThan(dec("510")) || pit.GreaterThan(dec("535")) {
		t.Errorf("PL_PIT = %s; want band 510-535 (≈522.81)", pit)
	}
}

// TestPLPackUpperBracket: PLN 15,000 / month → ZUS = 2056.50,
// pitBase = 12943.50, annualBase ≈ 152,503 — crosses PLN
// 120,000 cutoff into 32% bracket.
//
//	annualPIT = 120,000 × 12% + (152,503 - 120,000) × 32% - 3,600
//	          = 14,400 + 10,401 - 3,600 = 21,201
//	periodPIT ≈ 21,201 × 0.0848734 ≈ 1,799
//
// Compared to TestPLPackNominalSalary's 522 PLN, this must
// be substantially higher (band 1,700-1,900) to confirm the
// 32% upper bracket fired.
func TestPLPackUpperBracket(t *testing.T) {
	pack, _ := Lookup("PL")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(15000), monthPeriod())
	codes := indexByCode(out)
	if pit := codes["PL_PIT"]; pit.LessThan(dec("1700")) || pit.GreaterThan(dec("1900")) {
		t.Fatalf("PL_PIT upper bracket = %s; want band 1700-1900 (≈1799)", pit)
	}
}

// TestPLPackZUSCapYTD: a high earner with YTD > PLN 260,190
// receives no further pension/disability deductions but
// sickness (2.45%) still emits.
func TestPLPackZUSCapYTD(t *testing.T) {
	pack, _ := Lookup("PL")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
		YTDGross: dec("300000"),
	}, decimal.NewFromInt(20000), monthPeriod())
	codes := indexByCode(out)
	zus := codes["PL_ZUS"]
	// Only sickness 2.45% should emit: 20000 × 0.0245 = 490.00
	if !zus.Equal(dec("490")) {
		t.Errorf("PL_ZUS over cap = %s; want 490.00 (sickness only)", zus)
	}
}

// TestPLPackEmptyGross: zero-gross slip returns no deductions.
func TestPLPackEmptyGross(t *testing.T) {
	pack, _ := Lookup("PL")
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.Zero, monthPeriod())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected no deductions; got %v", out)
	}
}

// ===== Sweden =====

// TestSEPackNominalSalary: SEK 35,000 / month, Stockholm kommun
// (uppercase code STOCKHOLM, 30.38%), 31-day Jan period.
//
//	periodFraction = 0.0848734
//	annualGross    ≈ 412,378
//	taxableAnnual  = 412,378 - 15,400 (grundavdrag) = 396,978 (below brytpunkt 643,100)
//	kommunalskatt  = 396,978 × 30.38% ≈ 120,602
//	periodKommunal ≈ 120,602 × 0.0848734 ≈ 10,236
//	pensionsavgift = 35,000 × 7%       = 2,450
func TestSEPackNominalSalary(t *testing.T) {
	pack, err := Lookup("SE")
	if err != nil {
		t.Fatalf("lookup SE: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Resident: true,
		Canton:   "STOCKHOLM",
	}, decimal.NewFromInt(35000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 2 {
		t.Fatalf("expected 2 deductions (pensionsavgift, kommunalskatt); got %v", codes)
	}
	if pen := codes["SE_PENSION_AVGIFT"]; !pen.Equal(dec("2450")) {
		t.Errorf("SE_PENSION_AVGIFT = %s; want 2450", pen)
	}
	if k := codes["SE_KOMMUNALSKATT"]; k.LessThan(dec("10100")) || k.GreaterThan(dec("10350")) {
		t.Errorf("SE_KOMMUNALSKATT = %s; want band 10100-10350", k)
	}
}

// TestSEPackStatligThreshold: SEK 70,000 / month → annualGross
// ≈ 824,756, taxable ≈ 809,356 — above brytpunkt 643,100. The
// statlig line must emit.
func TestSEPackStatligThreshold(t *testing.T) {
	pack, _ := Lookup("SE")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(70000), monthPeriod())
	codes := indexByCode(out)
	if codes["SE_STATLIG_SKATT"].IsZero() {
		t.Fatalf("SE_STATLIG_SKATT not emitted above brytpunkt: %v", codes)
	}
}

// TestSEPackPensionCapYTD: high-YTD earner exceeds 8.07 × IBB
// = SEK 650,442 cap → no further pensionsavgift.
func TestSEPackPensionCapYTD(t *testing.T) {
	pack, _ := Lookup("SE")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		YTDGross: dec("700000"),
	}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if pen, ok := codes["SE_PENSION_AVGIFT"]; ok && !pen.IsZero() {
		t.Fatalf("SE_PENSION_AVGIFT emitted past cap: %s", pen)
	}
}

// ===== Norway =====

// TestNOPackNominalSalary: NOK 50,000 / month, 31-day Jan period.
//
//	periodFraction = 0.0848734
//	annualGross    ≈ 589,113
//	trygdeavgift   ≈ 589,113 × 7.7% × 0.0848734 ≈ 3,850
//	minstefradrag  = min(46% × 589,113, 104,450) = 104,450
//	alminneligInntekt = 484,663
//	inntektsskatt  ≈ 484,663 × 22% × 0.0848734 ≈ 9,050
//	trinnskatt: 589,113 in T3 (697,150 floor, base 17,151.05)
//	  actually 589,113 between 306,050 (T2 floor) and 697,150 (T3 floor)
//	  so T2: 1507.05 + (589,113 - 306,050) × 4% = 12,829.57
//	periodTrinnskatt ≈ 12,829.57 × 0.0848734 ≈ 1,089
func TestNOPackNominalSalary(t *testing.T) {
	pack, err := Lookup("NO")
	if err != nil {
		t.Fatalf("lookup NO: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Resident: true},
		decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 3 {
		t.Fatalf("expected 3 deductions (trygdeavgift, inntektsskatt, trinnskatt); got %v", codes)
	}
	if tr := codes["NO_TRYGDEAVGIFT"]; tr.LessThan(dec("3800")) || tr.GreaterThan(dec("3900")) {
		t.Errorf("NO_TRYGDEAVGIFT = %s; want band 3800-3900", tr)
	}
	if i := codes["NO_INNTEKTSSKATT"]; i.LessThan(dec("8900")) || i.GreaterThan(dec("9200")) {
		t.Errorf("NO_INNTEKTSSKATT = %s; want band 8900-9200", i)
	}
	if tx := codes["NO_TRINNSKATT"]; tx.LessThan(dec("1050")) || tx.GreaterThan(dec("1150")) {
		t.Errorf("NO_TRINNSKATT = %s; want band 1050-1150", tx)
	}
}

// TestNOPackBelowTrygdeavgiftFloor: a 5,000/mo earner annualises
// to ≈ 58,911, below the 99,650 floor — no trygdeavgift line.
func TestNOPackBelowTrygdeavgiftFloor(t *testing.T) {
	pack, _ := Lookup("NO")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(5000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["NO_TRYGDEAVGIFT"]; ok {
		t.Fatalf("NO_TRYGDEAVGIFT emitted below floor")
	}
}

// ===== Denmark =====

// TestDKPackNominalSalary: DKK 40,000 / month, 31-day Jan period.
//
//	AM-bidrag = 40000 × 8% = 3200.00 (exact)
//	aSkatBase = 36800
//	annualBase ≈ 36800 / 0.0848734 ≈ 433,548
//	taxableAnnual = 433548 - 51600 (personfradrag) = 381,948 (below topskat 588,900)
//	annualAskat = 381948 × (12.01% + 25%) ≈ 381948 × 37.01% ≈ 141,374
//	periodAskat ≈ 141,374 × 0.0848734 ≈ 12,000
func TestDKPackNominalSalary(t *testing.T) {
	pack, err := Lookup("DK")
	if err != nil {
		t.Fatalf("lookup DK: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(40000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 2 {
		t.Fatalf("expected 2 deductions (AM, A-skat); got %v", codes)
	}
	if am := codes["DK_AM_BIDRAG"]; !am.Equal(dec("3200")) {
		t.Errorf("DK_AM_BIDRAG = %s; want 3200", am)
	}
	if a := codes["DK_A_SKAT"]; a.LessThan(dec("11800")) || a.GreaterThan(dec("12200")) {
		t.Errorf("DK_A_SKAT = %s; want band 11800-12200", a)
	}
}

// TestDKPackTopskatThreshold: DKK 80,000 / month → annualPI
// crosses topskat threshold (588,900). A-skat must reflect the
// extra 15% on the slice of PI above the threshold.
//
// Per Personskatteloven § 7 the topskat threshold is measured
// against personlig indkomst (PI = gross − AM-bidrag), NOT
// against PI − personfradrag. With PI = 867,096 the topskat
// slice is (867,096 − 588,900) = 278,196 rather than the
// post-personfradrag slice (815,496 − 588,900 = 226,596) the
// earlier formulation used.
//
//	annualPI       ≈ 867,096
//	taxableAnnual  = 867,096 − 51,600 = 815,496
//	bundskat+kom   = 815,496 × 37.01% ≈ 301,815
//	topskat        = (867,096 − 588,900) × 15% ≈ 41,729
//	annualAskat    ≈ 343,544
//	periodAskat    ≈ 343,544 × 0.0848734 ≈ 29,157
func TestDKPackTopskatThreshold(t *testing.T) {
	pack, _ := Lookup("DK")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(80000), monthPeriod())
	codes := indexByCode(out)
	if a := codes["DK_A_SKAT"]; a.LessThan(dec("28800")) || a.GreaterThan(dec("29500")) {
		t.Errorf("DK_A_SKAT topskat = %s; want band 28800-29500 (≈29157)", a)
	}
}

// TestDKPackTopskatBaseIsPersonalIncome pins the
// Personskatteloven § 7 invariant that the topskat threshold
// is measured against personlig indkomst (PI = gross − AM-bidrag)
// rather than PI − personfradrag. An earner whose PI sits in
// the previously-undertaxed band — above the topskattegrænse
// (DKK 588,900) but with PI − personfradrag below the threshold —
// must now see topskat applied. The earlier formulation tested
// the threshold against (PI − personfradrag) and silently
// undertaxed this 51,600-DKK-wide band.
//
//	gross = 54,000 / month, 31-day period:
//	  AM-bidrag      = 54,000 × 0.08 = 4,320
//	  aSkatBase      = 49,680
//	  periodFraction = 31 / 365.25 ≈ 0.0848734
//	  annualPI       = 49,680 / 0.0848734 ≈ 585,265
//
//	This is just below 588,900 so the old + new agree. Push to
//	gross = 56,000 / mo:
//	  AM-bidrag      = 4,480
//	  aSkatBase      = 51,520
//	  annualPI       ≈ 606,943
//	  Old: taxable = 606,943 − 51,600 = 555,343 < 588,900 → NO topskat
//	  New: annualPI = 606,943 > 588,900 → topskat = (606,943 − 588,900) × 15%
//	                                             ≈ 2,706
//	  Period topskat ≈ 2,706 × 0.0848734 ≈ 230
//
// Comparing the corrected A-skat to a hypothetical no-topskat
// computation at the same gross would give a difference of ~230
// DKK/month — the band we want to pin. Concretely:
//
//	taxableAnnual  = 555,343
//	bundskat+kom   = 555,343 × 37.01% ≈ 205,532
//	annualAskat    = 205,532 + 2,706 = 208,238
//	periodAskat    ≈ 208,238 × 0.0848734 ≈ 17,673
//
// Without the fix this would have been ≈ 17,443 (no topskat
// component). The band [17,500, 17,850] catches the corrected
// value and would fail if the pack regressed to the buggy
// post-personfradrag threshold test.
func TestDKPackTopskatBaseIsPersonalIncome(t *testing.T) {
	pack, _ := Lookup("DK")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(56000), monthPeriod())
	codes := indexByCode(out)
	if a := codes["DK_A_SKAT"]; a.LessThan(dec("17500")) || a.GreaterThan(dec("17850")) {
		t.Errorf("DK_A_SKAT mid-band = %s; want band 17500-17850 (≈17673 with topskat on PI > 588,900)", a)
	}
}

// ===== Finland =====

// TestFIPackNominalSalary: EUR 3,000 / month, 31-day Jan period,
// age 30 (default TyEL 7.15%), Helsinki kunta (5.36%).
//
//	annualGross    ≈ 35,344
//	TyEL           = 3000 × 7.15% = 214.50
//	SAVA above floor: 3000 × 1.57% = 47.10
//	state tax: bracket walk on 35,344
//	  base = bracket 3 floor 31,500 → 4636.68 + (35344-31500) × 30.25% = 5799.30
//	periodStateTax ≈ 5799.30 × 0.0848734 ≈ 492.20
//	kunnallisvero  ≈ 35,344 × 5.36% × 0.0848734 ≈ 160.78
func TestFIPackNominalSalary(t *testing.T) {
	pack, err := Lookup("FI")
	if err != nil {
		t.Fatalf("lookup FI: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		Age:    30,
		Canton: "HELSINKI",
	}, decimal.NewFromInt(3000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 4 {
		t.Fatalf("expected 4 deductions (TyEL, SAVA, state, kunta); got %v", codes)
	}
	if tyel := codes["FI_TYEL"]; !tyel.Equal(dec("214.50")) {
		t.Errorf("FI_TYEL = %s; want 214.50 (3000 × 7.15%%)", tyel)
	}
	if sava := codes["FI_SAVA"]; !sava.Equal(dec("47.10")) {
		t.Errorf("FI_SAVA = %s; want 47.10", sava)
	}
	if st := codes["FI_VALTION_VERO"]; st.LessThan(dec("485")) || st.GreaterThan(dec("500")) {
		t.Errorf("FI_VALTION_VERO = %s; want band 485-500", st)
	}
	if k := codes["FI_KUNNALLISVERO"]; k.LessThan(dec("155")) || k.GreaterThan(dec("170")) {
		t.Errorf("FI_KUNNALLISVERO = %s; want band 155-170", k)
	}
}

// TestFIPackMidlifeTyEL: age 55 → 8.65% TyEL rate.
func TestFIPackMidlifeTyEL(t *testing.T) {
	pack, _ := Lookup("FI")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Age: 55}, decimal.NewFromInt(3000), monthPeriod())
	codes := indexByCode(out)
	// 3000 × 8.65% = 259.50
	if tyel := codes["FI_TYEL"]; !tyel.Equal(dec("259.50")) {
		t.Errorf("FI_TYEL (midlife) = %s; want 259.50", tyel)
	}
}

// TestFIPackBelowSAVAFloor: a small slip whose annualised gross
// is under EUR 16,862 has no SAVA line.
func TestFIPackBelowSAVAFloor(t *testing.T) {
	pack, _ := Lookup("FI")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Age: 30}, decimal.NewFromInt(1000), monthPeriod())
	codes := indexByCode(out)
	if _, ok := codes["FI_SAVA"]; ok {
		t.Fatalf("FI_SAVA emitted below EUR 16,862 / yr floor")
	}
}

// ===== Czech Republic =====

// TestCZPackNominalSalary: CZK 50,000 / month, 31-day Jan period.
//
//	SP        = 50000 × 6.5%  = 3250.00
//	ZP        = 50000 × 4.5%  = 2250.00
//	annualGross ≈ 589,113 (below 1,582,812 cutoff)
//	annualPIT = 589,113 × 15% - 30,840 = 57,527
//	periodPIT ≈ 57,527 × 0.0848734 ≈ 4,883
func TestCZPackNominalSalary(t *testing.T) {
	pack, err := Lookup("CZ")
	if err != nil {
		t.Fatalf("lookup CZ: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(50000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 3 {
		t.Fatalf("expected 3 deductions (SP, ZP, PIT); got %v", codes)
	}
	if sp := codes["CZ_SP"]; !sp.Equal(dec("3250")) {
		t.Errorf("CZ_SP = %s; want 3250", sp)
	}
	if zp := codes["CZ_ZP"]; !zp.Equal(dec("2250")) {
		t.Errorf("CZ_ZP = %s; want 2250", zp)
	}
	if pit := codes["CZ_PIT"]; pit.LessThan(dec("4800")) || pit.GreaterThan(dec("4960")) {
		t.Errorf("CZ_PIT = %s; want band 4800-4960 (≈4883)", pit)
	}
}

// TestCZPackUpperBracket: CZK 200,000 / month → annualGross
// ≈ 2,356,453 → crosses CZK 1,582,812 cutoff. PIT must reflect
// the 23% upper bracket.
func TestCZPackUpperBracket(t *testing.T) {
	pack, _ := Lookup("CZ")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(200000), monthPeriod())
	codes := indexByCode(out)
	// annualPIT ≈ 1,582,812 × 15% + (2,356,453 - 1,582,812) × 23% - 30,840
	//           = 237,422 + 177,937 - 30,840 = 384,519
	// periodPIT ≈ 32,633 — band [32,000, 33,500]
	if pit := codes["CZ_PIT"]; pit.LessThan(dec("32000")) || pit.GreaterThan(dec("33500")) {
		t.Errorf("CZ_PIT upper bracket = %s; want band 32000-33500", pit)
	}
}

// ===== Hungary =====

// TestHUPackNominalSalary: HUF 500,000 / month.
//
//	TB   = 500000 × 18.5% = 92,500.00
//	SZJA = 500000 × 15%   = 75,000.00
func TestHUPackNominalSalary(t *testing.T) {
	pack, err := Lookup("HU")
	if err != nil {
		t.Fatalf("lookup HU: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(500000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 2 {
		t.Fatalf("expected 2 deductions (TB, SZJA); got %v", codes)
	}
	if tb := codes["HU_TB"]; !tb.Equal(dec("92500")) {
		t.Errorf("HU_TB = %s; want 92500 (500000 × 18.5%%)", tb)
	}
	if sz := codes["HU_SZJA"]; !sz.Equal(dec("75000")) {
		t.Errorf("HU_SZJA = %s; want 75000 (500000 × 15%%)", sz)
	}
}

// ===== Romania =====

// TestROPackNominalSalary: RON 6,000 / month, 31-day Jan period.
//
//	periodFraction = 31 / 365.25 ≈ 0.084873
//	CAS    = 6000 × 25%  = 1500.00
//	CASS   = 6000 × 10%  = 600.00
//	annualGross   = 6000 / 0.084873  ≈ 70,684.62
//	annualCAS     = 1500 / 0.084873  ≈ 17,671.16
//	annualCASS    =  600 / 0.084873  ≈  7,068.46
//	annualTaxBase = 70,684.62 - 17,671.16 - 7,068.46 - 7,200
//	              ≈ 38,744.99
//	annualImpozit = 38,744.99 × 10%  ≈  3,874.50
//	periodImpozit = 3,874.50 × 0.084873 ≈ 328.89
//
// Algebraic short-form (since CAS / CASS scale linearly with
// period gross): periodImpozit = (gross - cas - cass) × rate -
// roAnnualDeducerePersonala × periodFraction × rate.
func TestROPackNominalSalary(t *testing.T) {
	pack, err := Lookup("RO")
	if err != nil {
		t.Fatalf("lookup RO: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(6000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 3 {
		t.Fatalf("expected 3 deductions (CAS, CASS, Impozit); got %v", codes)
	}
	if cas := codes["RO_CAS"]; !cas.Equal(dec("1500")) {
		t.Errorf("RO_CAS = %s; want 1500", cas)
	}
	if cass := codes["RO_CASS"]; !cass.Equal(dec("600")) {
		t.Errorf("RO_CASS = %s; want 600", cass)
	}
	if imp := codes["RO_IMPOZIT"]; imp.LessThan(dec("325")) || imp.GreaterThan(dec("332")) {
		t.Errorf("RO_IMPOZIT = %s; want band 325-332 (≈328.89)", imp)
	}
}

// TestROPackBiWeeklyDeductionScales pins the period-fraction fix
// for the monthly RON 600 personal deduction. A 14-day slip at the
// same daily rate as the nominal monthly case (RON 6000/mo) must
// only get the prorated portion of the deduction (~276 RON) rather
// than the full monthly value, so the resulting impozit reflects
// the true 14-day base.
//
//	periodFraction = 14 / 365.25 ≈ 0.038330
//	gross_14d      = 6000 × 14/31 ≈ 2709.68
//	CAS / CASS      scale linearly (677.42 / 270.97)
//	annualBase     ≈ (2709.68 - 677.42 - 270.97) / 0.038330 - 7200
//	               ≈ 38,754 (matches monthly annualisation, within
//	                 RON 10 of TestROPackNominalSalary's 38,744 by
//	                 design — proves the deduction is annualised
//	                 once, not re-applied per period)
//	periodImpozit  ≈ 148.50
//
// Pre-fix the same input produced periodImpozit ≈ 116.13 because
// the full RON 600 deduction was subtracted from the half-month
// 2709.68 base; this test will fail at that value.
func TestROPackBiWeeklyDeductionScales(t *testing.T) {
	pack, _ := Lookup("RO")
	biWeekly := PayPeriod{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC),
	}
	// gross at the same daily rate as the monthly test: 6000 × 14/31
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, dec("2709.68"), biWeekly)
	codes := indexByCode(out)
	if imp := codes["RO_IMPOZIT"]; imp.LessThan(dec("145")) || imp.GreaterThan(dec("152")) {
		t.Errorf("RO_IMPOZIT (bi-weekly) = %s; want band 145-152 (≈148.50)", imp)
	}
}

// TestROPackBelowPersonalDeductionFloor: a low-wage slip whose
// annualised gross net of CAS / CASS sits below the annual personal
// deduction (RON 7,200) → annual tax base clips to zero → no
// income tax.
//
//	gross=900: CAS=225, CASS=90.
//	annualBase ≈ (900 - 225 - 90) / 0.084873 - 7200
//	           ≈ 6,892.55 - 7,200 = -307.45 → clipped to 0.
func TestROPackBelowPersonalDeductionFloor(t *testing.T) {
	pack, _ := Lookup("RO")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(900), monthPeriod())
	codes := indexByCode(out)
	if imp, ok := codes["RO_IMPOZIT"]; ok && !imp.IsZero() {
		t.Fatalf("RO_IMPOZIT emitted below floor: %s", imp)
	}
}

// ===== Greece =====

// TestGRPackNominalSalary: EUR 2,000 / month, 31-day Jan period.
//
//	annualGross ≈ 23,562.96
//	EFKA        = 2000 × 13.87% = 277.40 (under cap)
//	taxableAnnual = annualGross - annualEFKA ≈ 23,562.96 - 3268.30 ≈ 20,295
//	bracket walk (3rd: 20,000-30,000, base 3100, rate 28%):
//	  annualPIT = 3100 + (20,295 - 20,000) × 28% = 3,182.50
//	allowance phase-out: taxable > 12,000
//	  reduction = (20,295 - 12,000) / 1000 × 20 = 165.90
//	  effective allowance = 777 - 165.90 = 611.10
//	annualPIT - allowance ≈ 2,571.40
//	periodPIT ≈ 2,571.40 × 0.0848734 ≈ 218.23
func TestGRPackNominalSalary(t *testing.T) {
	pack, err := Lookup("GR")
	if err != nil {
		t.Fatalf("lookup GR: %v", err)
	}
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(2000), monthPeriod())
	codes := indexByCode(out)
	if len(codes) != 2 {
		t.Fatalf("expected 2 deductions (EFKA, PIT); got %v", codes)
	}
	if efka := codes["GR_EFKA"]; !efka.Equal(dec("277.40")) {
		t.Errorf("GR_EFKA = %s; want 277.40 (2000 × 13.87%%)", efka)
	}
	if pit := codes["GR_PIT"]; pit.LessThan(dec("210")) || pit.GreaterThan(dec("230")) {
		t.Errorf("GR_PIT = %s; want band 210-230 (≈218)", pit)
	}
}

// TestGRPackEFKACapYTD: high-YTD earner above EUR 86,946.80 cap
// receives no further EFKA.
func TestGRPackEFKACapYTD(t *testing.T) {
	pack, _ := Lookup("GR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		YTDGross: dec("90000"),
	}, decimal.NewFromInt(8000), monthPeriod())
	codes := indexByCode(out)
	if efka, ok := codes["GR_EFKA"]; ok && !efka.IsZero() {
		t.Fatalf("GR_EFKA emitted past cap: %s", efka)
	}
}

// TestGRPackTopBracket: EUR 5,000 / month → annualGross ≈
// 58,907 → into 44% top bracket. PIT should be substantially
// higher than nominal case.
func TestGRPackTopBracket(t *testing.T) {
	pack, _ := Lookup("GR")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(5000), monthPeriod())
	codes := indexByCode(out)
	if pit := codes["GR_PIT"]; pit.LessThan(dec("700")) {
		t.Fatalf("GR_PIT top bracket = %s; want > 700", pit)
	}
}

// ===== Cross-pack: zero-period & negative-gross edge cases =====

// TestExtendedPacksZeroPeriodNoDeductions: zero-day period must
// return no deductions across all 9 packs.
func TestExtendedPacksZeroPeriodNoDeductions(t *testing.T) {
	for _, cc := range []string{"PL", "SE", "NO", "DK", "FI", "CZ", "HU", "RO", "GR"} {
		t.Run(cc, func(t *testing.T) {
			pack, err := Lookup(cc)
			if err != nil {
				t.Fatalf("lookup %s: %v", cc, err)
			}
			out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(5000), zeroDayPeriod())
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			if len(out) != 0 {
				t.Fatalf("%s: expected no deductions for zero-day period; got %v", cc, out)
			}
		})
	}
}

// TestExtendedPacksNegativeGrossNoDeductions: negative gross must
// return no deductions across all 9 packs.
func TestExtendedPacksNegativeGrossNoDeductions(t *testing.T) {
	for _, cc := range []string{"PL", "SE", "NO", "DK", "FI", "CZ", "HU", "RO", "GR"} {
		t.Run(cc, func(t *testing.T) {
			pack, _ := Lookup(cc)
			out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{}, decimal.NewFromInt(-100), monthPeriod())
			if len(out) != 0 {
				t.Fatalf("%s: negative gross produced deductions: %v", cc, out)
			}
		})
	}
}

// europeBracketRow is a typed projection of any Phase-N2 bracket
// struct used by TestEuropeExtendedBracketTablesAreContiguous.
// The per-pack types (noBracket / fiBracket / grBracket) stay
// distinct so a future schedule change to one pack cannot
// cross-leak into another — but every walk function relies on
// the same shape (Floor / Top / Base / Rate) so we project the
// per-pack rows through this view and check the shared
// invariants in one place. Mirrors the bracketRow / checkRows
// pattern from apac_packs_test.go and americas_packs_test.go.
type europeBracketRow struct {
	floor decimal.Decimal
	top   decimal.Decimal
	base  decimal.Decimal
	rate  decimal.Decimal
}

// TestEuropeExtendedBracketTablesAreContiguous pins the two
// invariants every Phase-N2 walk function relies on:
//
//  1. Top-contiguity — adjacent rows satisfy
//     `Top[i] == Floor[i+1]`, and the last row is open-ended
//     (`Top == 0`).
//
//  2. Base-consistency — adjacent rows satisfy
//     `Base[i+1] == Base[i] + (Floor[i+1] - Floor[i]) * Rate[i]`.
//     This is the real correctness invariant: the walk resolves
//     annual tax as `Base + (income - Floor) * Rate` for the
//     matched bracket, so a wrong Base produces a wrong tax at
//     every income in that bracket. A bracket-row Base
//     transcription error in NO trinnskatt (the original bug
//     this test was added in response to) would have failed
//     this test at construction time.
func TestEuropeExtendedBracketTablesAreContiguous(t *testing.T) {
	checkRows := func(t *testing.T, label string, rows []europeBracketRow) {
		t.Helper()
		for i := 0; i < len(rows)-1; i++ {
			cur, next := rows[i], rows[i+1]
			if !cur.top.Equal(next.floor) {
				t.Fatalf("%s brackets[%d].Top (%s) != brackets[%d].Floor (%s)",
					label, i, cur.top, i+1, next.floor)
			}
			want := cur.base.Add(next.floor.Sub(cur.floor).Mul(cur.rate))
			if !next.base.Equal(want) {
				t.Fatalf("%s brackets[%d].Base (%s) != Base[%d] + (Floor[%d]-Floor[%d])*Rate[%d] (= %s)",
					label, i+1, next.base, i, i+1, i, i, want)
			}
		}
		last := rows[len(rows)-1]
		if !last.top.IsZero() {
			t.Fatalf("%s last bracket Top should be 0 (open-ended), got %s", label, last.top)
		}
	}

	noRows := make([]europeBracketRow, len(noTrinnskattBrackets))
	for i, b := range noTrinnskattBrackets {
		noRows[i] = europeBracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	fiRows := make([]europeBracketRow, len(fiStateTaxBrackets))
	for i, b := range fiStateTaxBrackets {
		fiRows[i] = europeBracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}
	grRows := make([]europeBracketRow, len(grPITBrackets))
	for i, b := range grPITBrackets {
		grRows[i] = europeBracketRow{floor: b.Floor, top: b.Top, base: b.Base, rate: b.Rate}
	}

	t.Run("NO trinnskatt", func(t *testing.T) { checkRows(t, "NO trinnskatt", noRows) })
	t.Run("FI valtion tulovero", func(t *testing.T) { checkRows(t, "FI valtion tulovero", fiRows) })
	t.Run("GR PIT", func(t *testing.T) { checkRows(t, "GR PIT", grRows) })
}
