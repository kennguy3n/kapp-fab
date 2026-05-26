package main

import (
	"testing"
	"time"
)

// TestEndOfDayAdvancesToInclusiveEnd locks in the BUG_0001 fix: a
// caller-supplied YYYY-MM-DD `to` parameter must be advanced to
// 23:59:59 on that calendar day so the SQL filter
// `je.posted_at <= $to` includes every journal entry posted that
// day, not only entries posted at exactly midnight. The advance
// also matches the default To semantics in
// finance.BudgetStore.BudgetVsActual which uses Dec 31 23:59:59 when
// the caller leaves To zero.
func TestEndOfDayAdvancesToInclusiveEnd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			name: "utc midnight to end of day",
			in:   time.Date(2025, time.July, 31, 0, 0, 0, 0, time.UTC),
			want: time.Date(2025, time.July, 31, 23, 59, 59, int(time.Second-1), time.UTC),
		},
		{
			name: "leap-day boundary",
			in:   time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
			want: time.Date(2024, time.February, 29, 23, 59, 59, int(time.Second-1), time.UTC),
		},
		{
			name: "preserves caller-supplied location",
			in:   time.Date(2025, time.July, 31, 0, 0, 0, 0, time.FixedZone("ICT", 7*60*60)),
			want: time.Date(2025, time.July, 31, 23, 59, 59, int(time.Second-1), time.FixedZone("ICT", 7*60*60)),
		},
		{
			// Sub-second precision regression: an entry posted at
			// 23:59:59.500 must compare <= endOfDay. With nsec=0,
			// the filter excludes it; with nsec=999999999 it
			// passes. This case pins the inclusive contract.
			name: "final sub-second is inclusive",
			in:   time.Date(2025, time.July, 31, 0, 0, 0, 0, time.UTC),
			want: time.Date(2025, time.July, 31, 23, 59, 59, 999999999, time.UTC),
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := endOfDay(tc.in)
			if !got.Equal(tc.want) {
				t.Fatalf("endOfDay(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}
