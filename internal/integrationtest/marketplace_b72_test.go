//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// B7.2 — Review worker hardening: parallel-per-version with
// semaphore, retry counter + dead-letter queue, multi-worker
// leader election (drop leader singleton on the review worker).
//
// These tests pin the store-layer contracts the worker depends
// on:
//
//   * RecordAttemptFailure increments attempt_count atomically
//     under the claim guard, clears the claim so the next poll
//     re-picks the row immediately, and returns
//     ErrReviewMaxAttemptsExceeded the moment the new count
//     reaches MaxReviewAttempts (without itself making the
//     dead_letter transition — that's the worker's job, going
//     through the normal UpdateReviewState gate so the
//     transition graph still applies).
//
//   * ResetReviewStateForRescan resets attempt_count to 0,
//     last_attempt_error to '', and last_attempt_at to NULL so
//     a rescued version gets the full MaxReviewAttempts budget
//     on its next worker pass (reflects admin "this time it'll
//     work" intent).
//
//   * Two concurrent workers (different workerID) hitting the
//     same submitted row never both successfully claim it: the
//     UPDATE…RETURNING SKIP LOCKED guarantees exactly-one
//     claimer. This is the multi-replica posture B7.2
//     introduces — review worker is no longer leader-singleton.

// TestMarketplaceB72_AttemptCountIncrements pins the basic
// failure-recording contract: each call bumps attempt_count by 1,
// stamps last_attempt_error / last_attempt_at, and clears the
// claim columns so the next claim attempt sees a NULL-claimed row.
func TestMarketplaceB72_AttemptCountIncrements(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	preClaimSubmittedRows(ctx, t, h.pool)
	ver := seedExtensionVersion(t, store, "b72_inc")

	const workerID = "worker-b72-inc"
	claims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, 64)
	if err != nil {
		t.Fatalf("ClaimSubmittedReviewVersions: %v", err)
	}
	claim := findClaim(t, claims, ver)
	guard := &marketplace.ReviewClaimGuard{
		ClaimedBy: workerID,
		ClaimedAt: claim.ClaimedAt,
	}

	// First failure: count → 1, error message stored, claim cleared.
	newCount, err := store.Reviews().RecordAttemptFailure(ctx, ver, guard, "cdn fetch timeout (attempt 1)")
	if err != nil {
		t.Fatalf("RecordAttemptFailure attempt 1: %v", err)
	}
	if newCount != 1 {
		t.Errorf("attempt 1 count = %d, want 1", newCount)
	}
	got, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState: %v", err)
	}
	if got.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1", got.AttemptCount)
	}
	if !strings.Contains(got.LastAttemptError, "cdn fetch timeout") {
		t.Errorf("LastAttemptError = %q, want substring 'cdn fetch timeout'", got.LastAttemptError)
	}
	if got.LastAttemptAt == nil {
		t.Errorf("LastAttemptAt should be set after RecordAttemptFailure")
	}
	if got.Status != marketplace.ReviewStatusSubmitted {
		t.Errorf("status = %s after attempt 1, want submitted", got.Status)
	}

	// Claim should be cleared so the next poll sees the row immediately.
	reClaims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, 64)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if findClaimOrNil(reClaims, ver) == nil {
		t.Fatalf("re-claim did not return the freshly-failed row")
	}

	// Verify the second claim has a NEW ClaimedAt (RecordAttempt
	// Failure cleared the prior claim_at, ClaimSubmittedReviewVersions
	// stamped a fresh now()).
	c2 := findClaim(t, reClaims, ver)
	if !c2.ClaimedAt.After(claim.ClaimedAt) {
		t.Errorf("re-claim ClaimedAt %v should be strictly after first claim %v", c2.ClaimedAt, claim.ClaimedAt)
	}
}

// TestMarketplaceB72_DeadLetterAfterMaxAttempts drives the row
// through MaxReviewAttempts consecutive failures and verifies the
// terminal-attempt call returns ErrReviewMaxAttemptsExceeded. The
// worker would then run the dead_letter UpdateReviewState
// transition; we verify that transition succeeds and produces a
// terminal row that ClaimSubmittedReviewVersions no longer sees.
func TestMarketplaceB72_DeadLetterAfterMaxAttempts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	preClaimSubmittedRows(ctx, t, h.pool)
	ver := seedExtensionVersion(t, store, "b72_dl")

	const workerID = "worker-b72-dl"
	var lastErr error
	for i := 1; i <= marketplace.MaxReviewAttempts; i++ {
		claims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, 64)
		if err != nil {
			t.Fatalf("attempt %d: claim: %v", i, err)
		}
		claim := findClaim(t, claims, ver)
		guard := &marketplace.ReviewClaimGuard{ClaimedBy: workerID, ClaimedAt: claim.ClaimedAt}
		newCount, err := store.Reviews().RecordAttemptFailure(ctx, ver, guard,
			fmt.Sprintf("synthetic failure %d", i))
		if newCount != i {
			t.Errorf("attempt %d newCount=%d", i, newCount)
		}
		lastErr = err
		if i < marketplace.MaxReviewAttempts {
			if err != nil {
				t.Errorf("attempt %d: expected nil error, got %v", i, err)
			}
		} else {
			if !errors.Is(err, marketplace.ErrReviewMaxAttemptsExceeded) {
				t.Errorf("attempt %d: expected ErrReviewMaxAttemptsExceeded, got %v", i, err)
			}
		}
	}
	if !errors.Is(lastErr, marketplace.ErrReviewMaxAttemptsExceeded) {
		t.Fatalf("final attempt did not signal max-attempts exceeded; got %v", lastErr)
	}

	// Worker drives the dead-letter transition via the standard
	// UpdateReviewState path (no backdoor). The row stays in
	// submitted with cleared claim columns until the worker does
	// this step, so the transition mirrors what the production
	// worker does on ErrReviewMaxAttemptsExceeded.
	if _, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID:     ver,
		Status:        marketplace.ReviewStatusDeadLetter,
		Reviewer:      "system",
		ManualNotes:   fmt.Sprintf("pipeline failed %d times", marketplace.MaxReviewAttempts),
		ExpectedClaim: nil,
	}); err != nil {
		t.Fatalf("dead-letter transition: %v", err)
	}

	got, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState: %v", err)
	}
	if got.Status != marketplace.ReviewStatusDeadLetter {
		t.Errorf("status = %s, want dead_letter", got.Status)
	}
	if got.AttemptCount != marketplace.MaxReviewAttempts {
		t.Errorf("AttemptCount = %d, want %d", got.AttemptCount, marketplace.MaxReviewAttempts)
	}

	// dead_letter rows must not show up in ClaimSubmittedReviewVersions
	// (status filter is `submitted` only).
	reClaims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, 64)
	if err != nil {
		t.Fatalf("post-dead-letter claim: %v", err)
	}
	if findClaimOrNil(reClaims, ver) != nil {
		t.Fatalf("dead-letter row leaked into ClaimSubmittedReviewVersions")
	}
}

// TestMarketplaceB72_RescanResetsAttempts verifies the operator
// recovery contract: ResetReviewStateForRescan on a dead-letter
// row resets attempt_count to 0, clears last_attempt_error /
// last_attempt_at, and moves the row back to `submitted` so the
// next worker tick claims it fresh with the full MaxReviewAttempts
// budget. This is the documented recovery path for dead-lettered
// versions; the rescue can't carry forward the prior attempt
// count or a single transient blip would re-dead-letter the row
// immediately.
func TestMarketplaceB72_RescanResetsAttempts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	preClaimSubmittedRows(ctx, t, h.pool)
	ver := seedExtensionVersion(t, store, "b72_rescan")

	// Drive to dead_letter quickly: hand-stamp the row.
	if _, err := h.pool.Exec(ctx,
		`UPDATE marketplace_extension_review_state
		    SET status = 'dead_letter',
		        attempt_count = $1,
		        last_attempt_error = $2,
		        last_attempt_at = now(),
		        manual_review_notes = 'pipeline failed too many times',
		        reviewer = 'system',
		        reviewed_at = now(),
		        updated_at = now()
		  WHERE extension_version_id = $3`,
		marketplace.MaxReviewAttempts, "synthetic cause", ver,
	); err != nil {
		t.Fatalf("stamp dead_letter: %v", err)
	}

	if err := store.ResetReviewStateForRescan(ctx, ver); err != nil {
		t.Fatalf("ResetReviewStateForRescan on dead_letter: %v", err)
	}

	got, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState post-rescan: %v", err)
	}
	if got.Status != marketplace.ReviewStatusSubmitted {
		t.Errorf("rescan should restore status=submitted, got %s", got.Status)
	}
	if got.AttemptCount != 0 {
		t.Errorf("rescan should reset attempt_count to 0, got %d", got.AttemptCount)
	}
	if got.LastAttemptError != "" {
		t.Errorf("rescan should clear last_attempt_error, got %q", got.LastAttemptError)
	}
	if got.LastAttemptAt != nil {
		t.Errorf("rescan should clear last_attempt_at, got %v", got.LastAttemptAt)
	}

	// Worker can re-claim immediately.
	const workerID = "worker-b72-rescan"
	claims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, 64)
	if err != nil {
		t.Fatalf("post-rescan claim: %v", err)
	}
	if findClaimOrNil(claims, ver) == nil {
		t.Fatalf("rescanned row should be re-claimable, but ClaimSubmittedReviewVersions did not return it")
	}
}

// TestMarketplaceB72_ClaimGuardOnRecordAttemptFailure exercises the
// admin-rescan race: a rescan clears claimed_by/claimed_at between
// the worker's claim and the worker's RecordAttemptFailure. The
// claim guard MUST fire and return ErrClaimLost — incrementing
// the attempt counter on a freshly-reset row would corrupt the
// retry budget.
func TestMarketplaceB72_ClaimGuardOnRecordAttemptFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	preClaimSubmittedRows(ctx, t, h.pool)
	ver := seedExtensionVersion(t, store, "b72_guard")

	const workerID = "worker-b72-guard"
	claims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, 64)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	claim := findClaim(t, claims, ver)
	guard := &marketplace.ReviewClaimGuard{ClaimedBy: workerID, ClaimedAt: claim.ClaimedAt}

	// Admin rescan lands between claim and RecordAttemptFailure.
	if err := store.ResetReviewStateForRescan(ctx, ver); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	// The worker's late RecordAttemptFailure must abort.
	if _, err := store.Reviews().RecordAttemptFailure(ctx, ver, guard, "would have failed"); !errors.Is(err, marketplace.ErrClaimLost) {
		t.Errorf("RecordAttemptFailure after rescan should return ErrClaimLost, got %v", err)
	}

	// The row's attempt_count must remain at 0 — the rescan
	// reset it and the late failure recording did not.
	got, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState: %v", err)
	}
	if got.AttemptCount != 0 {
		t.Errorf("attempt_count = %d after rescued failure, want 0", got.AttemptCount)
	}
}

// TestMarketplaceB72_MultiReplicaClaimSafety simulates two worker
// replicas hitting the same row concurrently. The atomic
// UPDATE…RETURNING SKIP LOCKED + claim guard must give
// exactly-one-claimer semantics; the LATE replica's persist
// (post-rescan or post-other-claimant-success) must return
// ErrClaimLost so the row's state isn't double-written.
//
// This is the load-bearing safety guarantee that lets B7.2 drop
// the leader-singleton wrapper on the review worker.
func TestMarketplaceB72_MultiReplicaClaimSafety(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	preClaimSubmittedRows(ctx, t, h.pool)

	// Seed N versions; spin K worker goroutines that each claim
	// in a tight loop. Each version must be claimed by exactly
	// one worker.
	const (
		nVersions = 12
		nWorkers  = 4
	)
	versionsByID := make(map[uuid.UUID]bool, nVersions)
	for i := 0; i < nVersions; i++ {
		ver := seedExtensionVersion(t, store, fmt.Sprintf("b72_mr_%d", i))
		versionsByID[ver] = true
	}

	// Use a start barrier to make every worker race the first
	// claim call simultaneously; otherwise the first goroutine
	// to win the scheduler can drain the whole batch before its
	// peers wake up, and the test reduces to a single-worker
	// run. The barrier is the closest we can get to truly
	// concurrent contention from within a single-process test
	// without spawning real worker processes.
	//
	// claimLimit is intentionally capped to 2 so a single
	// worker can't drain the whole queue in one round even if
	// it wins the race: forces at least nVersions/2 = 6 claim
	// rounds and therefore real contention across workers.
	startBarrier := make(chan struct{})

	var mu sync.Mutex
	claimedBy := make(map[uuid.UUID]string, nVersions)

	var wg sync.WaitGroup
	for w := 0; w < nWorkers; w++ {
		wg.Add(1)
		workerID := fmt.Sprintf("worker-mr-%d", w)
		go func(workerID string) {
			defer wg.Done()
			<-startBarrier
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				const perTickLimit = 2
				claims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, perTickLimit)
				if err != nil {
					t.Errorf("[%s] claim: %v", workerID, err)
					return
				}
				gotOne := false
				for _, c := range claims {
					if !versionsByID[c.VersionID] {
						continue
					}
					gotOne = true
					mu.Lock()
					prior, dup := claimedBy[c.VersionID]
					claimedBy[c.VersionID] = workerID
					mu.Unlock()
					if dup {
						t.Errorf("version %s double-claimed: first=%s second=%s", c.VersionID, prior, workerID)
					}
					// Terminal-transition via the standard
					// UpdateReviewState so a subsequent claim
					// finds nothing.
					guard := &marketplace.ReviewClaimGuard{ClaimedBy: workerID, ClaimedAt: c.ClaimedAt}
					if _, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
						VersionID:     c.VersionID,
						Status:        marketplace.ReviewStatusAutomatedPassed,
						ExpectedClaim: guard,
					}); err != nil {
						t.Errorf("[%s] persist on %s: %v", workerID, c.VersionID, err)
					}
				}
				// Quick stop: if all seeded versions accounted for, exit.
				mu.Lock()
				done := len(claimedBy) == nVersions
				mu.Unlock()
				if done {
					return
				}
				if !gotOne {
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(workerID)
	}
	close(startBarrier)
	wg.Wait()

	if len(claimedBy) != nVersions {
		t.Fatalf("only %d / %d versions claimed", len(claimedBy), nVersions)
	}
	// The load-bearing assertion is the inline `if dup` check
	// inside the loop: a double-claim would have failed the
	// test there. We deliberately do NOT assert that work was
	// spread across workers — pgx connection scheduling +
	// kernel scheduling can put one goroutine permanently
	// ahead, and the multi-replica safety guarantee is "no
	// double claim" not "perfectly balanced load". A separate
	// test (TestMarketplaceB72_ClaimGuardOnRecordAttemptFailure)
	// pins the guard semantics that defeat the lease-expiry
	// race.
}

// TestMarketplaceB72_DeadLetterRaceWithRescan pins the
// dead-letter-vs-rescan race fix added after Devin Review on
// PR #134. The scenario:
//
//  1. Worker pipeline fails, RecordAttemptFailure increments
//     attempt_count to MaxReviewAttempts and clears the claim
//     columns (returns ErrReviewMaxAttemptsExceeded).
//  2. Admin clicks Rescan; ResetReviewStateForRescan resets
//     attempt_count to 0 (status stays submitted because
//     RecordAttemptFailure didn't transition).
//  3. Worker proceeds with the dead-letter UpdateReviewState
//     transition. ExpectedClaim is nil (step 1 cleared it), so
//     the claim guard cannot fire — without the MinAttemptCount
//     guard the UPDATE would clobber the rescued row back to
//     dead_letter and admin Rescan would be silently undone.
//
// With MinAttemptCount=MaxReviewAttempts on the dead-letter
// UpdateReviewState call, the UPDATE refuses (attempt_count=0
// after rescan) and surfaces as ErrClaimLost so the worker drops
// the transition. Row remains submitted with a fresh budget.
func TestMarketplaceB72_DeadLetterRaceWithRescan(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	preClaimSubmittedRows(ctx, t, h.pool)
	ver := seedExtensionVersion(t, store, "b72_deadletter_race")

	// Drive attempt_count to MaxReviewAttempts and clear the
	// claim, mirroring the state RecordAttemptFailure leaves
	// behind on the Nth failure.
	if _, err := h.pool.Exec(ctx,
		`UPDATE marketplace_extension_review_state
		    SET attempt_count = $1,
		        last_attempt_error = 'synthetic max',
		        last_attempt_at = now(),
		        claimed_by = NULL,
		        claimed_at = NULL,
		        updated_at = now()
		  WHERE extension_version_id = $2`,
		marketplace.MaxReviewAttempts, ver,
	); err != nil {
		t.Fatalf("stamp pre-deadletter: %v", err)
	}

	// Admin Rescan lands now: attempt_count back to 0.
	if err := store.ResetReviewStateForRescan(ctx, ver); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	// Worker's late dead-letter transition MUST refuse.
	_, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID:       ver,
		Status:          marketplace.ReviewStatusDeadLetter,
		AutomatedChecks: []byte(`{"status":"dead_letter"}`),
		ManualNotes:     "pipeline failed N times",
		Reviewer:        "system",
		ExpectedClaim:   nil,
		MinAttemptCount: marketplace.MaxReviewAttempts,
	})
	if !errors.Is(err, marketplace.ErrClaimLost) {
		t.Fatalf("dead-letter after rescan should return ErrClaimLost, got %v", err)
	}

	// Row must still be submitted with attempt_count=0 (the
	// rescan's reset), NOT dead_letter.
	got, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState: %v", err)
	}
	if got.Status != marketplace.ReviewStatusSubmitted {
		t.Errorf("status = %s, want %s (dead-letter must have been refused)", got.Status, marketplace.ReviewStatusSubmitted)
	}
	if got.AttemptCount != 0 {
		t.Errorf("attempt_count = %d, want 0 (rescan reset must have survived)", got.AttemptCount)
	}
}

// TestMarketplaceB72_DeadLetterStillWorksWithGuard sanity-checks
// that the MinAttemptCount guard does NOT break the happy path:
// a normal dead-letter transition (attempt_count == MaxAttempts,
// no concurrent rescan) still succeeds.
func TestMarketplaceB72_DeadLetterStillWorksWithGuard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	preClaimSubmittedRows(ctx, t, h.pool)
	ver := seedExtensionVersion(t, store, "b72_deadletter_happy")

	if _, err := h.pool.Exec(ctx,
		`UPDATE marketplace_extension_review_state
		    SET attempt_count = $1,
		        last_attempt_error = 'synthetic',
		        last_attempt_at = now(),
		        claimed_by = NULL,
		        claimed_at = NULL,
		        updated_at = now()
		  WHERE extension_version_id = $2`,
		marketplace.MaxReviewAttempts, ver,
	); err != nil {
		t.Fatalf("stamp pre-deadletter: %v", err)
	}

	out, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID:       ver,
		Status:          marketplace.ReviewStatusDeadLetter,
		AutomatedChecks: []byte(`{"status":"dead_letter"}`),
		ManualNotes:     "pipeline failed N times",
		Reviewer:        "system",
		ExpectedClaim:   nil,
		MinAttemptCount: marketplace.MaxReviewAttempts,
	})
	if err != nil {
		t.Fatalf("dead-letter happy path: %v", err)
	}
	if out.Status != marketplace.ReviewStatusDeadLetter {
		t.Errorf("status = %s, want dead_letter", out.Status)
	}
}

// preClaimSubmittedRows is the same hack used by the existing
// B7 tests: stamp every existing `submitted` row with a fresh
// claimed_at so our newly-seeded rows are the only NULL-claim
// candidates in the shared test DB.
func preClaimSubmittedRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE marketplace_extension_review_state
		    SET claimed_at = now(),
		        claimed_by = 'test-pre-claim-b72'
		  WHERE status = 'submitted'`,
	); err != nil {
		t.Fatalf("pre-claim submitted rows: %v", err)
	}
}

func findClaim(t *testing.T, claims []marketplace.ClaimedReviewVersion, ver uuid.UUID) marketplace.ClaimedReviewVersion {
	t.Helper()
	c := findClaimOrNil(claims, ver)
	if c == nil {
		t.Fatalf("expected claim for version %s, got %d claims none matching", ver, len(claims))
	}
	return *c
}

func findClaimOrNil(claims []marketplace.ClaimedReviewVersion, ver uuid.UUID) *marketplace.ClaimedReviewVersion {
	for i := range claims {
		if claims[i].VersionID == ver {
			return &claims[i]
		}
	}
	return nil
}
