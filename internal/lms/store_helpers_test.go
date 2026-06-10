package lms

import (
	"testing"
	"time"
)

// TestWithClock exercises the clock-injection helpers on every new
// store. They take no DB, so this runs in the hermetic suite.
func TestWithClock(t *testing.T) {
	fixed := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := func() time.Time { return fixed }

	lp := (&LearningPathStore{}).WithClock(clock)
	if !lp.now().Equal(fixed) {
		t.Fatal("LearningPathStore clock not applied")
	}
	// nil clock is ignored (keeps existing).
	if lp.WithClock(nil).now == nil {
		t.Fatal("nil clock should be ignored")
	}

	g := (&GamificationStore{}).WithClock(clock)
	if !g.now().Equal(fixed) {
		t.Fatal("GamificationStore clock not applied")
	}
	d := (&DiscussionStore{}).WithClock(clock)
	if !d.now().Equal(fixed) {
		t.Fatal("DiscussionStore clock not applied")
	}
	s := (&ScormStore{}).WithClock(clock)
	if !s.now().Equal(fixed) {
		t.Fatal("ScormStore clock not applied")
	}
	x := (&XAPIStore{}).WithClock(clock)
	if !x.now().Equal(fixed) {
		t.Fatal("XAPIStore clock not applied")
	}
}
