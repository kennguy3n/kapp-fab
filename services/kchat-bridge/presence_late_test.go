package main

import (
	"testing"
	"time"
)

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
