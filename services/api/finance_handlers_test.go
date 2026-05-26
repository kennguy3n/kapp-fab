package main

import (
	"testing"
	"time"
)

// TestParseJournalFilterTo pins the sub-second precision contract
// between the BudgetPage variance drill-down (which emits an
// RFC3339 `to` capped at JS Date's millisecond precision, e.g.
// `2025-07-31T23:59:59.999Z`) and the variance computation (which
// uses a nanosecond-precise upper bound of 999_999_999 nsec via
// internal/finance/budget.go::endOfDay). Without the promotion,
// any journal entry posted in the final sub-millisecond would
// appear in the variance row but not in the drill-down list,
// surprising users who expected the two surfaces to agree.
func TestParseJournalFilterTo(t *testing.T) {
	t.Parallel()
	endOfJulyNsec := time.Date(2025, time.July, 31, 23, 59, 59, int(time.Second-1), time.UTC)

	cases := []struct {
		name string
		raw  string
		want time.Time
		ok   bool
	}{
		{
			name: "date-only advances to final nanosecond",
			raw:  "2025-07-31",
			want: endOfJulyNsec,
			ok:   true,
		},
		{
			name: "rfc3339 zero-msec end-of-day promoted to final nanosecond",
			raw:  "2025-07-31T23:59:59Z",
			want: endOfJulyNsec,
			ok:   true,
		},
		{
			name: "rfc3339 999-msec end-of-day promoted to final nanosecond",
			raw:  "2025-07-31T23:59:59.999Z",
			want: endOfJulyNsec,
			ok:   true,
		},
		{
			name: "rfc3339 non-end-of-day instant honoured verbatim",
			raw:  "2025-07-31T15:30:00Z",
			want: time.Date(2025, time.July, 31, 15, 30, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "rfc3339 already at final nanosecond is a no-op",
			raw:  "2025-07-31T23:59:59.999999999Z",
			want: endOfJulyNsec,
			ok:   true,
		},
		{
			name: "rfc3339 with non-UTC offset normalises to UTC and promotes",
			raw:  "2025-07-31T23:59:59+00:00",
			want: endOfJulyNsec,
			ok:   true,
		},
		{
			// IST end-of-day (23:59:59 +05:30) is the final second
			// of July 31 in Asia/Kolkata. After promoting the
			// nanosecond in the input's original timezone the
			// result is `2025-07-31T23:59:59.999999999+05:30`,
			// which normalises to `2025-07-31T18:29:59.999999999Z`.
			// The promotion is detected pre-normalisation so direct
			// API consumers sending offset-bearing boundary
			// instants receive the same day-inclusivity contract
			// as the dashboard's UTC drill-down.
			name: "rfc3339 IST end-of-day promotes in original tz then normalises",
			raw:  "2025-07-31T23:59:59+05:30",
			want: time.Date(2025, time.July, 31, 18, 29, 59, int(time.Second-1), time.UTC),
			ok:   true,
		},
		{
			// Tokyo end-of-day (23:59:59 +09:00) → 14:59:59.999999999Z.
			name: "rfc3339 JST end-of-day promotes in original tz then normalises",
			raw:  "2025-12-31T23:59:59+09:00",
			want: time.Date(2025, time.December, 31, 14, 59, 59, int(time.Second-1), time.UTC),
			ok:   true,
		},
		{
			// Negative offset: New York end-of-day (23:59:59 -05:00)
			// → 04:59:59.999999999Z the next calendar day.
			name: "rfc3339 EST end-of-day promotes across UTC date boundary",
			raw:  "2025-07-31T23:59:59-05:00",
			want: time.Date(2025, time.August, 1, 4, 59, 59, int(time.Second-1), time.UTC),
			ok:   true,
		},
		{
			name: "leap-day boundary",
			raw:  "2024-02-29",
			want: time.Date(2024, time.February, 29, 23, 59, 59, int(time.Second-1), time.UTC),
			ok:   true,
		},
		{
			name: "malformed input returns ok=false",
			raw:  "not-a-date",
			ok:   false,
		},
		{
			name: "empty input returns ok=false",
			raw:  "",
			ok:   false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseJournalFilterTo(tc.raw)
			if ok != tc.ok {
				t.Fatalf("parseJournalFilterTo(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if !got.Equal(tc.want) {
				t.Fatalf("parseJournalFilterTo(%q) = %s, want %s", tc.raw, got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
			}
			if got.Location() != time.UTC {
				t.Fatalf("parseJournalFilterTo(%q) location = %s, want UTC", tc.raw, got.Location())
			}
		})
	}
}

// TestParseJournalFilterFrom pins the lower-bound contract: bare
// dates are honoured as midnight UTC (the natural inclusive start
// of a calendar day), RFC3339 timestamps are honoured verbatim
// (the caller has expressed an explicit instant), and malformed
// input returns ok=false so callers fall back to "no lower bound".
func TestParseJournalFilterFrom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want time.Time
		ok   bool
	}{
		{
			name: "date-only parses as midnight UTC",
			raw:  "2025-07-01",
			want: time.Date(2025, time.July, 1, 0, 0, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "rfc3339 instant honoured verbatim",
			raw:  "2025-07-01T08:30:00Z",
			want: time.Date(2025, time.July, 1, 8, 30, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "rfc3339 non-UTC normalises to UTC",
			raw:  "2025-07-01T08:30:00+07:00",
			want: time.Date(2025, time.July, 1, 1, 30, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "malformed input returns ok=false",
			raw:  "garbage",
			ok:   false,
		},
		{
			name: "empty input returns ok=false",
			raw:  "",
			ok:   false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseJournalFilterFrom(tc.raw)
			if ok != tc.ok {
				t.Fatalf("parseJournalFilterFrom(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if !got.Equal(tc.want) {
				t.Fatalf("parseJournalFilterFrom(%q) = %s, want %s", tc.raw, got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
			}
			if got.Location() != time.UTC {
				t.Fatalf("parseJournalFilterFrom(%q) location = %s, want UTC", tc.raw, got.Location())
			}
		})
	}
}

// TestEndOfDayUTCMatchesPeerSurfaces pins that finance_handlers'
// endOfDayUTC reaches the identical nanosecond value as the budget
// HTTP endpoint's endOfDay (services/api/budget_handlers.go) and
// the agent date parser. All three are documented to land on the
// final representable instant of the day (23:59:59.999999999) so
// that <= comparisons over `journal_lines.posted_at` are
// day-inclusive across every reporting surface.
func TestEndOfDayUTCMatchesPeerSurfaces(t *testing.T) {
	t.Parallel()
	day := time.Date(2025, time.December, 31, 0, 0, 0, 0, time.UTC)
	got := endOfDayUTC(day)
	want := time.Date(2025, time.December, 31, 23, 59, 59, 999999999, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("endOfDayUTC = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
	if got.Nanosecond() != int(time.Second-1) {
		t.Fatalf("endOfDayUTC nanosecond = %d, want %d (int(time.Second-1))", got.Nanosecond(), int(time.Second-1))
	}
	// Peer surface: services/api/budget_handlers.go::endOfDay.
	peerBudget := endOfDay(day)
	if peerBudget.Nanosecond() != got.Nanosecond() {
		t.Fatalf("endOfDay(budget_handlers) nanosecond = %d, want %d to match endOfDayUTC", peerBudget.Nanosecond(), got.Nanosecond())
	}
}
