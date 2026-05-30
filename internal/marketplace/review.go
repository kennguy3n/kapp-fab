package marketplace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
		        reviewed_at, created_at, updated_at
		 FROM marketplace_extension_review_state
		 WHERE extension_version_id = $1`, versionID,
	).Scan(
		&out.ExtensionVersionID, &out.Status,
		scanJSONB(&out.AutomatedChecks),
		&out.ManualReviewNotes, &out.Reviewer,
		&out.ReviewedAt, &out.CreatedAt, &out.UpdatedAt,
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
type UpdateReviewStateInput struct {
	VersionID       uuid.UUID
	Status          ReviewStatus
	AutomatedChecks []byte
	ManualNotes     string
	Reviewer        string
	ReviewedAt      *time.Time
}

// UpdateReviewState transitions a review row. Enforces the same
// transition graph the B7 pipeline assumes:
//
//	submitted        → automated_passed | rejected | withdrawn
//	automated_passed → manual_review    | rejected | withdrawn
//	manual_review    → approved | rejected | withdrawn
//	approved         → (terminal; only the AutomatedChecks JSON may
//	                    be amended for forensic detail)
//	rejected         → (terminal; same caveat)
//	withdrawn        → (terminal)
//
// A transition to a terminal state stamps ReviewedAt = now() if not
// provided. Returns ErrNotFound if the version has no review state.
func (rs *ReviewStateStore) UpdateReviewState(ctx context.Context, in UpdateReviewStateInput) (*ReviewState, error) {
	if in.VersionID == uuid.Nil {
		return nil, fmt.Errorf("%w: version id required", ErrNotFound)
	}
	if !in.Status.Valid() {
		return nil, fmt.Errorf("%w: unknown review status %q", ErrInvalidManifest, in.Status)
	}
	current, err := rs.GetReviewState(ctx, in.VersionID)
	if err != nil {
		return nil, err
	}
	if !reviewStatusTransitionAllowed(current.Status, in.Status) {
		return nil, fmt.Errorf("%w: cannot transition review status from %q to %q",
			ErrInvalidManifest, current.Status, in.Status)
	}
	// Approved / rejected require a reviewer + reviewed_at; the DB
	// CHECK enforces this, but doing it here surfaces the actionable
	// error to the caller.
	needReviewer := in.Status == ReviewStatusApproved || in.Status == ReviewStatusRejected
	if needReviewer && strings.TrimSpace(in.Reviewer) == "" {
		return nil, fmt.Errorf("%w: reviewer required when status=%s", ErrInvalidManifest, in.Status)
	}
	reviewedAt := in.ReviewedAt
	if needReviewer && reviewedAt == nil {
		now := time.Now().UTC()
		reviewedAt = &now
	}
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
		 RETURNING extension_version_id, status, automated_checks::text,
		           COALESCE(manual_review_notes,''), COALESCE(reviewer,''),
		           reviewed_at, created_at, updated_at`,
		in.VersionID, string(in.Status), string(checks), notes, reviewer, reviewedAt,
	).Scan(
		&out.ExtensionVersionID, &out.Status,
		scanJSONB(&out.AutomatedChecks),
		&out.ManualReviewNotes, &out.Reviewer,
		&out.ReviewedAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: update review state: %w", err)
	}
	return &out, nil
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
		        reviewed_at, created_at, updated_at
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
			&r.ReviewedAt, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list versions by review status: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// reviewStatusTransitionAllowed encodes the directed graph from the
// ReviewStatus godoc. Terminal states only allow self-loops (so
// repeat writes of the same approved/rejected/withdrawn row succeed
// for idempotency; this makes B7's at-least-once worker safe).
func reviewStatusTransitionAllowed(from, to ReviewStatus) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	switch from {
	case ReviewStatusSubmitted:
		switch to {
		case ReviewStatusAutomatedPassed, ReviewStatusRejected, ReviewStatusWithdrawn:
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
