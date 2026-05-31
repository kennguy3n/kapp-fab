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
