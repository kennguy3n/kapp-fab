package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/review"
)

// ReviewWorker drains the marketplace_extension_review_state queue:
// every tick it claims up to claimLimit versions whose status is
// `submitted` (SKIP LOCKED + ORDER BY created_at ASC for FIFO),
// runs the automated-check pipeline against each in parallel up to
// `concurrency` simultaneous runs, and writes the resulting findings
// + state transition.
//
// Multi-replica posture (B7.2): no longer leader-singleton. Every
// worker replica runs this loop. The atomic UPDATE…RETURNING
// SKIP LOCKED in Store.ClaimSubmittedReviewVersions ensures each
// version is claimed by exactly one replica, and the
// (claimed_by, claimed_at) guard on UpdateReviewState atomically
// aborts a late-persist if another replica re-claimed after lease
// expiry. Spreading review across replicas scales throughput
// linearly without requiring a coordinator process.
//
// Per-version parallelism (B7.2): inside a single tick, claimed
// versions are processed by goroutines bounded by a weighted
// semaphore (`concurrency`). The pipeline is CDN-bound (bundle
// fetch dominates wall time); sequential drain at claimLimit=4
// would worst-case 4× the slowest fetch per tick, while parallel
// drain caps tick latency at the slowest single fetch.
//
// Failure handling (B7.2): a per-version pipeline failure
// increments attempt_count on the row via
// ReviewStateStore.RecordAttemptFailure, clears the claim so the
// next poll re-claims immediately (no waiting for the 10-min
// lease), and leaves the row in `submitted`. Once attempt_count
// reaches marketplace.MaxReviewAttempts, the worker transitions
// the row to `dead_letter` with a synthetic finding row recording
// the final failure. ErrClaimLost (admin rescan race) is logged at
// info and does NOT count as a failed attempt — the rescan
// already reset the counter.
type ReviewWorker struct {
	store    *marketplace.Store
	pipeline *review.Pipeline
	logger   *slog.Logger

	interval    time.Duration
	claimLimit  int
	concurrency int
	workerID    string

	// sem bounds the number of goroutines running pipeline.Run +
	// Persist concurrently inside a single drain tick. Sized to
	// `concurrency` at construction. Acquire is ctx-aware so a
	// shutdown unblocks queued goroutines instead of hanging.
	sem *semaphore.Weighted
}

// NewReviewWorker wires a worker.
//
// claimLimit caps the per-tick batch size; 4 keeps tail latency
// low while still amortising the DB round-trip cost across
// multiple versions.
//
// concurrency caps how many of those claimed versions can run
// pipeline.Run + Persist in parallel inside a tick. Default 4
// (matches claimLimit). A bound is needed because each in-flight
// pipeline holds the bundle in memory + a CDN connection + a
// pgxpool slot for the eventual UpdateReviewState write; an
// unbounded fan-out at claimLimit=64 would spawn 64 of each.
//
// workerID is recorded on each claimed row's claimed_by column for
// forensic debugging (which replica was running the pipeline when
// a row went stale) and participates in the claim guard on
// UpdateReviewState / RecordAttemptFailure. An empty workerID
// falls back to a sentinel.
//
// The pipeline argument MUST already have Source/Policy/Findings/
// State sinks wired against the same Store this worker owns; the
// worker only adds the polling + state-transition machinery on top
// of review.Pipeline.Run + Persist.
func NewReviewWorker(
	store *marketplace.Store,
	pipeline *review.Pipeline,
	logger *slog.Logger,
	interval time.Duration,
	claimLimit int,
	concurrency int,
	workerID string,
) *ReviewWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if claimLimit <= 0 {
		claimLimit = 4
	}
	if concurrency <= 0 {
		concurrency = claimLimit
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ReviewWorker{
		store:       store,
		pipeline:    pipeline,
		logger:      logger,
		interval:    interval,
		claimLimit:  claimLimit,
		concurrency: concurrency,
		workerID:    workerID,
		sem:         semaphore.NewWeighted(int64(concurrency)),
	}
}

// Run blocks until ctx is cancelled, draining the review queue on
// every tick. Errors during a single version are logged and the
// row's attempt_count is bumped (or dead-lettered on the Nth
// failure); they never abort the loop.
func (w *ReviewWorker) Run(ctx context.Context) {
	w.logger.Info("review-worker: started",
		slog.String("interval", w.interval.String()),
		slog.Int("claim_limit", w.claimLimit),
		slog.Int("concurrency", w.concurrency),
		slog.String("worker_id", w.workerID),
	)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("review-worker: shutdown")
			return
		case <-t.C:
			w.drain(ctx)
		}
	}
}

// drain claims a batch of submitted versions and fans them out to
// per-version goroutines bounded by the semaphore. Joins all
// goroutines before returning so a long-running tick doesn't
// compound onto the next ticker fire (which would let drain ticks
// stack up if the pipeline is slower than the interval).
func (w *ReviewWorker) drain(ctx context.Context) {
	claims, err := w.store.ClaimSubmittedReviewVersions(ctx, w.workerID, w.claimLimit)
	if err != nil {
		w.logger.Warn("review-worker: claim failed", slog.String("err", err.Error()))
		return
	}
	if len(claims) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, claim := range claims {
		// Semaphore.Acquire is ctx-aware so a shutdown unblocks
		// queued goroutines. If Acquire returns an error the
		// only cause is ctx cancellation; we leave the
		// remaining claims in place — their rows stay claimed
		// until the 10-min lease lapses, after which any
		// replica re-claims them.
		if err := w.sem.Acquire(ctx, 1); err != nil {
			w.logger.Info("review-worker: drain interrupted before all claims processed",
				slog.String("err", err.Error()),
				slog.Int("remaining", len(claims)),
			)
			break
		}
		wg.Add(1)
		go func(c marketplace.ClaimedReviewVersion) {
			defer wg.Done()
			defer w.sem.Release(1)
			// Per-version timeout: the pipeline pulls the
			// bundle from the CDN + runs static analysis;
			// both are bounded by the per-source timeouts but
			// defence in depth caps total wall time at 90s
			// so a stuck DNS lookup doesn't pin a queue slot
			// (or a semaphore slot) forever.
			runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			w.processOne(runCtx, c)
		}(claim)
	}
	wg.Wait()
}

// processOne runs the pipeline for a single claimed version and
// persists the result. On any pipeline failure (other than
// ErrClaimLost / ErrNotFound) the row's attempt_count is bumped
// and, on the Nth consecutive failure, the row is transitioned to
// dead_letter with a synthetic finding row.
//
// The claim's (workerID, claimedAt) tuple is threaded into the
// pipeline Result AND into RecordAttemptFailure so both the
// state UPDATE and the failure increment atomically abort if an
// admin Rescan cleared the columns mid-flight.
func (w *ReviewWorker) processOne(ctx context.Context, claim marketplace.ClaimedReviewVersion) {
	versionID := claim.VersionID
	expectedClaim := &marketplace.ReviewClaimGuard{
		ClaimedBy: w.workerID,
		ClaimedAt: claim.ClaimedAt,
	}
	version, err := w.store.GetVersion(ctx, versionID)
	if err != nil {
		// GetVersion failure is unusual (the claim succeeded so
		// the version row existed milliseconds ago). Surface as
		// an attempt failure so a persistently-broken row
		// dead-letters rather than spinning.
		w.logger.Warn("review-worker: get version failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()))
		w.recordFailureOrDeadLetter(ctx, versionID, expectedClaim, fmt.Errorf("get version: %w", err))
		return
	}
	res, err := w.pipeline.Run(ctx, version)
	if err != nil {
		// pipeline.Run errors are policy / bundle-load level
		// failures that pipeline.Run itself didn't convert to a
		// finding (LoadPolicy errors, ctx cancellation, etc.).
		// Bundle-load failures DO get converted to a synthetic
		// finding + rejected status by pipeline.Run; this branch
		// is for the unrecoverable upstream cases.
		w.logger.Warn("review-worker: pipeline run failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()))
		w.recordFailureOrDeadLetter(ctx, versionID, expectedClaim, fmt.Errorf("pipeline run: %w", err))
		return
	}
	// Attach claim metadata so Pipeline.Persist's state UPDATE
	// includes the atomic claim guard.
	res.Claim = expectedClaim
	if err := w.pipeline.Persist(ctx, res); err != nil {
		if errors.Is(err, marketplace.ErrNotFound) {
			w.logger.Info("review-worker: version disappeared during pipeline",
				slog.String("version_id", versionID.String()))
			return
		}
		if errors.Is(err, marketplace.ErrClaimLost) {
			w.logger.Info("review-worker: claim lost to concurrent rescan, dropping result",
				slog.String("version_id", versionID.String()),
				slog.String("claimed_at", claim.ClaimedAt.Format(time.RFC3339Nano)),
			)
			return
		}
		w.logger.Warn("review-worker: persist failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()))
		w.recordFailureOrDeadLetter(ctx, versionID, expectedClaim, fmt.Errorf("pipeline persist: %w", err))
		return
	}
	w.logger.Info("review-worker: completed",
		slog.String("version_id", versionID.String()),
		slog.String("status", string(res.Status)),
		slog.Int("findings", len(res.Findings)),
		slog.String("worst_severity", string(res.WorstSeverity)),
	)
}

// recordFailureOrDeadLetter bumps the row's attempt_count and, if
// the post-increment count hits MaxReviewAttempts, transitions
// the row to dead_letter with a synthetic finding row recording
// the final failure mode.
//
// The deadletter transition deliberately goes through the
// standard UpdateReviewState path (not a backdoor UPDATE) so the
// transition-graph guard, the claim guard, and the
// UpsertReviewFindings idempotency all apply normally.
func (w *ReviewWorker) recordFailureOrDeadLetter(
	ctx context.Context,
	versionID uuid.UUID,
	expectedClaim *marketplace.ReviewClaimGuard,
	cause error,
) {
	// Derive the bookkeeping context from the parent worker
	// context (not context.Background) so a clean shutdown still
	// cancels bookkeeping promptly. We DO drop the per-version
	// 90s timeout that the caller's ctx carries so a near-budget
	// pipeline error still has a small fresh window to record
	// the failure (without this, a pipeline failure that ran out
	// the clock would never increment attempt_count and the row
	// would spin forever).
	//
	// context.WithoutCancel pins the parent's values (loggers,
	// trace IDs) but drops the parent's deadline; the explicit
	// 10s budget then bounds the bookkeeping write so a
	// truly-dead DB doesn't hang shutdown either.
	bookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	errMsg := cause.Error()
	newCount, err := w.store.Reviews().RecordAttemptFailure(bookCtx, versionID, expectedClaim, errMsg)
	if errors.Is(err, marketplace.ErrClaimLost) {
		w.logger.Info("review-worker: claim lost during failure recording, dropping attempt",
			slog.String("version_id", versionID.String()),
		)
		return
	}
	if errors.Is(err, marketplace.ErrNotFound) {
		w.logger.Info("review-worker: version row disappeared before failure could be recorded",
			slog.String("version_id", versionID.String()),
		)
		return
	}
	if err != nil && !errors.Is(err, marketplace.ErrReviewMaxAttemptsExceeded) {
		w.logger.Warn("review-worker: record attempt failure failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()),
		)
		return
	}
	w.logger.Info("review-worker: attempt failed",
		slog.String("version_id", versionID.String()),
		slog.Int("attempt_count", newCount),
		slog.Int("max_attempts", marketplace.MaxReviewAttempts),
		slog.String("cause", errMsg),
	)
	if !errors.Is(err, marketplace.ErrReviewMaxAttemptsExceeded) {
		return
	}
	// Dead-letter the row. Synthetic finding records the final
	// failure so the admin queue surfaces a one-row explanation
	// of why the version was abandoned (in addition to the
	// last_attempt_error column). UpsertReviewFindings is
	// idempotent on the natural key (version_id, check_name,
	// code, location), so re-running this branch on a poll
	// after the dead-letter transition would write the same
	// row (the dead-letter transition guards prevent that
	// re-run, but defence in depth).
	now := time.Now().UTC()
	finding := marketplace.ReviewFinding{
		ExtensionVersionID: versionID,
		CheckName:          "review.dead_letter",
		Code:               "review.max_attempts_exceeded",
		Severity:           marketplace.SeverityError,
		Message: fmt.Sprintf(
			"review pipeline failed %d consecutive attempts; final error: %s",
			marketplace.MaxReviewAttempts, errMsg,
		),
		CreatedAt: now,
	}
	if err := w.store.Findings().UpsertReviewFindings(bookCtx, versionID, []marketplace.ReviewFinding{finding}); err != nil {
		w.logger.Warn("review-worker: persist dead-letter finding failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()),
		)
		// Continue with the state transition anyway — the
		// state flag is the load-bearing signal; the finding
		// row is the human-readable explanation.
	}
	// Re-encode automated_checks JSON with the dead-letter
	// summary so the publisher dashboard's "checks ran" panel
	// shows the abandonment.
	checksJSON := []byte(fmt.Sprintf(
		`{"ran_at":%q,"status":"dead_letter","worst":"error","checks":[{"name":"review.dead_letter","passed":false,"worst":"error","errors":1,"warns":0,"infos":0}]}`,
		now.Format(time.RFC3339Nano),
	))
	// Note: RecordAttemptFailure cleared claimed_by/claimed_at,
	// so the ExpectedClaim guard would fail here. Pass nil so
	// the UPDATE proceeds unguarded — the row has already been
	// mutated (attempt_count++, claim cleared) inside
	// RecordAttemptFailure's same tx, so the dead-letter
	// transition is correctly attributing to this worker.
	if _, err := w.store.Reviews().UpdateReviewState(bookCtx, marketplace.UpdateReviewStateInput{
		VersionID:       versionID,
		Status:          marketplace.ReviewStatusDeadLetter,
		AutomatedChecks: checksJSON,
		ManualNotes: fmt.Sprintf(
			"pipeline failed %d times; last error: %s",
			marketplace.MaxReviewAttempts, errMsg,
		),
		Reviewer:      "system",
		ExpectedClaim: nil,
	}); err != nil {
		w.logger.Warn("review-worker: dead-letter transition failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()),
		)
		return
	}
	w.logger.Warn("review-worker: dead-lettered version after max attempts",
		slog.String("version_id", versionID.String()),
		slog.Int("attempt_count", newCount),
	)
}

// buildReviewPipeline constructs the production review.Pipeline,
// wiring the store + bundle source + sinks. Lives here (next to
// the worker) so main.go can keep wiring concise.
func buildReviewPipeline(store *marketplace.Store, sourceTimeout time.Duration) *review.Pipeline {
	// HTTPSource enforces HTTPS-only bundle URLs + the per-fetch
	// timeout + the 10 MiB size cap on the streaming read. Tests
	// can substitute the in-memory source.
	src := review.NewHTTPSource()
	if sourceTimeout > 0 {
		src.Timeout = sourceTimeout
	}
	policy := review.PolicyLoaderFunc(func(ctx context.Context, version *marketplace.ExtensionVersion) (*review.PolicyContext, error) {
		pub, keys, err := store.ResolvePublisherKeysForVersion(ctx, version.ID)
		if err != nil {
			return nil, fmt.Errorf("review policy: %w", err)
		}
		return &review.PolicyContext{Publisher: pub, NonRevokedKeys: keys}, nil
	})
	return &review.Pipeline{
		Source:   src,
		Policy:   policy,
		Findings: store.Findings(),
		State:    store.Reviews(),
		Checks:   review.DefaultChecks(),
	}
}
