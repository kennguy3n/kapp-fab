package agents

import (
	"testing"
	"time"
)

// TestParseAgentDateBounds locks in the BUG_0002 fix: agent-supplied
// YYYY-MM-DD `to` values must be parsed as the end of the calendar
// day, not midnight, so the underlying SQL filter
// `je.posted_at <= $to` in finance.loadActualsTx includes every
// entry posted on the requested end date. The `from` bound stays
// at midnight because the SQL filter is `je.posted_at >= $from`,
// which already includes a midnight start naturally.
func TestParseAgentDateBounds(t *testing.T) {
	t.Parallel()

	t.Run("from is midnight UTC", func(t *testing.T) {
		t.Parallel()
		got, err := parseAgentDate("2025-07-31")
		if err != nil {
			t.Fatalf("parseAgentDate: %v", err)
		}
		want := time.Date(2025, time.July, 31, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("parseAgentDate = %s, want %s", got, want)
		}
	})

	t.Run("to is end of day UTC inclusive of final nanosecond", func(t *testing.T) {
		t.Parallel()
		got, err := parseAgentDateEnd("2025-07-31")
		if err != nil {
			t.Fatalf("parseAgentDateEnd: %v", err)
		}
		want := time.Date(2025, time.July, 31, 23, 59, 59, int(time.Second-1), time.UTC)
		if !got.Equal(want) {
			t.Fatalf("parseAgentDateEnd = %s, want %s", got, want)
		}
		// Defence-in-depth: an entry posted at 23:59:59.999
		// must be <= parseAgentDateEnd. With nsec=0 the prior
		// implementation would silently drop it.
		subSecond := time.Date(2025, time.July, 31, 23, 59, 59, 999_000_000, time.UTC)
		if !subSecond.Before(got.Add(time.Nanosecond)) {
			t.Fatalf("parseAgentDateEnd excludes sub-second entry: %s vs %s", subSecond, got)
		}
	})

	t.Run("to rejects garbage", func(t *testing.T) {
		t.Parallel()
		if _, err := parseAgentDateEnd("not-a-date"); err == nil {
			t.Fatal("parseAgentDateEnd accepted invalid input")
		}
	})
}
