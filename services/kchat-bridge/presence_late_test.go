package main

import (
	"testing"
	"time"
)

// TestLocalCalendarDateTenantTimezone is the regression guard for
// the second PR #52 review pass that flagged a day-boundary skip
// in evaluateLateArrival: dateKey was UTC-derived from
// upsertAttendance but compared against shift_assignment.shift_date,
// which stores the tenant's local calendar date. For non-UTC
// tenants whose check-in time crosses a UTC day boundary the
// dates differed and the loop silently skipped the matching
// assignment. localCalendarDate now derives the comparison key in
// the tenant's own timezone.
func TestLocalCalendarDateTenantTimezone(t *testing.T) {
	cases := []struct {
		name string
		when string
		tz   string
		want string
	}{
		{
			name: "NY 23:30 ET on April 15 stays April 15 locally (UTC = April 16)",
			when: "2026-04-16T03:30:00Z",
			tz:   "America/New_York",
			want: "2026-04-15",
		},
		{
			name: "Sydney 09:00 AEST on April 15 stays April 15 locally (UTC = April 14)",
			when: "2026-04-14T23:00:00Z",
			tz:   "Australia/Sydney",
			want: "2026-04-15",
		},
		{
			name: "UTC tenant unchanged",
			when: "2026-04-15T23:30:00Z",
			tz:   "UTC",
			want: "2026-04-15",
		},
		{
			name: "Empty timezone falls back to UTC",
			when: "2026-04-15T23:30:00Z",
			tz:   "",
			want: "2026-04-15",
		},
		{
			name: "Garbage timezone falls back to UTC",
			when: "2026-04-15T23:30:00Z",
			tz:   "Not/A/Real/Zone",
			want: "2026-04-15",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			when, err := time.Parse(time.RFC3339, tc.when)
			if err != nil {
				t.Fatalf("invalid when: %v", err)
			}
			got := localCalendarDate(when, tc.tz)
			if got != tc.want {
				t.Fatalf("localCalendarDate(%s, %q) = %s; want %s", tc.when, tc.tz, got, tc.want)
			}
		})
	}
}

// TestPickShiftForCheckInSplitShifts is the regression guard for
// the PR #52 review pass that flagged evaluateLateArrival picking
// shifts in ListByField order (updated_at DESC) rather than by
// proximity to the check-in time. For split-shift employees a
// recently re-saved Morning could steal match priority away from a
// still-relevant Evening shift, producing tardy_minutes computed
// against the wrong shift (e.g. 725 instead of 5 for an 18:05
// check-in vs Evening 18:00 / Morning 06:00).
//
// pickShiftForCheckIn now selects:
//   - the latest already-started shift relative to the check-in,
//     so the "shift the employee just clocked in for" wins;
//   - or the earliest upcoming shift when nothing has started
//     yet, so an early-arrival grace window still fires.
func TestPickShiftForCheckInSplitShifts(t *testing.T) {
	dateKey := "2026-04-15"
	mustStart := func(hhmm string) time.Time {
		s, ok := parseShiftStart(dateKey, hhmm, "UTC")
		if !ok {
			t.Fatalf("parseShiftStart(%q): not ok", hhmm)
		}
		return s
	}
	morning := shiftCandidate{StartStr: "06:00", StartUTC: mustStart("06:00")}
	evening := shiftCandidate{StartStr: "18:00", StartUTC: mustStart("18:00")}
	overnight := shiftCandidate{StartStr: "22:00", StartUTC: mustStart("22:00")}

	cases := []struct {
		name    string
		input   []shiftCandidate
		checkIn string // RFC3339 UTC
		want    string // wanted StartStr; empty = ok=false
	}{
		{
			name:    "split shift, 18:05 check-in picks Evening over Morning",
			input:   []shiftCandidate{morning, evening},
			checkIn: "2026-04-15T18:05:00Z",
			want:    "18:00",
		},
		{
			name:    "split shift, list ordering reversed (updated_at DESC simulation) still picks Evening",
			input:   []shiftCandidate{evening, morning}, // morning re-saved most recently
			checkIn: "2026-04-15T18:05:00Z",
			want:    "18:00",
		},
		{
			name:    "split shift, 06:05 check-in picks Morning, not Evening",
			input:   []shiftCandidate{morning, evening},
			checkIn: "2026-04-15T06:05:00Z",
			want:    "06:00",
		},
		{
			name:    "early arrival before any shift starts picks earliest upcoming",
			input:   []shiftCandidate{evening, morning, overnight},
			checkIn: "2026-04-15T05:55:00Z",
			want:    "06:00",
		},
		{
			name:    "single shift unchanged behaviour",
			input:   []shiftCandidate{morning},
			checkIn: "2026-04-15T06:05:00Z",
			want:    "06:00",
		},
		{
			name:    "empty candidates -> ok=false",
			input:   nil,
			checkIn: "2026-04-15T06:05:00Z",
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkIn, err := time.Parse(time.RFC3339, tc.checkIn)
			if err != nil {
				t.Fatalf("invalid checkIn: %v", err)
			}
			got, ok := pickShiftForCheckIn(tc.input, checkIn)
			if tc.want == "" {
				if ok {
					t.Fatalf("expected ok=false; got %v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("expected ok=true with StartStr=%s", tc.want)
			}
			if got.StartStr != tc.want {
				t.Fatalf("StartStr = %s; want %s", got.StartStr, tc.want)
			}
		})
	}
}

// TestParseShiftStartTenantTimezone is the regression guard for
// the PR #52 review finding that evaluateLateArrival parsed
// shift_type.start_time as UTC, ignoring the tenant's wall-clock
// timezone. A NY tenant's 09:00 shift parsed under the old code
// produced 09:00 UTC, so a 09:05 EST check-in (14:05 UTC) showed
// 305 tardy minutes instead of 5. parseShiftStart now interprets
// the (date, HH:MM) pair in the supplied IANA timezone and
// converts to UTC for the delta arithmetic.
func TestParseShiftStartTenantTimezone(t *testing.T) {
	cases := []struct {
		name      string
		dateKey   string
		startStr  string
		tz        string
		wantUTC   string
		wantOK    bool
		checkInTS string
		wantMins  int
	}{
		{
			name:      "NY 09:00 shift, 09:05 NY check-in = 5 tardy minutes",
			dateKey:   "2026-04-15",
			startStr:  "09:00",
			tz:        "America/New_York",
			wantUTC:   "2026-04-15T13:00:00Z",
			wantOK:    true,
			checkInTS: "2026-04-15T13:05:00Z",
			wantMins:  5,
		},
		{
			name:      "Sydney 09:00 shift, 09:30 AEST check-in = 30 tardy minutes",
			dateKey:   "2026-04-15",
			startStr:  "09:00",
			tz:        "Australia/Sydney",
			wantUTC:   "2026-04-14T23:00:00Z",
			wantOK:    true,
			checkInTS: "2026-04-14T23:30:00Z",
			wantMins:  30,
		},
		{
			name:     "UTC tenant, 09:00 shift parses as 09:00 UTC",
			dateKey:  "2026-04-15",
			startStr: "09:00",
			tz:       "UTC",
			wantUTC:  "2026-04-15T09:00:00Z",
			wantOK:   true,
		},
		{
			name:     "Empty timezone falls back to UTC (legacy tenants)",
			dateKey:  "2026-04-15",
			startStr: "09:00",
			tz:       "",
			wantUTC:  "2026-04-15T09:00:00Z",
			wantOK:   true,
		},
		{
			name:     "Garbage timezone falls back to UTC, doesn't crash",
			dateKey:  "2026-04-15",
			startStr: "09:00",
			tz:       "Not/A/Real/Zone",
			wantUTC:  "2026-04-15T09:00:00Z",
			wantOK:   true,
		},
		{
			name:     "Malformed date returns ok=false",
			dateKey:  "2026/04/15",
			startStr: "09:00",
			tz:       "UTC",
			wantOK:   false,
		},
		{
			name:     "Malformed time returns ok=false",
			dateKey:  "2026-04-15",
			startStr: "9am",
			tz:       "UTC",
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseShiftStart(tc.dateKey, tc.startStr, tc.tz)
			if ok != tc.wantOK {
				t.Fatalf("parseShiftStart ok=%v; want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			want, err := time.Parse(time.RFC3339, tc.wantUTC)
			if err != nil {
				t.Fatalf("invalid wantUTC: %v", err)
			}
			if !got.Equal(want) {
				t.Fatalf("parseShiftStart = %s; want %s", got.UTC().Format(time.RFC3339), tc.wantUTC)
			}
			if tc.checkInTS != "" {
				checkIn, err := time.Parse(time.RFC3339, tc.checkInTS)
				if err != nil {
					t.Fatalf("invalid checkInTS: %v", err)
				}
				gotMins := int(checkIn.Sub(got) / time.Minute)
				if gotMins != tc.wantMins {
					t.Fatalf("tardy minutes = %d; want %d (UTC tx start=%s)", gotMins, tc.wantMins, got.UTC().Format(time.RFC3339))
				}
			}
		})
	}
}
