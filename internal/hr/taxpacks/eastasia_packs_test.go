package taxpacks

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// eastasia_packs_test.go covers the China (CN) tax pack. China uses
// the 累计预扣预缴 (cumulative withholding) method, so unlike the
// annualise-one-period packs the assertions below pin the cumulative
// behaviour explicitly: a first-month slip (no YTD), a city housing-
// fund override (Canton-driven), a later-month slip whose cumulative
// base has already crossed into the second IIT band (YTD-aware), and
// the zero / zero-day edge cases. Hand-derivations are documented in
// each test body so an annual rate refresh can be re-verified by
// walking the same math.

// cnMonthEnding returns a one-calendar-month pay period whose End
// falls in the given month of 2026, so the CN pack's cumulative
// month index (derived from the period end month) is `month`.
func cnMonthEnding(month time.Month) PayPeriod {
	return PayPeriod{
		Start: time.Date(2026, month, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, month, 28, 0, 0, 0, 0, time.UTC),
	}
}

// TestCNPackFirstMonthNominal: ¥30,000 gross, January (month 1), no
// YTD, default city (housing fund 12%).
//
//	SI (employee)   = 30,000 × (8% + 2% + 0.5%) + ¥3 = 3,150 + 3 = 3,153.00
//	Housing fund    = 30,000 × 12%                    = 3,600.00
//	IIT (cumulative, month 1):
//	  deductibleRate = 10.5% + 12% = 22.5%
//	  cum taxable    = 30,000 − (5,000 × 1) − (30,000 × 22.5% + 3 × 1)
//	                 = 30,000 − 5,000 − 6,753 = 18,247
//	  band 1 (≤36,000 @ 3%): 18,247 × 0.03 = 547.41
//	  prior cumulative tax (YTD 0, months 0) = 0
//	  period IIT = 547.41
func TestCNPackFirstMonthNominal(t *testing.T) {
	pack, err := Lookup("CN")
	if err != nil {
		t.Fatalf("lookup CN: %v", err)
	}
	out, err := pack.ComputeWithholding(context.Background(), EmployeeInfo{},
		decimal.NewFromInt(30000), cnMonthEnding(time.January))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if si := findDeduction(out, "CN_SOCIAL_INSURANCE").Amount; !si.Equal(dec("3153")) {
		t.Fatalf("CN_SOCIAL_INSURANCE: got %s, want 3153", si.String())
	}
	if hf := findDeduction(out, "CN_HOUSING_FUND").Amount; !hf.Equal(dec("3600")) {
		t.Fatalf("CN_HOUSING_FUND: got %s, want 3600", hf.String())
	}
	if iit := findDeduction(out, "CN_IIT").Amount; !iit.Equal(dec("547.41")) {
		t.Fatalf("CN_IIT: got %s, want 547.41", iit.String())
	}
}

// TestCNPackCityHousingFundOverride: same ¥30,000 January slip but
// Canton = Shanghai, whose housing-fund rate is 7% (vs the 12%
// default). The lower fund both shrinks the CN_HOUSING_FUND line and
// — because the fund is pre-tax deductible — raises the IIT base.
//
//	Housing fund   = 30,000 × 7% = 2,100.00
//	deductibleRate = 10.5% + 7% = 17.5%
//	cum taxable    = 30,000 − 5,000 − (30,000 × 17.5% + 3) = 30,000 − 5,000 − 5,253 = 19,747
//	IIT            = 19,747 × 0.03 = 592.41
func TestCNPackCityHousingFundOverride(t *testing.T) {
	pack, _ := Lookup("CN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{Canton: "SHANGHAI"},
		decimal.NewFromInt(30000), cnMonthEnding(time.January))
	if hf := findDeduction(out, "CN_HOUSING_FUND").Amount; !hf.Equal(dec("2100")) {
		t.Fatalf("CN_HOUSING_FUND (Shanghai): got %s, want 2100", hf.String())
	}
	if iit := findDeduction(out, "CN_IIT").Amount; !iit.Equal(dec("592.41")) {
		t.Fatalf("CN_IIT (Shanghai): got %s, want 592.41", iit.String())
	}
	// Social insurance is unaffected by the city housing-fund rate.
	if si := findDeduction(out, "CN_SOCIAL_INSURANCE").Amount; !si.Equal(dec("3153")) {
		t.Fatalf("CN_SOCIAL_INSURANCE (Shanghai): got %s, want 3153", si.String())
	}
}

// TestCNPackCumulativeBandCrossing: ¥50,000 gross in April (month 4)
// with ¥150,000 YTD (three prior ¥50,000 months), default city.
// Exercises the YTD-aware cumulative walk where the running taxable
// base sits in the 10% band.
//
//	deductibleRate = 22.5%
//	cum taxable (month 4) = 200,000 − (5,000 × 4) − (200,000 × 22.5% + 3 × 4)
//	                      = 200,000 − 20,000 − 45,012 = 134,988
//	  band 2 (36,000–144,000 @ 10%, QD 2,520): 134,988 × 0.10 − 2,520 = 10,978.80
//	cum taxable (month 3) = 150,000 − (5,000 × 3) − (150,000 × 22.5% + 3 × 3)
//	                      = 150,000 − 15,000 − 33,759 = 101,241
//	  band 2: 101,241 × 0.10 − 2,520 = 7,604.10
//	period IIT = 10,978.80 − 7,604.10 = 3,374.70
//	SI         = 50,000 × 10.5% + 3 = 5,253.00
//	Housing    = 50,000 × 12%       = 6,000.00
func TestCNPackCumulativeBandCrossing(t *testing.T) {
	pack, _ := Lookup("CN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{
		YTDGross: decimal.NewFromInt(150000),
	}, decimal.NewFromInt(50000), cnMonthEnding(time.April))
	if iit := findDeduction(out, "CN_IIT").Amount; !iit.Equal(dec("3374.70")) {
		t.Fatalf("CN_IIT (cumulative month 4): got %s, want 3374.70", iit.String())
	}
	if si := findDeduction(out, "CN_SOCIAL_INSURANCE").Amount; !si.Equal(dec("5253")) {
		t.Fatalf("CN_SOCIAL_INSURANCE: got %s, want 5253", si.String())
	}
	if hf := findDeduction(out, "CN_HOUSING_FUND").Amount; !hf.Equal(dec("6000")) {
		t.Fatalf("CN_HOUSING_FUND: got %s, want 6000", hf.String())
	}
}

// TestCNPackMidYearStarterMonthCount pins the cumulative month index
// to EmployeeInfo.MonthsEmployedYTD rather than the calendar month.
// An employee hired in April whose first ¥30,000 slip falls in April
// (calendar month 4) has only received one month of income, so the
// cumulative standard deduction must be a single ¥5,000 — yielding
// exactly the same IIT as any other first-paid-month slip (547.41,
// per TestCNPackFirstMonthNominal). Without the field the pack would
// fall back to calendar month 4, credit four months of standard
// deduction, and under-withhold (97.14). The fallback is asserted too
// so pre-existing KRecords (MonthsEmployedYTD == 0) keep their prior
// calendar-month behaviour.
//
//	MonthsEmployedYTD = 1 (April starter, first paid month):
//	  cum taxable = 30,000 − (5,000 × 1) − (30,000 × 22.5% + 3 × 1)
//	              = 30,000 − 5,000 − 6,753 = 18,247
//	  IIT         = 18,247 × 0.03 = 547.41
//
//	MonthsEmployedYTD = 0 (unset → fallback to calendar month 4):
//	  cum taxable = 30,000 − (5,000 × 4) − (30,000 × 22.5% + 3 × 4)
//	              = 30,000 − 20,000 − 6,762 = 3,238
//	  IIT         = 3,238 × 0.03 = 97.14
func TestCNPackMidYearStarterMonthCount(t *testing.T) {
	pack, _ := Lookup("CN")

	// MonthsEmployedYTD pins the first paid month regardless of the
	// calendar month the slip falls in.
	corrected, _ := pack.ComputeWithholding(context.Background(),
		EmployeeInfo{MonthsEmployedYTD: 1},
		decimal.NewFromInt(30000), cnMonthEnding(time.April))
	if iit := findDeduction(corrected, "CN_IIT").Amount; !iit.Equal(dec("547.41")) {
		t.Fatalf("CN_IIT (April starter, MonthsEmployedYTD=1): got %s, want 547.41", iit.String())
	}

	// Unset (pre-existing KRecord) falls back to the calendar month.
	fallback, _ := pack.ComputeWithholding(context.Background(),
		EmployeeInfo{},
		decimal.NewFromInt(30000), cnMonthEnding(time.April))
	if iit := findDeduction(fallback, "CN_IIT").Amount; !iit.Equal(dec("97.14")) {
		t.Fatalf("CN_IIT (April, calendar-month fallback): got %s, want 97.14", iit.String())
	}

	// A tax year has at most 12 months: an out-of-range
	// MonthsEmployedYTD (e.g. a data-entry error) is clamped to 12 so
	// it cannot over-credit the standard deduction and under-withhold.
	// Use a high cumulative base so the IIT is solidly positive and
	// the 13-vs-12 comparison is meaningful (not both clamped to zero).
	twelve, _ := pack.ComputeWithholding(context.Background(),
		EmployeeInfo{MonthsEmployedYTD: 12, YTDGross: decimal.NewFromInt(550000)},
		decimal.NewFromInt(50000), cnMonthEnding(time.December))
	thirteen, _ := pack.ComputeWithholding(context.Background(),
		EmployeeInfo{MonthsEmployedYTD: 13, YTDGross: decimal.NewFromInt(550000)},
		decimal.NewFromInt(50000), cnMonthEnding(time.December))
	iit12 := findDeduction(twelve, "CN_IIT").Amount
	iit13 := findDeduction(thirteen, "CN_IIT").Amount
	if !iit12.IsPositive() {
		t.Fatalf("CN_IIT (12 months) should be positive for the clamp comparison, got %s", iit12.String())
	}
	if !iit13.Equal(iit12) {
		t.Fatalf("CN_IIT should clamp MonthsEmployedYTD>12 to 12: got %s for 13, want %s (==12)", iit13.String(), iit12.String())
	}
}

// TestCNPackBelowThresholdNoIIT: a low ¥5,500 January slip. After the
// ¥5,000 standard deduction and the employee statutory contributions
// the cumulative taxable base is negative, so no CN_IIT line is
// emitted; the SI and housing-fund lines remain positive.
func TestCNPackBelowThresholdNoIIT(t *testing.T) {
	pack, _ := Lookup("CN")
	out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{},
		decimal.NewFromInt(5500), cnMonthEnding(time.January))
	if iit := findDeduction(out, "CN_IIT").Amount; iit.IsPositive() {
		t.Fatalf("CN_IIT should be zero below the standard deduction, got %s", iit.String())
	}
	if si := findDeduction(out, "CN_SOCIAL_INSURANCE").Amount; !si.IsPositive() {
		t.Fatalf("CN_SOCIAL_INSURANCE should still be positive, got %s", si.String())
	}
	if hf := findDeduction(out, "CN_HOUSING_FUND").Amount; !hf.IsPositive() {
		t.Fatalf("CN_HOUSING_FUND should still be positive, got %s", hf.String())
	}
}

// TestCNPackEmptyInputs: zero gross and a zero-day period must each
// short-circuit and emit no deductions.
func TestCNPackEmptyInputs(t *testing.T) {
	pack, _ := Lookup("CN")
	if out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{},
		decimal.Zero, cnMonthEnding(time.January)); len(out) != 0 {
		t.Fatalf("zero gross should emit no deductions, got %+v", out)
	}
	if out, _ := pack.ComputeWithholding(context.Background(), EmployeeInfo{},
		decimal.NewFromInt(30000), zeroDayPeriod()); len(out) != 0 {
		t.Fatalf("zero-day period should emit no deductions, got %+v", out)
	}
}

// TestCNIITBracketTableIsContiguous guards the cumulative rate table
// against transcription errors on the annual refresh. For the quick-
// deduction form, contiguity means the tax computed at each band's
// top is identical under that band and the next band:
//
//	Top[i] × Rate[i] − QD[i] == Top[i] × Rate[i+1] − QD[i+1]
//
// plus the bands must be floor/top-contiguous and the last band
// open-ended (Top == 0).
func TestCNIITBracketTableIsContiguous(t *testing.T) {
	for i := 0; i < len(cnIITBrackets)-1; i++ {
		cur, next := cnIITBrackets[i], cnIITBrackets[i+1]
		if !cur.Top.Equal(next.Floor) {
			t.Fatalf("band[%d].Top (%s) != band[%d].Floor (%s)",
				i, cur.Top, i+1, next.Floor)
		}
		under := cur.Top.Mul(cur.Rate).Sub(cur.QuickDeduction)
		over := cur.Top.Mul(next.Rate).Sub(next.QuickDeduction)
		if !under.Equal(over) {
			t.Fatalf("band boundary at %s discontinuous: under=%s over=%s",
				cur.Top, under, over)
		}
	}
	if last := cnIITBrackets[len(cnIITBrackets)-1]; !last.Top.IsZero() {
		t.Fatalf("last band Top should be 0 (open-ended), got %s", last.Top)
	}
}
