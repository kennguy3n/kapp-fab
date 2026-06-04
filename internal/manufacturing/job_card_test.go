package manufacturing

import "testing"

// TestJobCardCanTransitionTo pins the shop-floor job-card state
// machine. CompleteJobCard relies on this matrix to reject reopening a
// completed card — a completed card may have already triggered the work
// order's inventory moves, so a backwards move would desynchronise the
// shop-floor record from the ledger.
func TestJobCardCanTransitionTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from string
		to   string
		want bool
	}{
		// Idempotent re-assertion is always allowed so HTTP / KChat
		// retries don't fail.
		{JobCardStatusPending, JobCardStatusPending, true},
		{JobCardStatusInProgress, JobCardStatusInProgress, true},
		{JobCardStatusCompleted, JobCardStatusCompleted, true},

		// Legal forward transitions.
		{JobCardStatusPending, JobCardStatusInProgress, true},
		{JobCardStatusPending, JobCardStatusCompleted, true},
		{JobCardStatusInProgress, JobCardStatusCompleted, true},

		// Backwards moves are rejected.
		{JobCardStatusInProgress, JobCardStatusPending, false},
		{JobCardStatusCompleted, JobCardStatusInProgress, false},
		{JobCardStatusCompleted, JobCardStatusPending, false},

		// Unknown source status rejects every outbound move.
		{"bogus", JobCardStatusInProgress, false},
	}
	for _, tc := range cases {
		j := JobCard{Status: tc.from}
		if got := j.CanTransitionTo(tc.to); got != tc.want {
			t.Errorf("JobCard{%s}.CanTransitionTo(%s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}
