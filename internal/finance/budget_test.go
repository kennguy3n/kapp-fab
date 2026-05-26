package finance

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// TestBudgetKTypes_SchemaValidJSON exercises the embedded KType JSON
// literals through the same `json.Valid` gate as the Phase C finance
// init() so a malformed budget schema fails fast in CI rather than at
// tenant-bootstrap time on a live install.
func TestBudgetKTypes_SchemaValidJSON(t *testing.T) {
	t.Parallel()
	kts := BudgetKTypes()
	if len(kts) != 2 {
		t.Fatalf("expected 2 budget KTypes, got %d", len(kts))
	}
	for _, kt := range kts {
		if !json.Valid(kt.Schema) {
			t.Fatalf("KType %q schema is not valid JSON", kt.Name)
		}
		if kt.Version <= 0 {
			t.Fatalf("KType %q has non-positive version %d", kt.Name, kt.Version)
		}
	}
	// Names match the documented Phase N5 surface.
	want := map[string]bool{KTypeBudget: false, KTypeBudgetLine: false}
	for _, kt := range kts {
		if _, ok := want[kt.Name]; !ok {
			t.Fatalf("unexpected KType %q in BudgetKTypes()", kt.Name)
		}
		want[kt.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("missing expected KType %q", name)
		}
	}
}

// TestIsCreditNormal codifies the sign-flip rule used by the variance
// computation: revenue / liability / equity are credit-normal so
// "actual − budget" needs to flip sign to read as over-spent / under-
// performing; asset and expense balances stay as-recorded.
func TestIsCreditNormal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ  string
		want bool
	}{
		{"revenue", true},
		{"liability", true},
		{"equity", true},
		{"asset", false},
		{"expense", false},
		{"", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := isCreditNormal(tc.typ); got != tc.want {
			t.Fatalf("isCreditNormal(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

// TestIsFavourableVariance pins the per-account-type favourability
// rule used to bucket TotalFavourableVariance / TotalUnfavourable
// Variance on the variance report footer. Revenue over-perform and
// expense under-spend are favourable; expense over-spend and
// revenue under-perform are unfavourable. Zero variance falls into
// the favourable bucket so a perfectly-on-plan period doesn't get
// rendered as a regression in the dashboard.
func TestIsFavourableVariance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		accountType string
		variance    decimal.Decimal
		want        bool
	}{
		{"revenue over plan = favourable", "revenue", decimal.NewFromInt(500), true},
		{"revenue under plan = unfavourable", "revenue", decimal.NewFromInt(-500), false},
		{"revenue on plan = favourable", "revenue", decimal.Zero, true},
		{"expense over plan = unfavourable", "expense", decimal.NewFromInt(500), false},
		{"expense under plan = favourable", "expense", decimal.NewFromInt(-500), true},
		{"expense on plan = favourable", "expense", decimal.Zero, true},
		{"asset over plan = favourable", "asset", decimal.NewFromInt(100), true},
		{"asset under plan = unfavourable", "asset", decimal.NewFromInt(-100), false},
		{"liability over plan = favourable", "liability", decimal.NewFromInt(100), true},
		{"equity zero = favourable", "equity", decimal.Zero, true},
		{"unknown type follows as-recorded sign", "", decimal.NewFromInt(-1), false},
	}
	for _, tc := range cases {
		if got := isFavourableVariance(tc.accountType, tc.variance); got != tc.want {
			t.Fatalf("%s: isFavourableVariance(%q, %s) = %v, want %v",
				tc.name, tc.accountType, tc.variance.String(), got, tc.want)
		}
	}
}

// TestRowMatchesCurrentMonth verifies the in-progress month filter
// used by the variance alert handler. Only the current calendar month
// raises notifications; past + future rows are excluded.
func TestRowMatchesCurrentMonth(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, time.July, 17, 14, 32, 0, 0, time.UTC)
	cases := []struct {
		period string
		want   bool
	}{
		{"2025-07", true},
		{"2025-06", false},
		{"2025-08", false},
		{"2024-07", false},
		{"", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		row := VarianceRow{Period: tc.period}
		if got := rowMatchesCurrentMonth(row, now); got != tc.want {
			t.Fatalf("rowMatchesCurrentMonth(period=%q, now=%s) = %v, want %v",
				tc.period, now.Format("2006-01-02"), got, tc.want)
		}
	}
}

// TestDistinctAccountCodes asserts the helper returns each account
// code exactly once and preserves first-seen order so the
// loadAccountMeta() SQL `IN (...)` clause stays deterministic in
// tests that snapshot the generated query.
func TestDistinctAccountCodes(t *testing.T) {
	t.Parallel()
	lines := []BudgetLine{
		{AccountCode: "5000"},
		{AccountCode: "6000"},
		{AccountCode: "5000"}, // dup
		{AccountCode: "4000"},
		{AccountCode: "6000"}, // dup
	}
	got := distinctAccountCodes(lines)
	want := []string{"5000", "6000", "4000"}
	if len(got) != len(want) {
		t.Fatalf("distinctAccountCodes: len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, code := range want {
		if got[i] != code {
			t.Fatalf("distinctAccountCodes[%d] = %q, want %q", i, got[i], code)
		}
	}
}

// TestDefaultVarianceThreshold pins the platform-default threshold
// at 10% so a regression that silently flips it to e.g. 1% (and
// floods every tenant's inbox) is caught here rather than in
// production.
func TestDefaultVarianceThreshold(t *testing.T) {
	t.Parallel()
	want := decimal.NewFromFloat(0.10)
	if !DefaultVarianceThreshold.Equal(want) {
		t.Fatalf("DefaultVarianceThreshold = %s, want %s",
			DefaultVarianceThreshold.String(), want.String())
	}
}

// TestNullIfEmpty asserts the helper distinguishes "" → SQL NULL
// from a populated string → the string itself. The Postgres driver
// needs untyped nil to bind to NULL on a TEXT column; passing ""
// would insert an empty string instead.
func TestNullIfEmpty(t *testing.T) {
	t.Parallel()
	if got := nullIfEmpty(""); got != nil {
		t.Fatalf("nullIfEmpty(\"\") = %v, want nil", got)
	}
	if got := nullIfEmpty("hello"); got != "hello" {
		t.Fatalf("nullIfEmpty(\"hello\") = %v, want %q", got, "hello")
	}
}
