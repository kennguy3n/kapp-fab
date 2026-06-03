package platform

import (
	"math/big"
	"testing"
	"time"
)

// uuidToInt parses a canonical UUID string back into its 128-bit integer
// value so tests can assert ordering and even spacing of computed bounds.
func uuidToInt(t *testing.T, s string) *big.Int {
	t.Helper()
	hex := ""
	for _, r := range s {
		if r != '-' {
			hex += string(r)
		}
	}
	if len(hex) != 32 {
		t.Fatalf("uuid %q has %d hex digits, want 32", s, len(hex))
	}
	n, ok := new(big.Int).SetString(hex, 16)
	if !ok {
		t.Fatalf("uuid %q is not valid hex", s)
	}
	return n
}

func TestPartitionPlan_BoundsAreOrderedAndClosed(t *testing.T) {
	const total = 8
	plan := PartitionPlan("krecords", total)
	if len(plan) != total {
		t.Fatalf("got %d partitions, want %d", len(plan), total)
	}

	// First opens at MINVALUE, last closes at MAXVALUE.
	if plan[0].Lower != boundMinValue {
		t.Errorf("plan[0].Lower = %q, want %q", plan[0].Lower, boundMinValue)
	}
	if plan[total-1].Upper != boundMaxValue {
		t.Errorf("plan[last].Upper = %q, want %q", plan[total-1].Upper, boundMaxValue)
	}

	// Names are deterministic and zero-padded.
	wantName := "krecords_p00"
	if plan[0].Name != wantName {
		t.Errorf("plan[0].Name = %q, want %q", plan[0].Name, wantName)
	}
	if plan[total-1].Name != "krecords_p07" {
		t.Errorf("plan[last].Name = %q, want %q", plan[total-1].Name, "krecords_p07")
	}

	// Adjacent partitions are contiguous: each upper equals the next
	// lower, and interior boundaries strictly increase.
	var prevUpper *big.Int
	for i := 0; i < total; i++ {
		s := plan[i]
		if i > 0 && s.Lower != plan[i-1].Upper {
			t.Errorf("partition %d lower %q != previous upper %q", i, s.Lower, plan[i-1].Upper)
		}
		if i > 0 {
			cur := uuidToInt(t, s.Lower)
			if prevUpper != nil && cur.Cmp(prevUpper) != 0 {
				t.Errorf("partition %d lower does not match previous upper", i)
			}
		}
		if i < total-1 {
			prevUpper = uuidToInt(t, s.Upper)
		}
	}
}

func TestPartitionPlan_EvenlySpacedInteriorBounds(t *testing.T) {
	const total = 4
	plan := PartitionPlan("events", total)

	// Interior boundaries should sit at 1/4, 2/4, 3/4 of 2^128.
	span := new(big.Int).Lsh(big.NewInt(1), 128)
	for i := 1; i < total; i++ {
		got := uuidToInt(t, plan[i].Lower)
		want := new(big.Int).Mul(span, big.NewInt(int64(i)))
		want.Div(want, big.NewInt(int64(total)))
		if got.Cmp(want) != 0 {
			t.Errorf("boundary %d = %s, want %s", i, got.String(), want.String())
		}
	}
}

func TestPartitionPlan_StableAsTotalIsFixed(t *testing.T) {
	// Re-planning with the same total must reproduce identical bounds so
	// incrementally creating partitions never moves an existing boundary.
	a := PartitionPlan("audit_log", 16)
	b := PartitionPlan("audit_log", 16)
	if len(a) != len(b) {
		t.Fatalf("plan lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("partition %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestPartitionPlan_TotalFloor(t *testing.T) {
	for _, total := range []int{0, -5, 1} {
		plan := PartitionPlan("krecords", total)
		if len(plan) != 1 {
			t.Errorf("total=%d: got %d partitions, want 1", total, len(plan))
		}
		if plan[0].Lower != boundMinValue || plan[0].Upper != boundMaxValue {
			t.Errorf("total=%d: single partition must span MINVALUE..MAXVALUE, got [%s,%s)",
				total, plan[0].Lower, plan[0].Upper)
		}
	}
}

func TestPartitionSpec_CreateSQL(t *testing.T) {
	plan := PartitionPlan("krecords", 2)

	// First partition: open lower bound emitted as the bare MINVALUE
	// keyword, upper bound quoted as a UUID literal.
	got := plan[0].createSQL("krecords")
	want := "CREATE TABLE IF NOT EXISTS krecords_p00 PARTITION OF krecords " +
		"FOR VALUES FROM (MINVALUE) TO ('" + plan[0].Upper + "')"
	if got != want {
		t.Errorf("createSQL[0]\n got: %s\nwant: %s", got, want)
	}

	// Last partition: quoted lower bound, bare MAXVALUE upper bound.
	got = plan[1].createSQL("krecords")
	want = "CREATE TABLE IF NOT EXISTS krecords_p01 PARTITION OF krecords " +
		"FOR VALUES FROM ('" + plan[1].Lower + "') TO (MAXVALUE)"
	if got != want {
		t.Errorf("createSQL[1]\n got: %s\nwant: %s", got, want)
	}
}

func TestBoundLiteral(t *testing.T) {
	if got := boundLiteral(boundMinValue); got != "MINVALUE" {
		t.Errorf("MINVALUE sentinel = %q, want MINVALUE (bare)", got)
	}
	if got := boundLiteral(boundMaxValue); got != "MAXVALUE" {
		t.Errorf("MAXVALUE sentinel = %q, want MAXVALUE (bare)", got)
	}
	const u = "40000000-0000-0000-0000-000000000000"
	if got := boundLiteral(u); got != "'"+u+"'" {
		t.Errorf("uuid bound = %q, want single-quoted", got)
	}
}

func TestDesiredPartitionCount(t *testing.T) {
	const maxParts = 16
	cases := []struct {
		name     string
		estRows  int64
		capacity int64
		maxParts int
		want     int
	}{
		{"empty table floors to one", 0, 1_000, maxParts, 1},
		{"under one capacity", 500, 1_000, maxParts, 1},
		{"exactly one capacity", 1_000, 1_000, maxParts, 1},
		{"just over one capacity", 1_001, 1_000, maxParts, 2},
		{"exactly two capacities", 2_000, 1_000, maxParts, 2},
		{"rounds up partial", 2_500, 1_000, maxParts, 3},
		{"caps at max partitions", 1_000_000, 1_000, maxParts, maxParts},
		{"zero capacity floors to one", 5_000, 0, maxParts, 1},
		{"zero max floors to one", 5_000, 1_000, 0, 1},
		{"negative rows floor to one", -10, 1_000, maxParts, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DesiredPartitionCount(tc.estRows, tc.capacity, tc.maxParts)
			if got != tc.want {
				t.Errorf("DesiredPartitionCount(%d, %d, %d) = %d, want %d",
					tc.estRows, tc.capacity, tc.maxParts, got, tc.want)
			}
		})
	}
}

func TestDesiredPartitionCount_MonotonicNonDecreasing(t *testing.T) {
	// As estimated rows grow, the desired count must never shrink — a
	// shrink would imply dropping a partition, which range partitioning
	// cannot do safely.
	prev := 0
	for rows := int64(0); rows <= 100_000; rows += 1_000 {
		got := DesiredPartitionCount(rows, 10_000, 16)
		if got < prev {
			t.Fatalf("desired count decreased from %d to %d at rows=%d", prev, got, rows)
		}
		prev = got
	}
}

func TestAnalyzeNeeded(t *testing.T) {
	cases := []struct {
		name       string
		mod, live  int64
		churnRatio float64
		want       bool
	}{
		{"no modifications", 0, 1_000, 0.10, false},
		{"empty table with mods analyzes", 5, 0, 0.10, true},
		{"empty table no mods", 0, 0, 0.10, false},
		{"below churn ratio", 50, 1_000, 0.10, false},
		{"at churn ratio", 100, 1_000, 0.10, true},
		{"above churn ratio", 250, 1_000, 0.10, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnalyzeNeeded(tc.mod, tc.live, tc.churnRatio); got != tc.want {
				t.Errorf("AnalyzeNeeded(%d, %d, %.2f) = %v, want %v",
					tc.mod, tc.live, tc.churnRatio, got, tc.want)
			}
		})
	}
}

func TestBloatRatio(t *testing.T) {
	if got := BloatRatio(0, 0); got != 0 {
		t.Errorf("empty table ratio = %v, want 0", got)
	}
	if got := BloatRatio(25, 75); got != 0.25 {
		t.Errorf("BloatRatio(25,75) = %v, want 0.25", got)
	}
	if got := BloatRatio(100, 0); got != 1 {
		t.Errorf("all-dead ratio = %v, want 1", got)
	}
}

func TestVacuumNeeded(t *testing.T) {
	if VacuumNeeded(100, 0) {
		t.Error("non-positive threshold must disable vacuum")
	}
	if VacuumNeeded(49, 50) {
		t.Error("below threshold must not vacuum")
	}
	if !VacuumNeeded(50, 50) {
		t.Error("at threshold must vacuum")
	}
	if !VacuumNeeded(5_000, 50) {
		t.Error("well above threshold must vacuum")
	}
}

func TestDueWeekly(t *testing.T) {
	now := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	interval := 7 * 24 * time.Hour
	if !dueWeekly(time.Time{}, now, interval) {
		t.Error("never-run task must be due")
	}
	if dueWeekly(now.Add(-3*24*time.Hour), now, interval) {
		t.Error("ran 3 days ago must not be due on a weekly cadence")
	}
	if !dueWeekly(now.Add(-8*24*time.Hour), now, interval) {
		t.Error("ran 8 days ago must be due on a weekly cadence")
	}
	if !dueWeekly(now.Add(-interval), now, interval) {
		t.Error("ran exactly one interval ago must be due")
	}
}

func TestParseIndexList(t *testing.T) {
	got := parseIndexList(" idx_a , idx_b ,, bad-name , idx_c ")
	want := []string{"idx_a", "idx_b", "idx_c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidIdent(t *testing.T) {
	valid := []string{"krecords", "events_undelivered_idx", "_tmp", "a1_b2"}
	for _, s := range valid {
		if !validIdent(s) {
			t.Errorf("%q should be a valid identifier", s)
		}
	}
	invalid := []string{"", "1table", "drop table", "a;b", "public.events", "Events", "a-b", "a'b"}
	for _, s := range invalid {
		if validIdent(s) {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestDefaultDBMaintenanceConfig_Sane(t *testing.T) {
	cfg := DefaultDBMaintenanceConfig()
	if cfg.PartitionTargetCount < 1 {
		t.Error("partition target count must be >= 1")
	}
	if cfg.PartitionCapacity <= 0 {
		t.Error("partition capacity must be positive")
	}
	if cfg.ReindexInterval <= 0 {
		t.Error("reindex interval must be positive")
	}
	if cfg.ChurnRatio <= 0 || cfg.ChurnRatio >= 1 {
		t.Errorf("churn ratio %v must be in (0,1)", cfg.ChurnRatio)
	}
	if len(cfg.HighChurnIndexes) == 0 {
		t.Error("default high-churn index list must be non-empty")
	}
	for _, idx := range cfg.HighChurnIndexes {
		if !validIdent(idx) {
			t.Errorf("default index %q is not a valid identifier", idx)
		}
	}
}
