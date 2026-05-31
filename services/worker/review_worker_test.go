package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// TestReviewWorker_RecordFailureOrDeadLetter_SkipsOnContextCanceled
// pins the round-3 fix: a parent ctx cancellation (i.e. worker
// shutdown propagating Run(ctx) → drain(ctx) → per-version runCtx)
// must NOT cause attempt_count to bump. Otherwise a hot-restart
// loop could burn through MaxReviewAttempts on a single in-flight
// row and dead-letter a perfectly healthy version.
//
// The assertion shape is indirect: we hand recordFailureOrDeadLetter
// a worker whose store is nil and a pre-cancelled ctx. With the
// skip in place, the function returns before any store deref. If
// someone removes the skip, the nil-store deref panics and the
// test fails loudly.
//
// We also assert the symmetric case: a ctx with DeadlineExceeded
// (the 90s per-version timeout firing) MUST proceed past the skip
// and call the store — a genuinely slow bundle is a real pipeline
// failure that should consume retry budget. (For that branch the
// nil-store deref panics and we catch the panic to confirm the
// proceed path took effect.)
func TestReviewWorker_RecordFailureOrDeadLetter_SkipsOnContextCanceled(t *testing.T) {
	t.Parallel()

	// silentLogger discards all output so the test doesn't pollute
	// stdout with the expected "pipeline cancelled" Info line.
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	w := &ReviewWorker{
		store:       nil, // intentionally nil to detect any unintended deref
		pipeline:    nil,
		logger:      silentLogger,
		interval:    5 * time.Second,
		claimLimit:  4,
		concurrency: 4,
		workerID:    "test-worker",
		sem:         semaphore.NewWeighted(4),
	}

	versionID := uuid.New()
	cause := errors.New("pipeline failed for synthetic reason")

	t.Run("ctx_canceled_skips_store_call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // pre-cancel

		// Deferred recover catches any unexpected nil-store deref.
		// With the skip in place, the function returns BEFORE
		// touching the store, so no panic should fire.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("recordFailureOrDeadLetter must skip on ctx.Canceled (no store call), but got panic: %v", r)
			}
		}()

		// expectedClaim is intentionally non-nil so the call site
		// would otherwise reach the store. The skip should win.
		expectedClaim := &marketplace.ReviewClaimGuard{
			ClaimedBy: "test-worker",
			ClaimedAt: time.Now(),
		}
		w.recordFailureOrDeadLetter(ctx, versionID, expectedClaim, cause)
	})

	t.Run("ctx_deadline_exceeded_proceeds_to_store", func(t *testing.T) {
		// A DeadlineExceeded ctx must NOT skip — the per-version
		// 90s deadline firing means the bundle is genuinely too
		// slow, which is a real pipeline failure worth burning
		// retry budget on. Confirmed by observing the nil-store
		// deref panic (the proceed path runs).
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
		defer cancel()
		// Sanity: the ctx must actually be DeadlineExceeded, not Canceled.
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("test setup: ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
		}

		expectedClaim := &marketplace.ReviewClaimGuard{
			ClaimedBy: "test-worker",
			ClaimedAt: time.Now(),
		}

		didPanic := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					didPanic = true
				}
			}()
			w.recordFailureOrDeadLetter(ctx, versionID, expectedClaim, cause)
		}()
		if !didPanic {
			t.Fatalf("recordFailureOrDeadLetter must proceed past skip on ctx.DeadlineExceeded and call the store (nil-store deref expected), but returned cleanly")
		}
	})
}
