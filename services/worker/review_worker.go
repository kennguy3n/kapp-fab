package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/review"
)

// ReviewWorker drains the marketplace_extension_review_state queue:
// every tick it claims up to claimLimit versions whose status is
// `submitted` (SKIP LOCKED + ORDER BY created_at ASC for FIFO),
// runs the automated-check pipeline against each, and writes the
// resulting findings + state transition.
//
// Singleton posture: invoked from the worker's leader callback
// (services/worker/main.go's leadWorker) so only the elected
// leader is running this loop. Other replicas idle on the same
// version queue — if the leader dies, the next election winner
// picks up where it left off; SKIP LOCKED + idempotent rescan
// transitions mean a re-run from scratch produces the same
// finding set.
//
// Failure handling: a per-version pipeline failure logs + leaves
// the row as `submitted` so the next poll re-claims it (errors are
// transient: CDN fetch failed, manifest parser blip, etc.). A
// claim failure logs + sleeps one tick before re-claiming — no
// fast-spin if the DB itself is down.
type ReviewWorker struct {
	store    *marketplace.Store
	pipeline *review.Pipeline
	logger   *slog.Logger

	interval   time.Duration
	claimLimit int
	workerID   string
}

// NewReviewWorker wires a worker. claimLimit caps the per-tick
// batch size; 4 keeps tail latency low while still amortising the
// DB round-trip cost across multiple versions.
//
// workerID is recorded on each claimed row's claimed_by column for
// forensic debugging (which replica was running the pipeline when
// a row went stale). It does NOT participate in any locking
// decision — the atomic SKIP LOCKED claim + claimed_at lease
// (see Store.ClaimSubmittedReviewVersions) enforces exactly-one-
// claimer semantics. An empty workerID falls back to a sentinel.
//
// The pipeline argument MUST already have Source/Policy/Findings/
// State sinks wired against the same Store this worker owns; the
// worker only adds the polling + state-transition machinery on top
// of review.Pipeline.Run + Persist.
func NewReviewWorker(store *marketplace.Store, pipeline *review.Pipeline, logger *slog.Logger, interval time.Duration, claimLimit int, workerID string) *ReviewWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if claimLimit <= 0 {
		claimLimit = 4
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ReviewWorker{
		store:      store,
		pipeline:   pipeline,
		logger:     logger,
		interval:   interval,
		claimLimit: claimLimit,
		workerID:   workerID,
	}
}

// Run blocks until ctx is cancelled, draining the review queue on
// every tick. Errors during a single version are logged and the
// row is left in `submitted` for the next poll; they never abort
// the loop.
func (w *ReviewWorker) Run(ctx context.Context) {
	w.logger.Info("review-worker: started",
		slog.String("interval", w.interval.String()),
		slog.Int("claim_limit", w.claimLimit),
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

func (w *ReviewWorker) drain(ctx context.Context) {
	ids, err := w.store.ClaimSubmittedReviewVersions(ctx, w.workerID, w.claimLimit)
	if err != nil {
		w.logger.Warn("review-worker: claim failed", slog.String("err", err.Error()))
		return
	}
	for _, id := range ids {
		// Per-version timeout: the pipeline pulls the bundle from
		// the CDN + runs static analysis; both are bounded by the
		// per-source timeouts but defence in depth caps total wall
		// time at 90s so a stuck DNS lookup doesn't pin a queue
		// slot forever.
		runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		w.processOne(runCtx, id)
		cancel()
	}
}

// processOne runs the pipeline for a single claimed version and
// persists the result. Errors are logged with the version id so
// the operator can correlate.
func (w *ReviewWorker) processOne(ctx context.Context, versionID uuid.UUID) {
	version, err := w.store.GetVersion(ctx, versionID)
	if err != nil {
		w.logger.Warn("review-worker: get version failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()))
		return
	}
	res, err := w.pipeline.Run(ctx, version)
	if err != nil {
		w.logger.Warn("review-worker: pipeline run failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()))
		return
	}
	if err := w.pipeline.Persist(ctx, res); err != nil {
		// A persist failure means the row is still in
		// `submitted` so the next poll re-claims it. The same
		// findings are written deterministically on re-run
		// (UpsertReviewFindings is idempotent on the natural
		// key), so we won't accumulate duplicate rows.
		if errors.Is(err, marketplace.ErrNotFound) {
			// The version row was deleted between claim and
			// persist — drop the result, don't retry.
			w.logger.Info("review-worker: version disappeared during pipeline",
				slog.String("version_id", versionID.String()))
			return
		}
		w.logger.Warn("review-worker: persist failed",
			slog.String("version_id", versionID.String()),
			slog.String("err", err.Error()))
		return
	}
	w.logger.Info("review-worker: completed",
		slog.String("version_id", versionID.String()),
		slog.String("status", string(res.Status)),
		slog.Int("findings", len(res.Findings)),
		slog.String("worst_severity", string(res.WorstSeverity)),
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
