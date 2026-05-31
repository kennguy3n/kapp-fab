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

// FindingsStore owns reads/writes against marketplace_review_findings.
// Mounted via Store.Findings() so callers can take only the
// findings surface they need.
type FindingsStore struct {
	store *Store
}

// Findings returns a FindingsStore bound to the parent pool.
func (s *Store) Findings() *FindingsStore {
	return &FindingsStore{store: s}
}

// UpsertReviewFindings replaces the finding set for the version
// atomically: every prior finding whose natural key (check_name,
// code, location) is NOT in the new set is deleted; new findings
// overwrite by the same key. All work happens in a single tx so a
// partial-failure re-scan never leaves the version with a mixed
// old+new finding state.
//
// The pipeline calls this after each Run; the worker is the only
// production caller. Tests substitute a fake via the
// review.FindingSink interface.
func (fs *FindingsStore) UpsertReviewFindings(ctx context.Context, versionID uuid.UUID, findings []ReviewFinding) error {
	if versionID == uuid.Nil {
		return fmt.Errorf("%w: version id required", ErrNotFound)
	}
	for i := range findings {
		f := &findings[i]
		if !f.Severity.Valid() {
			return fmt.Errorf("%w: findings[%d].severity %q not in (error|warn|info)",
				ErrInvalidManifest, i, f.Severity)
		}
		if f.CheckName == "" {
			return fmt.Errorf("%w: findings[%d].check_name required",
				ErrInvalidManifest, i)
		}
		if f.Code == "" {
			return fmt.Errorf("%w: findings[%d].code required",
				ErrInvalidManifest, i)
		}
	}
	tx, err := fs.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("marketplace: begin upsert findings tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Snapshot existing natural keys so we can compute the
	// delete-set without round-tripping per finding. The
	// natural key is (check_name, code, location).
	rows, err := tx.Query(ctx,
		`SELECT check_name, code, location
		   FROM marketplace_review_findings
		  WHERE extension_version_id = $1`, versionID,
	)
	if err != nil {
		return fmt.Errorf("marketplace: load existing findings: %w", err)
	}
	type key struct {
		Check    string
		Code     string
		Location string
	}
	existing := make(map[key]bool)
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.Check, &k.Code, &k.Location); err != nil {
			rows.Close()
			return fmt.Errorf("marketplace: load existing findings: scan: %w", err)
		}
		existing[k] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Upsert each new finding. ON CONFLICT (extension_version_id,
	// check_name, code, location) DO UPDATE refreshes the
	// message and severity (a re-scan may produce a different
	// message — e.g. the "X is too large" message includes the
	// new size).
	newKeys := make(map[key]bool, len(findings))
	for i := range findings {
		f := &findings[i]
		k := key{Check: f.CheckName, Code: f.Code, Location: f.Location}
		newKeys[k] = true
		created := f.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO marketplace_review_findings (
			    extension_version_id, check_name, severity, code, message, location, created_at
			 ) VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (extension_version_id, check_name, code, location)
			 DO UPDATE SET
			    severity = EXCLUDED.severity,
			    message  = EXCLUDED.message`,
			versionID, f.CheckName, string(f.Severity), f.Code, f.Message, f.Location, created,
		); err != nil {
			return fmt.Errorf("marketplace: upsert finding (%s/%s/%s): %w",
				f.CheckName, f.Code, f.Location, err)
		}
	}

	// Delete prior findings whose key is no longer present.
	for k := range existing {
		if newKeys[k] {
			continue
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM marketplace_review_findings
			  WHERE extension_version_id = $1
			    AND check_name = $2
			    AND code = $3
			    AND location = $4`,
			versionID, k.Check, k.Code, k.Location,
		); err != nil {
			return fmt.Errorf("marketplace: delete stale finding (%s/%s/%s): %w",
				k.Check, k.Code, k.Location, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("marketplace: commit upsert findings: %w", err)
	}
	return nil
}

// ListReviewFindings returns every finding for a version, ordered
// by (check_name, code, location) so the admin UI sees a stable
// list across reloads.
func (fs *FindingsStore) ListReviewFindings(ctx context.Context, versionID uuid.UUID) ([]ReviewFinding, error) {
	if versionID == uuid.Nil {
		return nil, fmt.Errorf("%w: version id required", ErrNotFound)
	}
	rows, err := fs.store.pool.Query(ctx,
		`SELECT id, extension_version_id, check_name, severity, code, message,
		        location, created_at
		 FROM marketplace_review_findings
		 WHERE extension_version_id = $1
		 ORDER BY check_name, code, location`, versionID,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list findings: %w", err)
	}
	defer rows.Close()
	out := make([]ReviewFinding, 0, 8)
	for rows.Next() {
		var f ReviewFinding
		var sev string
		if err := rows.Scan(
			&f.ID, &f.ExtensionVersionID, &f.CheckName, &sev, &f.Code, &f.Message,
			&f.Location, &f.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list findings: scan: %w", err)
		}
		f.Severity = Severity(sev)
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteAllReviewFindings deletes every finding for a version.
// Useful for the admin "rescan" endpoint which wants to discard
// the old finding set without trusting UpsertReviewFindings to
// catch every stale row (e.g. a check that was removed between
// runs).
func (fs *FindingsStore) DeleteAllReviewFindings(ctx context.Context, versionID uuid.UUID) error {
	if versionID == uuid.Nil {
		return fmt.Errorf("%w: version id required", ErrNotFound)
	}
	_, err := fs.store.pool.Exec(ctx,
		`DELETE FROM marketplace_review_findings WHERE extension_version_id = $1`,
		versionID,
	)
	if err != nil {
		return fmt.Errorf("marketplace: delete findings: %w", err)
	}
	return nil
}

// ReviewClaimLeaseDuration is how long a worker holds an
// atomically-claimed review_state row before another worker can
// re-claim it via the lease-expiry branch. Long enough to outlast
// the 90s per-version pipeline timeout (so a healthy worker never
// races itself) and short enough that an operator restart doesn't
// strand work for hours.
const ReviewClaimLeaseDuration = 10 * time.Minute

// ClaimedReviewVersion is the (version_id, claimed_at) tuple
// returned by ClaimSubmittedReviewVersions. The worker threads
// ClaimedAt into Pipeline.Persist (via Result.ClaimedAt and
// UpdateReviewStateInput.ExpectedClaim) so the eventual state
// UPDATE atomically aborts if an admin Rescan cleared the claim
// in the gap between claim and persist (see UpdateReviewState's
// claim-guard SQL).
type ClaimedReviewVersion struct {
	VersionID uuid.UUID
	ClaimedAt time.Time
}

// ClaimSubmittedReviewVersions is the worker's polling query. It
// atomically claims up to `limit` versions in the `submitted`
// review state by stamping claimed_at + claimed_by inside a single
// UPDATE...RETURNING statement, ordered oldest-first.
//
// Why UPDATE...RETURNING (not SELECT FOR UPDATE SKIP LOCKED):
// `s.pool.Query` runs the statement under an implicit per-statement
// transaction whose locks release at end-of-statement. A bare
// SELECT FOR UPDATE SKIP LOCKED would lose its locks the moment
// the result rows reach the worker's Go code, leaving a window
// where a concurrent worker (or the same worker on its next tick,
// before pipeline.Persist advances the status) could re-claim the
// same row. The canonical Postgres job-queue pattern is to fold
// the claim AND the state-mutation that proves it claimed into a
// single atomic UPDATE, which is what this method does.
//
// Lease expiry: rows whose claimed_at is older than the lease
// duration (see ReviewClaimLeaseDuration) are eligible for
// re-claim. This recovers work stranded by a crashed worker
// without needing a separate sweeper job.
//
// workerID is recorded as claimed_by for forensic debugging AND
// participates in the claim-guard SQL on UpdateReviewState (see
// UpdateReviewStateInput.ExpectedClaim) so a concurrent rescan
// that clears claimed_by reliably defeats a late Persist call.
// The (workerID, claimedAt) tuple is unique per claim because
// each ClaimSubmittedReviewVersions call stamps now() afresh,
// even on re-claim by the same worker after a lease expiry.
//
// This is a Store-level method (not exposed via FindingsStore)
// because it intentionally pulls from the review_state table, not
// the findings table — but it lives here for proximity to the
// pipeline's persistence flow.
func (s *Store) ClaimSubmittedReviewVersions(ctx context.Context, workerID string, limit int) ([]ClaimedReviewVersion, error) {
	if limit <= 0 {
		limit = 4
	}
	if limit > 64 {
		limit = 64
	}
	if strings.TrimSpace(workerID) == "" {
		// Forensic-only field; never gate the claim on it being
		// non-empty, but normalise to a sentinel so the DB never
		// stores an empty TEXT (which would obscure the
		// "set by some worker, identity unknown" case).
		workerID = "unknown-worker"
	}
	// The CTE picks the row IDs to claim inside SKIP LOCKED so
	// concurrent workers don't conflict on the ORDER BY. The
	// outer UPDATE flips claimed_at to now() atomically; rows
	// thus disappear from the SKIP LOCKED candidate set for any
	// concurrent claim in the same statement-window because
	// their claimed_at is non-NULL.
	//
	// Lease-expiry branch: status='submitted' AND claimed_at is
	// either NULL (fresh row) or older than now() - lease (stale
	// claim from a dead worker). Either way the row is up for
	// grabs.
	//
	// RETURNING includes claimed_at so the worker can pass the
	// exact DB-stamped timestamp through to UpdateReviewState's
	// claim guard — a re-read of the column would race the
	// rescan we're guarding against.
	rows, err := s.pool.Query(ctx,
		`WITH candidates AS (
		    SELECT extension_version_id
		      FROM marketplace_extension_review_state
		     WHERE status = $1
		       AND (claimed_at IS NULL OR claimed_at < now() - $2::interval)
		     ORDER BY created_at ASC
		     FOR UPDATE SKIP LOCKED
		     LIMIT $3
		 )
		 UPDATE marketplace_extension_review_state s
		    SET claimed_at = now(),
		        claimed_by = $4,
		        updated_at = now()
		   FROM candidates c
		  WHERE s.extension_version_id = c.extension_version_id
		 RETURNING s.extension_version_id, s.claimed_at`,
		string(ReviewStatusSubmitted),
		fmt.Sprintf("%d milliseconds", ReviewClaimLeaseDuration.Milliseconds()),
		limit,
		workerID,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: claim submitted versions: %w", err)
	}
	defer rows.Close()
	out := make([]ClaimedReviewVersion, 0, limit)
	for rows.Next() {
		var c ClaimedReviewVersion
		if err := rows.Scan(&c.VersionID, &c.ClaimedAt); err != nil {
			return nil, fmt.Errorf("marketplace: claim submitted versions: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// resolvePublisherKeysForVersion is the SignatureCheck's PolicyLoader.
// Given a version row, returns the publisher row + non-revoked
// key set. The publisher slug is read from the manifest's
// derived publisher field; we look the row up by slug rather than
// by id to keep the call site decoupled from B6's
// marketplace_extensions row.
func (s *Store) resolvePublisherKeysForVersion(ctx context.Context, versionID uuid.UUID) (*Publisher, []PublisherKey, error) {
	var publisherSlug string
	err := s.pool.QueryRow(ctx,
		`SELECT e.publisher
		   FROM marketplace_extension_versions v
		   JOIN marketplace_extensions e ON e.id = v.extension_id
		  WHERE v.id = $1`, versionID,
	).Scan(&publisherSlug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("marketplace: resolve publisher slug for version: %w", err)
	}
	pubs := s.Publishers()
	pub, err := pubs.GetPublisherBySlug(ctx, publisherSlug)
	if err != nil {
		return nil, nil, err
	}
	keys, err := pubs.ListPublisherKeys(ctx, pub.ID, false)
	if err != nil {
		return nil, nil, err
	}
	return pub, keys, nil
}

// ResolvePublisherKeysForVersion exposes the publisher + non-revoked
// key set so the worker can pass it to the SignatureCheck. Wraps
// the package-private helper to keep the lookup centralised.
func (s *Store) ResolvePublisherKeysForVersion(ctx context.Context, versionID uuid.UUID) (*Publisher, []PublisherKey, error) {
	return s.resolvePublisherKeysForVersion(ctx, versionID)
}
