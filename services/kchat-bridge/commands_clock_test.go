package main

import (
	"testing"
	"time"
)

// TestCommandDispatcherClockDefaultsToNow verifies the clock helper
// falls back to UTC wall-clock when no override has been wired so
// production callers don't need to construct a clock fn explicitly.
func TestCommandDispatcherClockDefaultsToNow(t *testing.T) {
	t.Parallel()
	d := &CommandDispatcher{}
	before := time.Now().UTC().Add(-time.Second)
	got := d.clock()
	after := time.Now().UTC().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("clock() = %s; expected wall-clock between %s and %s", got, before, after)
	}
}

// TestCommandDispatcherWithClockOverrides locks the ANALYSIS_0007
// fix: /budget variance and other MTD-window handlers must be
// driven by an injectable clock so tests can pin "today" to a
// fixed timestamp. Hardcoded time.Now().UTC() at the call site
// would render those code paths untestable against fixed
// expectations.
func TestCommandDispatcherWithClockOverrides(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC)
	d := (&CommandDispatcher{}).WithClock(func() time.Time { return fixed })
	got := d.clock()
	if !got.Equal(fixed) {
		t.Fatalf("clock() = %s; want %s", got, fixed)
	}
}

// TestCommandDispatcherWithClockRejectsNil documents the contract
// that WithClock(nil) is a no-op — the dispatcher still has a usable
// clock afterwards. This avoids the footgun where a caller
// accidentally clears the override and gets a nil-deref later.
func TestCommandDispatcherWithClockRejectsNil(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC)
	d := (&CommandDispatcher{}).WithClock(func() time.Time { return fixed })
	d.WithClock(nil)
	got := d.clock()
	if !got.Equal(fixed) {
		t.Fatalf("clock() after WithClock(nil) = %s; want unchanged %s", got, fixed)
	}
}
