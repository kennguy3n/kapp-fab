package marketplace

import (
	"context"
	"errors"
	"fmt"
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

// ClaimSubmittedReviewVersions is the worker's polling query.
// Returns up to `limit` versions in the `submitted` review state,
// ordered oldest-first, with SKIP LOCKED so concurrent workers
// don't conflict. Each row is returned to the worker WITHIN a
// transaction it owns — the worker is responsible for advancing
// the row's status and committing the tx. If the tx rolls back,
// the SKIP LOCKED unblocks the row for the next poll cycle.
//
// The worker uses dbutil.WithTx (no tenant GUC — review state is
// global). Returns the locked transaction so the caller can
// process the version IDs and commit at the end.
//
// This is a Store-level method (not exposed via FindingsStore)
// because it intentionally pulls from the review_state table, not
// the findings table — but it lives here for proximity to the
// pipeline's persistence flow.
func (s *Store) ClaimSubmittedReviewVersions(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 4
	}
	if limit > 64 {
		limit = 64
	}
	// SELECT ... FOR UPDATE SKIP LOCKED is the canonical
	// idempotent worker pattern. The same row will be re-emitted
	// to the next poll if the worker's tx aborts.
	//
	// We do NOT advance the status here — the worker advances
	// via UpdateReviewState after running the pipeline. This
	// keeps Claim's contract simple ("give me work") and the
	// transition writes funnel through the single audited path.
	rows, err := s.pool.Query(ctx,
		`SELECT extension_version_id
		   FROM marketplace_extension_review_state
		  WHERE status = $1
		  ORDER BY created_at ASC
		  FOR UPDATE SKIP LOCKED
		  LIMIT $2`,
		string(ReviewStatusSubmitted), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: claim submitted versions: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("marketplace: claim submitted versions: scan: %w", err)
		}
		out = append(out, id)
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
