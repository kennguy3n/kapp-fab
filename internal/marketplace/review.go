package marketplace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReviewStateStore owns reads/writes against
// marketplace_extension_review_state. Split from Store so B7's
// review pipeline can mount only the surface it needs without
// importing the install/version write methods.
type ReviewStateStore struct {
	store *Store
}

// Reviews returns a ReviewStateStore bound to the same pool. Re-uses
// the parent Store's pool so callers don't have to thread two pool
// instances through the wiring.
func (s *Store) Reviews() *ReviewStateStore {
	return &ReviewStateStore{store: s}
}

// GetReviewState returns the review state for a version. Returns
// ErrNotFound if no row exists — every PublishVersion seeds a row,
// so a missing row indicates either the version was deleted (which
// CASCADE prevents) or the caller passed a wrong id.
func (rs *ReviewStateStore) GetReviewState(ctx context.Context, versionID uuid.UUID) (*ReviewState, error) {
	if versionID == uuid.Nil {
		return nil, fmt.Errorf("%w: version id required", ErrNotFound)
	}
	var out ReviewState
	err := rs.store.pool.QueryRow(ctx,
		`SELECT extension_version_id, status, automated_checks::text,
		        COALESCE(manual_review_notes,''), COALESCE(reviewer,''),
		        reviewed_at, attempt_count, last_attempt_error, last_attempt_at,
		        created_at, updated_at
		 FROM marketplace_extension_review_state
		 WHERE extension_version_id = $1`, versionID,
	).Scan(
		&out.ExtensionVersionID, &out.Status,
		scanJSONB(&out.AutomatedChecks),
		&out.ManualReviewNotes, &out.Reviewer,
		&out.ReviewedAt, &out.AttemptCount, &out.LastAttemptError, &out.LastAttemptAt,
		&out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get review state: %w", err)
	}
	return &out, nil
}

// UpdateReviewStateInput is the parameter block for
// UpdateReviewState. Status MUST be a valid ReviewStatus. When
// transitioning to approved or rejected, Reviewer MUST be non-empty
// (the DB CHECK also enforces this — the early check here gives a
// clearer error than a constraint violation).
//
// ExpectedClaim is the optional review-worker claim guard used by
// B7's pipeline to defeat the TOCTOU race between an admin Rescan
// (which clears claimed_by / claimed_at) and an in-flight Persist.
// When set, the SQL UPDATE additionally gates on claimed_by AND
// claimed_at matching the worker's recorded values; if the row's
// claim was cleared in the gap, the UPDATE affects zero rows and
// UpdateReviewState returns ErrClaimLost so the worker can drop
// its result and let the next poll re-run the pipeline against
// the freshly-reset row. Human-driven transitions (admin review,
// publisher withdraw) leave ExpectedClaim nil and bypass the
// guard.
type UpdateReviewStateInput struct {
	VersionID       uuid.UUID
	Status          ReviewStatus
	AutomatedChecks []byte
	ManualNotes     string
	Reviewer        string
	ReviewedAt      *time.Time
	ExpectedClaim   *ReviewClaimGuard
	// MinAttemptCount, when > 0, adds an attempt_count >= N
	// predicate to the UPDATE. Used by the worker's dead-letter
	// transition to refuse the UPDATE if a concurrent admin
	// Rescan reset attempt_count to 0 between RecordAttemptFailure
	// and this UPDATE — without this guard the dead-letter UPDATE
	// could clobber a freshly-rescanned row (the claim guard
	// cannot help here because RecordAttemptFailure already
	// cleared the claim columns, so the dead-letter call passes
	// ExpectedClaim=nil and the claim guard never fires). Human-
	// driven transitions leave this 0 and the guard is skipped.
	MinAttemptCount int
}

// ReviewClaimGuard is the (claimed_by, claimed_at) tuple recorded
// on a marketplace_extension_review_state row when a worker claims
// it via ClaimSubmittedReviewVersions. The pipeline carries this
// tuple through Pipeline.Run → Pipeline.Persist and threads it
// into the UpdateReviewState SQL so an admin Rescan that lands
// between claim and persist (which clears these columns) reliably
// aborts the worker's late transition. ClaimedAt MUST be the
// exact timestamp the DB stamped on the row at claim time
// (timestamptz round-trips losslessly through pgx); a re-read of
// the row is not acceptable because the read itself races against
// the rescan.
type ReviewClaimGuard struct {
	ClaimedBy string
	ClaimedAt time.Time
}

// UpdateReviewState transitions a review row. Enforces the same
// transition graph the B7 pipeline assumes:
//
//	submitted        → automated_passed | manual_review | rejected | withdrawn | dead_letter
//	automated_passed → manual_review    | rejected | withdrawn
//	manual_review    → approved | rejected | withdrawn
//	approved         → (terminal; only the AutomatedChecks JSON may
//	                    be amended for forensic detail)
//	rejected         → (terminal; same caveat)
//	withdrawn        → (terminal)
//	dead_letter      → (terminal; recoverable via ResetReviewStateForRescan)
//
// dead_letter is the B7.2 system-driven terminal for "the worker
// tried MaxReviewAttempts times and the pipeline never produced a
// verdict". Reviewer-free transition (the system itself decided);
// admin Rescan moves the row back to `submitted` with a fresh
// attempt budget.
//
// Note: the worker emits BOTH automated_passed (no findings or
// info-only findings — clear pass) and manual_review (one or more
// warn-level findings) directly out of submitted in a single state
// transition. There is no intermediate "automated complete, awaiting
// human" state between submitted and manual_review — automated_passed
// is reserved for the case where the automated pipeline alone
// produces a verdict the system trusts.
//
// A transition to a terminal state stamps ReviewedAt = now() if not
// provided. Returns ErrNotFound if the version has no review state.
//
// Concurrency posture: the UPDATE asserts the status the transition
// graph was checked against (`AND status = $expected`). If a
// concurrent caller flipped the row in the gap between GetReviewState
// and the Exec, RowsAffected==0 and the retry loop re-reads the
// latest row to decide whether the new state is consistent with the
// requested target (idempotent re-issue of a converging transition)
// or a true conflict to surface. Without the status guard, a
// `submitted→rejected` (by a human reviewer) and `submitted→
// automated_passed` (by the B7 worker) race could both UPDATE
// successfully and the last writer would silently overwrite the
// other's decision — a rejected version could be reopened as
// automated_passed. Same posture as UpdateExtensionStatus.
func (rs *ReviewStateStore) UpdateReviewState(ctx context.Context, in UpdateReviewStateInput) (*ReviewState, error) {
	if in.VersionID == uuid.Nil {
		return nil, fmt.Errorf("%w: version id required", ErrNotFound)
	}
	if !in.Status.Valid() {
		return nil, fmt.Errorf("%w: unknown review status %q", ErrInvalidManifest, in.Status)
	}
	// Approved / rejected require a reviewer; the DB CHECK enforces
	// this, but the early check here surfaces the actionable error
	// to the caller without spending a round-trip on the read.
	needReviewer := in.Status == ReviewStatusApproved || in.Status == ReviewStatusRejected
	if needReviewer && strings.TrimSpace(in.Reviewer) == "" {
		return nil, fmt.Errorf("%w: reviewer required when status=%s", ErrInvalidManifest, in.Status)
	}

	current, err := rs.GetReviewState(ctx, in.VersionID)
	if err != nil {
		return nil, err
	}

	// Terminal-state self-loops (approved→approved, rejected→rejected,
	// withdrawn→withdrawn) are special: the reviewer and reviewed_at
	// columns are the audit trail of WHO decided this version and
	// WHEN. Silently overwriting them on a second call would erase
	// that audit detail — e.g. UpdateReviewState(approved, "bob") on
	// a row already approved by "alice" would lose Alice's reviewer
	// record. The transition graph allows the self-loop (so an
	// at-least-once retry from the same caller succeeds) but here we
	// gate the UPDATE explicitly:
	//
	//   - Same reviewer (or empty in.Reviewer): treat as a no-op
	//     idempotent re-issue. Return the existing row unchanged
	//     WITHOUT firing the UPDATE — automated_checks and notes
	//     are not refreshed because a terminal row is, by spec,
	//     frozen post-decision (re-running scans post-approval is
	//     not a supported workflow; publishers re-submit by uploading
	//     a new version).
	//
	//   - Different non-empty in.Reviewer: reject loudly. This is
	//     either a UI bug (two reviewers racing) or a deliberate
	//     audit-trail-overwrite attempt; either way it must not
	//     silently succeed.
	//
	// Non-terminal self-loops (automated_passed→automated_passed
	// etc.) are NOT gated here — the B7 worker re-issues those on
	// retry and the UPDATE intentionally refreshes automated_checks
	// with the new scan result.
	if current.Status.IsTerminal() && current.Status == in.Status {
		newReviewer := strings.TrimSpace(in.Reviewer)
		if newReviewer != "" && newReviewer != current.Reviewer {
			return nil, fmt.Errorf("%w: cannot change reviewer on terminal review row (current=%q, attempted=%q) — audit trail is write-once",
				ErrInvalidManifest, current.Reviewer, newReviewer)
		}
		// Idempotent re-issue: same reviewer or empty override.
		// Return the existing row unchanged.
		return current, nil
	}

	// Optimistic-concurrency retry loop. We accept up to 3 contended
	// attempts before giving up — the same budget UpdateExtensionStatus
	// uses. Beyond that, something is wrong (e.g. a thundering herd of
	// reviewers all hammering the same version) and surfacing the
	// failure is better than silently looping.
	//
	// The re-read at the bottom of each iteration costs a round-trip;
	// if unconditional we would always pay one wasted re-read on the
	// final iteration even though there is no further UPDATE to gate
	// on the fresh state. Skip the re-read on the last attempt and
	// fall through to the contention error.
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if !reviewStatusTransitionAllowed(current.Status, in.Status) {
			return nil, fmt.Errorf("%w: cannot transition review status from %q to %q",
				ErrInvalidManifest, current.Status, in.Status)
		}
		reviewedAt := in.ReviewedAt
		if needReviewer && reviewedAt == nil {
			now := time.Now().UTC()
			reviewedAt = &now
		}
		// Coalesce default values against `current` — when the caller
		// omits a field (zero-value AutomatedChecks / ManualNotes /
		// Reviewer), the existing row's value is preserved. Re-resolved
		// on every retry because a concurrent update may have written
		// fresh values that we should carry forward instead of clobber
		// with whatever `current` held at attempt 0.
		checks := in.AutomatedChecks
		if len(checks) == 0 {
			checks = current.AutomatedChecks
		}
		if len(checks) == 0 {
			checks = []byte("{}")
		}
		notes := in.ManualNotes
		if notes == "" {
			notes = current.ManualReviewNotes
		}
		reviewer := in.Reviewer
		if reviewer == "" {
			reviewer = current.Reviewer
		}

		// Claim guard parameters. NULL when the caller (admin /
		// human reviewer) does not pass an ExpectedClaim — the
		// SQL OR-fallback (`$N IS NULL OR ... = $N`) lets the
		// UPDATE proceed unchanged. When set, the UPDATE
		// additionally gates on BOTH columns matching exactly,
		// so a concurrent ResetReviewStateForRescan (which clears
		// them to NULL) atomically aborts this UPDATE without
		// race-prone read-then-write logic in Go.
		var expectedClaimBy, expectedClaimAt any
		if in.ExpectedClaim != nil {
			expectedClaimBy = in.ExpectedClaim.ClaimedBy
			expectedClaimAt = in.ExpectedClaim.ClaimedAt
		}

		var out ReviewState
		err = rs.store.pool.QueryRow(ctx,
			`UPDATE marketplace_extension_review_state
			   SET status = $2,
			       automated_checks = $3::jsonb,
			       manual_review_notes = NULLIF($4,''),
			       reviewer = NULLIF($5,''),
			       reviewed_at = $6,
			       updated_at = now()
			 WHERE extension_version_id = $1
			   AND status = $7
			   AND ($8::text IS NULL OR claimed_by = $8)
			   AND ($9::timestamptz IS NULL OR claimed_at = $9)
			   AND ($10::int = 0 OR attempt_count >= $10)
			 RETURNING extension_version_id, status, automated_checks::text,
			           COALESCE(manual_review_notes,''), COALESCE(reviewer,''),
			           reviewed_at, attempt_count, last_attempt_error, last_attempt_at,
			           created_at, updated_at`,
			in.VersionID, string(in.Status), string(checks), notes, reviewer, reviewedAt, string(current.Status),
			expectedClaimBy, expectedClaimAt, in.MinAttemptCount,
		).Scan(
			&out.ExtensionVersionID, &out.Status,
			scanJSONB(&out.AutomatedChecks),
			&out.ManualReviewNotes, &out.Reviewer,
			&out.ReviewedAt, &out.AttemptCount, &out.LastAttemptError, &out.LastAttemptAt,
			&out.CreatedAt, &out.UpdatedAt,
		)
		if err == nil {
			return &out, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("marketplace: update review state: %w", err)
		}
		// ErrNoRows from the RETURNING means one of:
		//   1. the row was deleted (CASCADE from version delete) —
		//      surface as ErrNotFound (resolved in the lookup
		//      below);
		//   2. the status guard failed because a concurrent caller
		//      flipped the row — re-read and re-evaluate the
		//      transition graph against the fresh state;
		//   3. the claim guard failed because a concurrent rescan
		//      cleared claimed_by / claimed_at — surface as
		//      ErrClaimLost so the worker can drop its result;
		//   4. the attempt_count guard failed because a concurrent
		//      admin Rescan reset attempt_count to 0 between
		//      RecordAttemptFailure and this UPDATE — also surface
		//      as ErrClaimLost so the worker drops the dead-letter
		//      transition (the row is now a fresh submitted attempt
		//      that the next worker tick will re-claim).
		// We disambiguate by re-reading the row's current claim
		// columns. The re-read is racy in isolation (claim could
		// flip again between read and any subsequent action) but
		// the worker treats ErrClaimLost as terminal-for-this-tick
		// and lets the next poll re-claim, so the diagnosis only
		// needs to be correct often enough to surface the right
		// error class.
		if in.ExpectedClaim != nil {
			lost, claimErr := rs.claimGuardFailed(ctx, in.VersionID, in.ExpectedClaim)
			if claimErr != nil {
				return nil, claimErr
			}
			if lost {
				return nil, ErrClaimLost
			}
		}
		if in.MinAttemptCount > 0 {
			lost, lookupErr := rs.attemptCountGuardFailed(ctx, in.VersionID, in.MinAttemptCount)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if lost {
				return nil, ErrClaimLost
			}
		}
		// Last attempt: no further UPDATE to gate on the fresh
		// state, so the re-read would be wasted. Break out and
		// surface the contention error below.
		if attempt == maxAttempts-1 {
			break
		}
		latest, lookupErr := rs.GetReviewState(ctx, in.VersionID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if latest.Status == in.Status {
			// Concurrent caller landed the same target — idempotent
			// success. Matches UpdateExtensionStatus's convergent-
			// transition contract. Return the latest row (which carries
			// the concurrent writer's reviewer / reviewed_at / notes)
			// rather than re-running the UPDATE — the caller's intent
			// is the resulting status, and overwriting fields written
			// by the winning concurrent caller would lose audit detail.
			return latest, nil
		}
		current = latest
	}
	return nil, fmt.Errorf("marketplace: update review state: gave up after %d contended retries on version %s", maxAttempts, in.VersionID)
}

// claimGuardFailed inspects the row's current claim columns and
// reports whether the worker's recorded claim was overwritten by
// a concurrent ResetReviewStateForRescan. A row that has been
// deleted is reported as not-lost (the caller falls through to
// the existing status-guard branch which translates ErrNoRows
// from the lookup into ErrNotFound).
func (rs *ReviewStateStore) claimGuardFailed(ctx context.Context, versionID uuid.UUID, expected *ReviewClaimGuard) (bool, error) {
	var (
		claimedBy *string
		claimedAt *time.Time
	)
	err := rs.store.pool.QueryRow(ctx,
		`SELECT claimed_by, claimed_at
		   FROM marketplace_extension_review_state
		  WHERE extension_version_id = $1`,
		versionID,
	).Scan(&claimedBy, &claimedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Row deleted out from under us — let the outer
			// retry loop's lookup translate this to ErrNotFound.
			return false, nil
		}
		return false, fmt.Errorf("marketplace: claim guard lookup: %w", err)
	}
	if claimedBy == nil || claimedAt == nil {
		// Rescan cleared the claim entirely.
		return true, nil
	}
	if *claimedBy != expected.ClaimedBy {
		return true, nil
	}
	if !claimedAt.Equal(expected.ClaimedAt) {
		return true, nil
	}
	return false, nil
}

// attemptCountGuardFailed inspects the row's current attempt_count
// and reports whether it is below the worker's required minimum
// (typically MaxReviewAttempts on the dead-letter transition path).
// A "lost" verdict means an admin Rescan reset the counter and the
// caller should drop the in-flight dead-letter transition. A row
// that has been deleted is reported as not-lost (the caller falls
// through to the existing status-guard branch which translates
// ErrNoRows into ErrNotFound).
func (rs *ReviewStateStore) attemptCountGuardFailed(ctx context.Context, versionID uuid.UUID, minRequired int) (bool, error) {
	var attemptCount int
	err := rs.store.pool.QueryRow(ctx,
		`SELECT attempt_count
		   FROM marketplace_extension_review_state
		  WHERE extension_version_id = $1`,
		versionID,
	).Scan(&attemptCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("marketplace: attempt count guard lookup: %w", err)
	}
	return attemptCount < minRequired, nil
}

// ListVersionsByReviewStatus returns the version ids currently in
// the given review status. Used by B7's review-queue UI to render
// the "awaiting human" inbox. Ordered by review_state.created_at ASC
// (oldest-first so the queue is a FIFO).
func (rs *ReviewStateStore) ListVersionsByReviewStatus(ctx context.Context, status ReviewStatus, limit int) ([]ReviewState, error) {
	if !status.Valid() {
		return nil, fmt.Errorf("%w: unknown review status %q", ErrInvalidManifest, status)
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := rs.store.pool.Query(ctx,
		`SELECT extension_version_id, status, automated_checks::text,
		        COALESCE(manual_review_notes,''), COALESCE(reviewer,''),
		        reviewed_at, attempt_count, last_attempt_error, last_attempt_at,
		        created_at, updated_at
		 FROM marketplace_extension_review_state
		 WHERE status = $1
		 ORDER BY created_at ASC
		 LIMIT $2`, string(status), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list versions by review status: %w", err)
	}
	defer rows.Close()
	out := make([]ReviewState, 0, 16)
	for rows.Next() {
		var r ReviewState
		if err := rows.Scan(
			&r.ExtensionVersionID, &r.Status,
			scanJSONB(&r.AutomatedChecks),
			&r.ManualReviewNotes, &r.Reviewer,
			&r.ReviewedAt, &r.AttemptCount, &r.LastAttemptError, &r.LastAttemptAt,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list versions by review status: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResetReviewStateForRescan re-claims a version for the review
// worker by re-setting its review_state row to `submitted`.
// Deliberately bypasses the normal reviewStatusTransitionAllowed
// graph (which only models forward transitions out of `submitted`):
// admin-initiated rescan is the one supported path that can move
// a non-terminal row backwards in the graph.
//
// Refuses to operate on the audit-trail terminal states (approved
// / rejected / withdrawn) — once a human reviewer decided the
// version, that decision is sealed and publishers re-submit by
// uploading a new version. `dead_letter` is the exception: the
// worker dead-lettered the row because it failed N attempts in a
// row WITHOUT producing any verdict, so it has no audit trail to
// preserve. Rescanning a dead-lettered row is exactly the
// "investigate, then retry" workflow B7.2 introduces, so we
// permit it. The handler-side check gates the audit terminals
// before calling here; this method enforces the same invariant
// defensively so a future caller can't bypass it.
//
// The reset clears reviewer + reviewed_at + manual_review_notes +
// automated_checks so the next worker run starts from a clean
// slate. Findings live in their own table and are overwritten by
// UpsertReviewFindings during the next pipeline run; the
// findings_store deletes orphaned rows that the new run does not
// re-emit (see UpsertReviewFindings godoc).
func (s *Store) ResetReviewStateForRescan(ctx context.Context, versionID uuid.UUID) error {
	if versionID == uuid.Nil {
		return fmt.Errorf("%w: version id required", ErrNotFound)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("marketplace: reset review state: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current ReviewStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM marketplace_extension_review_state
		 WHERE extension_version_id = $1 FOR UPDATE`,
		versionID,
	).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("marketplace: reset review state: select: %w", err)
	}
	// Audit-trail terminals (approved/rejected/withdrawn) are sealed.
	// dead_letter is also terminal in the worker's transition graph
	// but represents "no verdict was produced after N attempts" —
	// the row has no audit trail to preserve, and admin rescan is
	// the documented recovery path for it.
	if current.IsTerminal() && current != ReviewStatusDeadLetter {
		return fmt.Errorf("%w: cannot rescan terminal review state %q",
			ErrConflict, current)
	}
	// Also clear claimed_at / claimed_by atomically with the
	// status reset. Without this the previous worker's claim
	// would persist on the row, and the next worker's tick
	// would skip the row until the 10-minute lease lapses — an
	// admin clicking Rescan expects work to start immediately,
	// not 10 minutes later.
	// Reset attempt accounting too. An admin clicking Rescan is
	// asserting "this time it'll work" — preserving attempt_count
	// would mean a rescued row with 4 prior failures is one strike
	// away from dead-lettering again on the first transient blip
	// after rescan. The conservative choice is the full B7.2
	// MaxReviewAttempts budget; if the rescued run also fails the
	// operator gets the whole window to investigate before the
	// dead-letter transition fires again.
	if _, err := tx.Exec(ctx,
		`UPDATE marketplace_extension_review_state
		    SET status = $1,
		        automated_checks = '{}'::jsonb,
		        manual_review_notes = NULL,
		        reviewer = NULL,
		        reviewed_at = NULL,
		        claimed_at = NULL,
		        claimed_by = NULL,
		        attempt_count = 0,
		        last_attempt_error = '',
		        last_attempt_at = NULL,
		        updated_at = now()
		  WHERE extension_version_id = $2`,
		string(ReviewStatusSubmitted), versionID,
	); err != nil {
		return fmt.Errorf("marketplace: reset review state: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("marketplace: reset review state: commit: %w", err)
	}
	return nil
}

// reviewStatusTransitionAllowed encodes the directed graph from the
// ReviewStatus godoc.
//
// All states (terminal and non-terminal) allow self-loops so a B7
// at-least-once worker that re-issues the same UpdateReviewState
// call on retry succeeds without surfacing a spurious "invalid
// transition" error.
//
// Non-terminal self-loops: the UPDATE path bumps updated_at and
// re-writes automated_checks, which is the intended idempotent
// behaviour (re-running automated scans against the same version
// row overwrites the prior result with the fresh one).
//
// Terminal self-loops (approved→approved, rejected→rejected,
// withdrawn→withdrawn): permitted by this function so the
// transition-graph check in UpdateReviewState does not surface a
// spurious error, but UpdateReviewState short-circuits BEFORE the
// UPDATE fires — the existing row is returned unchanged so the
// reviewer/reviewed_at audit trail cannot be overwritten. A caller
// passing a different reviewer on a terminal-state row is rejected
// loudly. See UpdateReviewState for the precise gate.
//
// Terminal states (approved / rejected / withdrawn) additionally
// reject ALL non-self transitions — once a version is approved it
// cannot fall back to manual_review, once rejected it cannot be
// silently un-rejected, once withdrawn it cannot be resurrected.
// Publishers re-submit by uploading a new version (new
// extension_version_id, new review_state row).
func reviewStatusTransitionAllowed(from, to ReviewStatus) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	switch from {
	case ReviewStatusSubmitted:
		// submitted→manual_review is the worker's path when one or
		// more warn-level findings land: automated checks ran to
		// completion but a human must decide. Without this edge the
		// pipeline would have to two-step via automated_passed first,
		// which would (a) confuse the audit trail (automated_passed
		// would mean "checks ran cleanly" in one branch and "checks
		// ran but produced warnings" in another) and (b) require a
		// second UpdateReviewState call inside the same worker tick.
		//
		// submitted→dead_letter is the B7.2 retry-exhausted path:
		// the worker tried MaxReviewAttempts times in a row and the
		// pipeline never produced a verdict (CDN unreachable,
		// bundle parser exception). Reviewer-free transition because
		// the system itself decided to give up; the admin Rescan
		// endpoint moves the row back to `submitted` with a fresh
		// attempt budget when the operator has investigated.
		switch to {
		case ReviewStatusAutomatedPassed, ReviewStatusManualReview,
			ReviewStatusRejected, ReviewStatusWithdrawn,
			ReviewStatusDeadLetter:
			return true
		}
	case ReviewStatusAutomatedPassed:
		switch to {
		case ReviewStatusManualReview, ReviewStatusRejected, ReviewStatusWithdrawn:
			return true
		}
	case ReviewStatusManualReview:
		switch to {
		case ReviewStatusApproved, ReviewStatusRejected, ReviewStatusWithdrawn:
			return true
		}
	}
	return false
}

// MaxReviewAttempts is the upper bound on consecutive failed
// pipeline runs against a single review row before the worker
// transitions the row to ReviewStatusDeadLetter. Set to 5 so a
// short blip (one bad CDN response, a transient DB error) gets
// retried but a persistent failure (corrupted bundle, missing CDN
// origin) surfaces to the admin queue within ≥5 ticks.
//
// Lives here (not in the worker package) so the store's transition
// gate and the worker's loop reference the same constant; changing
// it requires a single edit and is automatically reflected in the
// transition-graph tests.
const MaxReviewAttempts = 5

// truncateUTF8 returns s truncated to at most maxBytes bytes at a
// UTF-8 rune boundary. If s is already <= maxBytes and valid UTF-8
// it is returned unchanged. The returned string is guaranteed to
// be valid UTF-8 even if s contained an invalid sequence at or
// beyond maxBytes (PostgreSQL `text` columns reject invalid
// UTF-8). Always produces a result of length <= maxBytes.
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || s == "" {
		return ""
	}
	if len(s) <= maxBytes && utf8.ValidString(s) {
		return s
	}
	// Walk runes until adding the next rune would exceed maxBytes.
	// utf8.DecodeRuneInString returns RuneError + size=1 for an
	// invalid byte; treat that byte as a stop signal so the
	// returned string never contains the bad sequence.
	out := 0
	for out < len(s) && out < maxBytes {
		r, size := utf8.DecodeRuneInString(s[out:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		if out+size > maxBytes {
			break
		}
		out += size
	}
	return s[:out]
}

// RecordAttemptFailure increments attempt_count on the row and
// stamps the new last_attempt_error / last_attempt_at. The
// increment happens under FOR UPDATE on the row so concurrent
// ClaimSubmittedReviewVersions calls see the incremented counter
// atomically.
//
// Claim handling is conditional on the post-increment count:
//
//   - count < MaxReviewAttempts: claim is cleared (set NULL) so
//     the next ClaimSubmittedReviewVersions poll re-picks the row
//     immediately, no need to wait for the 10-minute lease to
//     lapse.
//   - count >= MaxReviewAttempts: claim is RETAINED so the
//     caller (worker) can run the dead-letter UpdateReviewState
//     transition under its own ExpectedClaim guard. This closes
//     a wasted-pipeline-run race window where, between this
//     UPDATE committing and the dead-letter UPDATE landing,
//     another replica could otherwise claim the row (claim was
//     NULL) and begin a doomed pipeline run that the dead-letter
//     transition would then invalidate. With the claim retained,
//     ClaimSubmittedReviewVersions correctly skips the row
//     (its WHERE clause requires NULL-or-stale claimed_at), so
//     no second worker can race.
//
// When the post-increment attempt_count is >= MaxReviewAttempts,
// returns (newCount, ErrReviewMaxAttemptsExceeded) WITHOUT
// transitioning the row. The caller (worker) catches the sentinel
// and runs the dead-letter UpdateReviewState transition with a
// synthetic finding row recording the final failure, so the
// dead-letter path goes through the normal transition-graph guard
// rather than a backdoor UPDATE here.
//
// expectedClaim is the same ReviewClaimGuard the worker passes to
// UpdateReviewState: an admin Rescan landing between the worker's
// claim and this call would have cleared the claim, so the UPDATE
// guards on (claimed_by, claimed_at) matching exactly. If the
// guard fails, returns (0, ErrClaimLost) and the worker drops the
// attempt — the freshly-reset row's attempt_count is already 0,
// so incrementing here would be wrong.
func (rs *ReviewStateStore) RecordAttemptFailure(
	ctx context.Context,
	versionID uuid.UUID,
	expectedClaim *ReviewClaimGuard,
	errMsg string,
) (int, error) {
	if versionID == uuid.Nil {
		return 0, fmt.Errorf("%w: version id required", ErrNotFound)
	}
	// Bound the stored error string. Pipeline errors are usually
	// short but defensive: a panic-wrapped multi-line stack would
	// blow out the admin queue response. 1 KiB is generous for a
	// one-line summary.
	//
	// Truncate at a rune boundary, NOT at a raw byte position. A
	// naive `errMsg[:maxErrLen]` slice could split a multi-byte
	// UTF-8 character (Go errors usually ASCII, but file paths,
	// wrapped library messages, or i18n text can carry runes), and
	// PostgreSQL's `text` column type validates UTF-8 — an invalid
	// trailing fragment causes the INSERT to fail with
	// `invalid_byte_sequence_for_encoding`, which would mean the
	// failure-recording UPDATE itself fails and attempt_count
	// silently stops incrementing.
	const maxErrLen = 1024
	errMsg = truncateUTF8(errMsg, maxErrLen)

	var (
		expectedClaimBy any
		expectedClaimAt any
	)
	if expectedClaim != nil {
		expectedClaimBy = expectedClaim.ClaimedBy
		expectedClaimAt = expectedClaim.ClaimedAt
	}

	tx, err := rs.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("marketplace: record attempt failure: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Increment + conditionally clear claim atomically. The claim
	// is retained when the post-increment count crosses
	// MaxReviewAttempts so the caller's dead-letter UPDATE can
	// authenticate via ExpectedClaim — see this method's godoc for
	// the wasted-pipeline-run race this closes.
	//
	// The claim guard fires when an admin Rescan landed between
	// claim and this call: ResetReviewStateForRescan nulls
	// claimed_by/claimed_at, so the guard's `claimed_by =
	// $expectedBy` predicate fails and the UPDATE affects zero
	// rows.
	var newCount int
	err = tx.QueryRow(ctx,
		`UPDATE marketplace_extension_review_state
		    SET attempt_count = attempt_count + 1,
		        last_attempt_error = $2,
		        last_attempt_at = now(),
		        claimed_at = CASE WHEN (attempt_count + 1) >= $6::int
		                          THEN claimed_at ELSE NULL END,
		        claimed_by = CASE WHEN (attempt_count + 1) >= $6::int
		                          THEN claimed_by ELSE NULL END,
		        updated_at = now()
		  WHERE extension_version_id = $1
		    AND status = $3
		    AND ($4::text IS NULL OR claimed_by = $4)
		    AND ($5::timestamptz IS NULL OR claimed_at = $5)
		 RETURNING attempt_count`,
		versionID, errMsg, string(ReviewStatusSubmitted),
		expectedClaimBy, expectedClaimAt,
		MaxReviewAttempts,
	).Scan(&newCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Disambiguate row-deleted vs status-flipped vs
			// claim-lost. We re-read under the same tx (no FOR
			// UPDATE because we're about to commit-and-return).
			var status ReviewStatus
			var claimedBy *string
			var claimedAt *time.Time
			lookupErr := tx.QueryRow(ctx,
				`SELECT status, claimed_by, claimed_at
				   FROM marketplace_extension_review_state
				  WHERE extension_version_id = $1`,
				versionID,
			).Scan(&status, &claimedBy, &claimedAt)
			if lookupErr != nil {
				if errors.Is(lookupErr, pgx.ErrNoRows) {
					return 0, ErrNotFound
				}
				return 0, fmt.Errorf("marketplace: record attempt failure: disambiguate: %w", lookupErr)
			}
			if status != ReviewStatusSubmitted {
				// Concurrent transition out of submitted —
				// success, withdrawal, or admin rescan completing
				// just-in-time. Treat as claim-lost so the worker
				// drops its attempt.
				return 0, ErrClaimLost
			}
			if expectedClaim != nil {
				if claimedBy == nil || claimedAt == nil ||
					*claimedBy != expectedClaim.ClaimedBy ||
					!claimedAt.Equal(expectedClaim.ClaimedAt) {
					return 0, ErrClaimLost
				}
			}
			return 0, fmt.Errorf("marketplace: record attempt failure: update affected zero rows on version %s with no diagnosable cause", versionID)
		}
		return 0, fmt.Errorf("marketplace: record attempt failure: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("marketplace: record attempt failure: commit: %w", err)
	}
	if newCount >= MaxReviewAttempts {
		return newCount, ErrReviewMaxAttemptsExceeded
	}
	return newCount, nil
}
