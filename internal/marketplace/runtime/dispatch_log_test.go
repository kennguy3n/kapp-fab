package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestWriteDispatchLogComplete_RejectsInconsistentCompletion locks
// in the pre-flight guard added in response to Devin Review
// ANALYSIS_0002 on PR #128. The DB CHECK
// marketplace_dispatch_log_completion_consistent_chk (migration
// 000069 lines 324-327) requires that when completed_at IS NOT
// NULL, at least one of response_status or error is non-NULL. A
// caller that passed (status=0, sendErr=nil) would silently mint a
// row violating that CHECK on the SQL round-trip; we instead
// surface ErrDispatchLogCompletionInconsistent locally so the bug
// is visible at the call site, not as an opaque Postgres
// constraint-violation error.
//
// All current callers (Dispatcher.Invoke + transportHooks.Dispatch)
// only invoke this helper when they have either a non-zero status
// (HTTP round-trip completed) or a non-nil error (transport
// failure), so the guard exists for future refactors and new
// callers — not for present-day code paths.
func TestWriteDispatchLogComplete_RejectsInconsistentCompletion(t *testing.T) {
	// nil pool is fine — the guard fires before any DB I/O.
	tenantID := uuid.New()
	rowID := uuid.New()

	err := writeDispatchLogComplete(context.Background(), nil, tenantID, rowID, 0, 0, nil)
	if !errors.Is(err, ErrDispatchLogCompletionInconsistent) {
		t.Fatalf("status=0, sendErr=nil: got err=%v, want %v", err, ErrDispatchLogCompletionInconsistent)
	}

	// Negative latency must NOT bypass the guard — the SQL would
	// elide latency but still write completed_at = now() with both
	// audit fields NULL.
	err = writeDispatchLogComplete(context.Background(), nil, tenantID, rowID, 0, -1*time.Millisecond, nil)
	if !errors.Is(err, ErrDispatchLogCompletionInconsistent) {
		t.Fatalf("status=0, sendErr=nil, negative latency: got err=%v, want %v", err, ErrDispatchLogCompletionInconsistent)
	}
}

// TestWriteDispatchLogComplete_NilRowID_NoOp asserts that a
// zero-UUID rowID short-circuits before the inconsistency guard.
// This is the documented "in-flight INSERT failed, no row to
// complete" path; callers in this state have already surfaced the
// start error and just need the helper to return nil cleanly so
// the dispatcher's main loop can continue without a second error
// to handle.
func TestWriteDispatchLogComplete_NilRowID_NoOp(t *testing.T) {
	if err := writeDispatchLogComplete(context.Background(), nil, uuid.New(), uuid.Nil, 0, 0, nil); err != nil {
		t.Fatalf("nil rowID: got err=%v, want nil", err)
	}
	// Also verify it returns nil when the caller did supply a
	// status/error — the rowID guard must come first regardless.
	if err := writeDispatchLogComplete(context.Background(), nil, uuid.New(), uuid.Nil, 500, time.Millisecond, errors.New("transport boom")); err != nil {
		t.Fatalf("nil rowID with status+err: got err=%v, want nil", err)
	}
}
