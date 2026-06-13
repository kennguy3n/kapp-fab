package hr

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr/taxpacks"
)

func boolPtr(b bool) *bool { return &b }

func march2026() taxpacks.PayPeriod {
	return taxpacks.PayPeriod{
		Start: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
	}
}

// findLine returns the first line with the given code, or fails.
func findLine(t *testing.T, lines []PayslipLine, code string) PayslipLine {
	t.Helper()
	for _, l := range lines {
		if l.Code == code {
			return l
		}
	}
	t.Fatalf("line %q not found", code)
	return PayslipLine{}
}

// TestBuildPipeline_FullPeriodNoInputs is the baseline: a single base
// earning, no proration, no inputs. Gross == taxable == base.
func TestBuildPipeline_FullPeriodNoInputs(t *testing.T) {
	sv := structureData{BaseSalary: decimal.NewFromInt(3100)}
	res := buildPipeline(sv, nil, time.Time{}, march2026())

	if !res.Gross.Equal(decimal.NewFromInt(3100)) {
		t.Fatalf("gross: got %s want 3100", res.Gross)
	}
	if !res.TaxableGross.Equal(decimal.NewFromInt(3100)) {
		t.Fatalf("taxable: got %s want 3100", res.TaxableGross)
	}
	if !res.ProrationFactor.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("factor: got %s want 1", res.ProrationFactor)
	}
	base := findLine(t, res.Earnings, "BASE")
	if base.Kind != "" { // Kind is assigned at persist time, not in pipeline
		t.Fatalf("pipeline should not set Kind: got %q", base.Kind)
	}
}

// TestBuildPipeline_ProratesMidPeriodJoiner scales recurring earnings by
// employedDays/totalDays for a join date inside the period.
func TestBuildPipeline_ProratesMidPeriodJoiner(t *testing.T) {
	sv := structureData{BaseSalary: decimal.NewFromInt(3100)}
	join := time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)
	res := buildPipeline(sv, nil, join, march2026())

	// employedDays = (31-17)+1 = 15, totalDays = 31.
	want := decimal.NewFromInt(3100).Mul(decimal.NewFromInt(15)).
		Div(decimal.NewFromInt(31)).Round(2)
	if !res.Gross.Equal(want) {
		t.Fatalf("prorated gross: got %s want %s", res.Gross, want)
	}
	base := findLine(t, res.Earnings, "BASE")
	if base.Base == nil || !base.Base.Equal(decimal.NewFromInt(3100)) {
		t.Fatalf("prorated BASE should record full amount as Base, got %v", base.Base)
	}
	if base.Rate == nil {
		t.Fatalf("prorated BASE should record the proration factor as Rate")
	}
}

// TestBuildPipeline_LOPReducesGross docks an unpaid-leave day from gross.
func TestBuildPipeline_LOPReducesGross(t *testing.T) {
	sv := structureData{BaseSalary: decimal.NewFromInt(3100)}
	inputs := []PayInput{{Type: PayInputLOPDays, Qty: decimal.NewFromInt(1)}}
	res := buildPipeline(sv, inputs, time.Time{}, march2026())

	// perDay = 3100/31 = 100; one LOP day → gross = 3000.
	if !res.Gross.Equal(decimal.NewFromInt(3000)) {
		t.Fatalf("gross after LOP: got %s want 3000", res.Gross)
	}
	lop := findLine(t, res.Earnings, "LOP")
	if !lop.Amount.Equal(decimal.NewFromInt(100).Neg()) {
		t.Fatalf("LOP amount: got %s want -100", lop.Amount)
	}
	if !res.TaxableGross.Equal(decimal.NewFromInt(3000)) {
		t.Fatalf("taxable after LOP: got %s want 3000", res.TaxableGross)
	}
}

// TestBuildPipeline_BonusIsAdditiveEarning adds a taxable bonus to gross.
func TestBuildPipeline_BonusIsAdditiveEarning(t *testing.T) {
	sv := structureData{BaseSalary: decimal.NewFromInt(3100)}
	inputs := []PayInput{{Type: PayInputBonus, Amount: decimal.NewFromInt(500), Taxable: true}}
	res := buildPipeline(sv, inputs, time.Time{}, march2026())

	if !res.Gross.Equal(decimal.NewFromInt(3600)) {
		t.Fatalf("gross with bonus: got %s want 3600", res.Gross)
	}
	if !res.TaxableGross.Equal(decimal.NewFromInt(3600)) {
		t.Fatalf("taxable with bonus: got %s want 3600", res.TaxableGross)
	}
	b := findLine(t, res.Earnings, "BONUS")
	if !b.Amount.Equal(decimal.NewFromInt(500)) {
		t.Fatalf("bonus amount: got %s want 500", b.Amount)
	}
}

// TestBuildPipeline_NonTaxableInputDoesNotRaiseTaxable keeps a
// non-taxable reimbursement out of the taxable base while still adding to
// gross.
func TestBuildPipeline_NonTaxableInputDoesNotRaiseTaxable(t *testing.T) {
	sv := structureData{BaseSalary: decimal.NewFromInt(3100)}
	inputs := []PayInput{{Type: PayInputReimbursement, Amount: decimal.NewFromInt(200), Taxable: false}}
	res := buildPipeline(sv, inputs, time.Time{}, march2026())

	if !res.Gross.Equal(decimal.NewFromInt(3300)) {
		t.Fatalf("gross with reimbursement: got %s want 3300", res.Gross)
	}
	if !res.TaxableGross.Equal(decimal.NewFromInt(3100)) {
		t.Fatalf("taxable should exclude non-taxable reimbursement: got %s want 3100", res.TaxableGross)
	}
}

// TestBuildPipeline_PreTaxDeductionReducesTaxable is the D2 invariant:
// a salary-sacrifice (pre-tax) deduction lowers the taxable base BEFORE
// withholding, while a post-tax deduction does not.
func TestBuildPipeline_PreTaxDeductionReducesTaxable(t *testing.T) {
	sv := structureData{
		BaseSalary: decimal.NewFromInt(3100),
		Components: []structureComponent{
			{Code: "PENSION", Name: "Pension Sacrifice", Type: "deduction",
				AmountType: "fixed", Amount: decimal.NewFromInt(300), PreTax: true},
			{Code: "UNION", Name: "Union Dues", Type: "deduction",
				AmountType: "fixed", Amount: decimal.NewFromInt(50)},
		},
	}
	res := buildPipeline(sv, nil, time.Time{}, march2026())

	// gross is unaffected by deductions.
	if !res.Gross.Equal(decimal.NewFromInt(3100)) {
		t.Fatalf("gross: got %s want 3100", res.Gross)
	}
	// pre-tax reduces taxable; post-tax does not.
	if !res.TaxableGross.Equal(decimal.NewFromInt(2800)) {
		t.Fatalf("taxable: got %s want 2800 (3100-300 pre-tax)", res.TaxableGross)
	}
	if len(res.PretaxDeductions) != 1 || res.PretaxDeductions[0].Code != "PENSION" {
		t.Fatalf("expected one PENSION pre-tax deduction, got %+v", res.PretaxDeductions)
	}
	if len(res.PosttaxDeductions) != 1 || res.PosttaxDeductions[0].Code != "UNION" {
		t.Fatalf("expected one UNION post-tax deduction, got %+v", res.PosttaxDeductions)
	}
}

// TestBuildPipeline_TaxableComponentFlag honours a per-earning taxable=false.
func TestBuildPipeline_TaxableComponentFlag(t *testing.T) {
	sv := structureData{
		BaseSalary: decimal.NewFromInt(3000),
		Components: []structureComponent{
			{Code: "TRANSPORT", Name: "Transport (non-tax)", Type: "earning",
				AmountType: "fixed", Amount: decimal.NewFromInt(200), Taxable: boolPtr(false)},
			{Code: "HRA", Name: "Housing (taxable)", Type: "earning",
				AmountType: "fixed", Amount: decimal.NewFromInt(400), Taxable: boolPtr(true)},
		},
	}
	res := buildPipeline(sv, nil, time.Time{}, march2026())

	if !res.Gross.Equal(decimal.NewFromInt(3600)) {
		t.Fatalf("gross: got %s want 3600", res.Gross)
	}
	// taxable = base(3000) + HRA(400); TRANSPORT excluded.
	if !res.TaxableGross.Equal(decimal.NewFromInt(3400)) {
		t.Fatalf("taxable: got %s want 3400", res.TaxableGross)
	}
}

// TestBuildPipeline_NetFormula verifies net = gross − pretax − tax − post
// using a zero-tax pipeline (tax is applied later in FinalizePayslip).
func TestBuildPipeline_NetFormula(t *testing.T) {
	sv := structureData{
		BaseSalary: decimal.NewFromInt(5000),
		Components: []structureComponent{
			{Code: "PENSION", Type: "deduction", AmountType: "fixed",
				Amount: decimal.NewFromInt(250), PreTax: true},
			{Code: "UNION", Type: "deduction", AmountType: "fixed",
				Amount: decimal.NewFromInt(40)},
		},
	}
	res := buildPipeline(sv, nil, time.Time{}, march2026())

	var pretax, posttax decimal.Decimal
	for _, l := range res.PretaxDeductions {
		pretax = pretax.Add(l.Amount)
	}
	for _, l := range res.PosttaxDeductions {
		posttax = posttax.Add(l.Amount)
	}
	tax := decimal.Zero // no pack in the pure pipeline
	net := res.Gross.Sub(pretax).Sub(tax).Sub(posttax)
	if !net.Equal(decimal.NewFromInt(4710)) {
		t.Fatalf("net: got %s want 4710 (5000-250-0-40)", net)
	}
}

// TestProrationFactor_Boundaries checks the edge cases of the calendar-day
// proration factor.
func TestProrationFactor_Boundaries(t *testing.T) {
	p := march2026()
	if got := prorationFactor(time.Time{}, p); !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("zero join date → full period, got %s", got)
	}
	if got := prorationFactor(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), p); !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("join before period → full period, got %s", got)
	}
	if got := prorationFactor(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), p); !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("join on start → full period, got %s", got)
	}
	if got := prorationFactor(time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC), p); !got.Equal(decimal.Zero) {
		t.Fatalf("join after period → zero, got %s", got)
	}
}

// TestMonthsEmployedYTD covers the cumulative month index used by the CN
// cumulative-withholding pack.
func TestMonthsEmployedYTD(t *testing.T) {
	end := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	// Joined before the tax year → counts from January (3 months through March).
	if got := monthsEmployedYTD(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), end, 2026); got != 3 {
		t.Fatalf("pre-year joiner: got %d want 3", got)
	}
	// Joined in February of the tax year → 2 months (Feb, Mar).
	if got := monthsEmployedYTD(time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC), end, 2026); got != 2 {
		t.Fatalf("feb joiner: got %d want 2", got)
	}
	// Joins next year → 0.
	if got := monthsEmployedYTD(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), end, 2026); got != 0 {
		t.Fatalf("future joiner: got %d want 0", got)
	}
}
