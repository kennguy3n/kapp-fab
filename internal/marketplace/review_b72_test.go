package marketplace

import "testing"

// Unit-level coverage for the B7.2 review-state additions:
// dead_letter status, IsTerminal()/Valid() updates, and
// reviewStatusTransitionAllowed() acceptance of the
// submitted→dead_letter edge plus rejection of every outbound
// edge from dead_letter (it is the system-driven terminal —
// admin Rescan moves it out of band via ResetReviewStateForRescan,
// NOT via UpdateReviewState).
//
// Lives in the marketplace package (not a _test split) so it can
// exercise the un-exported reviewStatusTransitionAllowed
// directly — the transition graph is the authoritative gate on
// every UpdateReviewState call and tests against it pin the
// shape of the state machine.

func TestReviewStatus_DeadLetter_IsTerminal(t *testing.T) {
	if !ReviewStatusDeadLetter.IsTerminal() {
		t.Fatalf("dead_letter must be terminal")
	}
	if !ReviewStatusDeadLetter.Valid() {
		t.Fatalf("dead_letter must be Valid()")
	}
	// Sanity: the other three terminals stay terminal.
	for _, s := range []ReviewStatus{
		ReviewStatusApproved,
		ReviewStatusRejected,
		ReviewStatusWithdrawn,
	} {
		if !s.IsTerminal() {
			t.Fatalf("%s expected terminal", s)
		}
	}
	// Sanity: the intermediates stay non-terminal.
	for _, s := range []ReviewStatus{
		ReviewStatusSubmitted,
		ReviewStatusAutomatedPassed,
		ReviewStatusManualReview,
	} {
		if s.IsTerminal() {
			t.Fatalf("%s must NOT be terminal", s)
		}
	}
}

func TestReviewStatusTransitionAllowed_DeadLetterIngress(t *testing.T) {
	// submitted → dead_letter is the B7.2 retry-exhausted path
	// and must be allowed without a reviewer (the worker drives
	// the transition).
	if !reviewStatusTransitionAllowed(ReviewStatusSubmitted, ReviewStatusDeadLetter) {
		t.Fatalf("submitted→dead_letter must be allowed")
	}
}

func TestReviewStatusTransitionAllowed_DeadLetterEgress(t *testing.T) {
	// No UpdateReviewState call may move a row OUT of
	// dead_letter — admin Rescan goes through
	// ResetReviewStateForRescan, not the transition graph.
	for _, to := range []ReviewStatus{
		ReviewStatusSubmitted,
		ReviewStatusAutomatedPassed,
		ReviewStatusManualReview,
		ReviewStatusApproved,
		ReviewStatusRejected,
		ReviewStatusWithdrawn,
	} {
		if reviewStatusTransitionAllowed(ReviewStatusDeadLetter, to) {
			t.Fatalf("dead_letter→%s must be forbidden via UpdateReviewState", to)
		}
	}
}

func TestReviewStatusTransitionAllowed_NonSubmittedToDeadLetter(t *testing.T) {
	// Only `submitted` (the worker-claimable state) may
	// transition to dead_letter — the worker doesn't run
	// against rows in other states, so any other ingress would
	// be a bug.
	for _, from := range []ReviewStatus{
		ReviewStatusAutomatedPassed,
		ReviewStatusManualReview,
		ReviewStatusApproved,
		ReviewStatusRejected,
		ReviewStatusWithdrawn,
	} {
		if reviewStatusTransitionAllowed(from, ReviewStatusDeadLetter) {
			t.Fatalf("%s→dead_letter must be forbidden (only submitted may dead-letter)", from)
		}
	}
}

func TestMaxReviewAttempts_PositiveBound(t *testing.T) {
	// Defence in depth: a value of 0 would dead-letter every
	// row on the first attempt; a negative value would
	// underflow the >= comparison in RecordAttemptFailure.
	// Pin a sensible minimum so an accidental edit can't
	// silently turn the queue into a self-destruct fuse.
	if MaxReviewAttempts < 3 {
		t.Fatalf("MaxReviewAttempts must be >= 3, got %d", MaxReviewAttempts)
	}
}

// TruncateUTF8 powers RecordAttemptFailure's error-message
// bound (1 KiB). PostgreSQL `text` columns reject invalid UTF-8,
// so a naive byte-slice would risk the failure-recording UPDATE
// itself failing if an upstream error message ever carried a
// multi-byte rune that straddled the cap. These cases pin the
// rune-boundary truncation behaviour the production helper must
// preserve.
func TestTruncateUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty_passthrough", "", 16, ""},
		{"under_cap", "hello", 16, "hello"},
		{"at_cap_ascii", "0123456789abcdef", 16, "0123456789abcdef"},
		{"over_cap_ascii", "0123456789abcdefghij", 16, "0123456789abcdef"},
		// "é" is 2 bytes (0xC3 0xA9). "abcéfg" = a(1) b(1) c(1)
		// é(2) f(1) g(1) = 7 bytes. A naive slice at 4 would
		// keep "abc" + first byte of "é", producing invalid
		// UTF-8 (the 0xC3 lead byte without its 0xA9
		// continuation). We must stop one rune earlier and
		// return "abc".
		{"rune_boundary_split_at_4", "abcéfg", 4, "abc"},
		// "abcé" is exactly 5 bytes — fits the cap and the
		// trailing 'f' does NOT fit (would push to 6). Return
		// "abcé" intact (no rune split, no partial emission).
		{"rune_boundary_fits_exact", "abcéfg", 5, "abcé"},
		{"rune_boundary_plus_one", "abcéfg", 6, "abcéf"},
		// 4-byte rune (emoji 😀 = 0xF0 0x9F 0x98 0x80). Cap of 3
		// must drop the partial.
		{"emoji_partial_dropped", "ab😀", 3, "ab"},
		{"emoji_full_kept", "ab😀", 6, "ab😀"},
		// Pre-existing invalid byte (lone 0xC3 with no follow-on)
		// must be dropped, not propagated.
		{"invalid_prefix_dropped", "ab\xC3", 16, "ab"},
		{"zero_max", "anything", 0, ""},
		{"negative_max", "anything", -1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateUTF8(c.in, c.max)
			if got != c.want {
				t.Fatalf("TruncateUTF8(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
			if len(got) > c.max && c.max > 0 {
				t.Fatalf("result %q (%d bytes) exceeds cap %d", got, len(got), c.max)
			}
		})
	}
}
