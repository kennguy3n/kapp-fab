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

// PublisherStore owns reads/writes against marketplace_publishers
// and marketplace_publisher_keys. Mounted via Store.Publishers() so
// callers (handlers, the review pipeline) can take only the
// publisher surface they need without picking up the full Store.
type PublisherStore struct {
	store *Store
}

// Publishers returns a PublisherStore bound to the parent pool.
func (s *Store) Publishers() *PublisherStore {
	return &PublisherStore{store: s}
}

// GetPublisher returns the publisher row by ID. ErrNotFound if
// the row is missing.
func (ps *PublisherStore) GetPublisher(ctx context.Context, id uuid.UUID) (*Publisher, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	var out Publisher
	var verifiedAt *time.Time
	var verifiedBy, verifyNotes string
	err := ps.store.pool.QueryRow(ctx,
		`SELECT id, slug, display_name, contact_email,
		        verified_at, COALESCE(verified_by,''), COALESCE(verification_notes,''),
		        auto_approve_patch, created_at, updated_at
		 FROM marketplace_publishers WHERE id = $1`, id,
	).Scan(
		&out.ID, &out.Slug, &out.DisplayName, &out.ContactEmail,
		&verifiedAt, &verifiedBy, &verifyNotes,
		&out.AutoApprovePatch, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get publisher: %w", err)
	}
	out.VerifiedAt = verifiedAt
	out.VerifiedBy = verifiedBy
	out.VerificationNotes = verifyNotes
	return &out, nil
}

// GetPublisherBySlug returns the publisher row by slug.
// ErrNotFound if no row exists.
func (ps *PublisherStore) GetPublisherBySlug(ctx context.Context, slug string) (*Publisher, error) {
	if slug == "" {
		return nil, fmt.Errorf("%w: publisher slug required", ErrNotFound)
	}
	var out Publisher
	var verifiedAt *time.Time
	var verifiedBy, verifyNotes string
	err := ps.store.pool.QueryRow(ctx,
		`SELECT id, slug, display_name, contact_email,
		        verified_at, COALESCE(verified_by,''), COALESCE(verification_notes,''),
		        auto_approve_patch, created_at, updated_at
		 FROM marketplace_publishers WHERE slug = $1`, slug,
	).Scan(
		&out.ID, &out.Slug, &out.DisplayName, &out.ContactEmail,
		&verifiedAt, &verifiedBy, &verifyNotes,
		&out.AutoApprovePatch, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get publisher by slug: %w", err)
	}
	out.VerifiedAt = verifiedAt
	out.VerifiedBy = verifiedBy
	out.VerificationNotes = verifyNotes
	return &out, nil
}

// CreatePublisherInput is the parameter block for a brand-new
// publisher row. CreatePublisher is the operator-side flow; B7
// itself relies on the migration's backfill from the distinct
// publisher column on marketplace_extensions to seed legacy rows.
type CreatePublisherInput struct {
	Slug         string
	DisplayName  string
	ContactEmail string
}

// CreatePublisher inserts a new publisher row. ErrConflict if the
// slug already exists. Format constraints are enforced by the DB
// CHECK constraints; the Go code surfaces the validation errors
// with a clearer message.
func (ps *PublisherStore) CreatePublisher(ctx context.Context, in CreatePublisherInput) (*Publisher, error) {
	slug := strings.TrimSpace(in.Slug)
	display := strings.TrimSpace(in.DisplayName)
	email := strings.TrimSpace(in.ContactEmail)
	if slug == "" {
		return nil, fmt.Errorf("%w: publisher slug required", ErrInvalidManifest)
	}
	if display == "" {
		display = slug
	}
	if email == "" {
		return nil, fmt.Errorf("%w: publisher contact_email required", ErrInvalidManifest)
	}
	var id uuid.UUID
	err := ps.store.pool.QueryRow(ctx,
		`INSERT INTO marketplace_publishers (slug, display_name, contact_email)
		 VALUES ($1, $2, $3)
		 RETURNING id`, slug, display, email,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("marketplace: create publisher: %w", err)
	}
	return ps.GetPublisher(ctx, id)
}

// VerifyPublisherInput is the parameter block for VerifyPublisher.
// AutoApprovePatch enables the B7.1 fast-path; ignored by the v1
// pipeline (which always routes through manual_review).
type VerifyPublisherInput struct {
	PublisherID      uuid.UUID
	Reviewer         string // operator identifier; written to verified_by
	Notes            string
	AutoApprovePatch bool
}

// VerifyPublisher marks a publisher as operator-verified. Stamps
// verified_at = now() and verified_by = in.Reviewer. The CHECK on
// auto_approve_requires_verified ensures auto_approve_patch can
// only be true on verified rows; we set both in the same UPDATE
// so the order doesn't matter.
//
// Idempotency: calling VerifyPublisher on an already-verified
// publisher returns the row unchanged WITHOUT bumping verified_at
// (the audit trail records the FIRST verification action). To
// re-verify (e.g. after a security incident) the operator must
// first call UnverifyPublisher.
func (ps *PublisherStore) VerifyPublisher(ctx context.Context, in VerifyPublisherInput) (*Publisher, error) {
	if in.PublisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	if strings.TrimSpace(in.Reviewer) == "" {
		return nil, fmt.Errorf("%w: reviewer required", ErrInvalidManifest)
	}
	cur, err := ps.GetPublisher(ctx, in.PublisherID)
	if err != nil {
		return nil, err
	}
	if cur.VerifiedAt != nil {
		// Idempotent: existing row unchanged so verified_at /
		// verified_by stay pinned to the FIRST verification
		// action for audit integrity. To flip auto_approve_patch
		// on an already-verified publisher, call
		// SetAutoApprovePatch — VerifyPublisher's
		// AutoApprovePatch input is the initial-verification
		// flag only.
		return cur, nil
	}
	// The UPDATE includes `AND verified_at IS NULL` so two
	// concurrent VerifyPublisher calls that both observed
	// verified_at = NULL in their pre-read can't both write
	// (only the first reaches a row whose verified_at is still
	// NULL; the second sees zero rows affected and the audit
	// trail keeps its first-verification timestamp). The Go-side
	// pre-check above is an early-out to avoid the round-trip on
	// the obvious "already verified" case; the SQL predicate is
	// the authoritative guard.
	now := time.Now().UTC()
	tag, err := ps.store.pool.Exec(ctx,
		`UPDATE marketplace_publishers
		    SET verified_at = $2,
		        verified_by = $3,
		        verification_notes = NULLIF($4,''),
		        auto_approve_patch = $5,
		        updated_at = now()
		  WHERE id = $1
		    AND verified_at IS NULL`,
		in.PublisherID, now, in.Reviewer, in.Notes, in.AutoApprovePatch,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: verify publisher: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// A concurrent caller verified between our pre-read and
		// our UPDATE; surface the row that won the race so the
		// caller sees the authoritative state.
		return ps.GetPublisher(ctx, in.PublisherID)
	}
	return ps.GetPublisher(ctx, in.PublisherID)
}

// SetAutoApprovePatch flips the auto_approve_patch flag on an
// already-verified publisher. Used when an operator wants to
// enable (or revoke) the B7.1 patch fast-path on a publisher
// that's already been operator-verified — VerifyPublisher's
// idempotency contract keeps the audit-trail integrity of the
// first verification, so this is the separate-call path the
// VerifyPublisher godoc promises for tweaking the flag after
// the fact.
//
// The DB CHECK auto_approve_requires_verified enforces the
// invariant that auto_approve_patch can only be true on rows
// with verified_at IS NOT NULL; we re-check Go-side so the
// caller gets ErrPublisherNotVerified rather than a generic
// CHECK violation. Setting AutoApprovePatch=false is always
// safe (the CHECK only constrains true) and is the recovery
// path when an operator wants to revoke fast-path on an
// already-verified publisher without unverifying them.
//
// The UPDATE is gated by `auto_approve_patch IS DISTINCT FROM
// $2` so calls that don't change the value are a true no-op —
// `updated_at` is preserved, no row is rewritten, and the
// existing publisher row is returned unchanged. Audit-trail
// consumers can rely on updated_at bumping only when the flag
// actually flips. (Devin Review ANALYSIS_0001 on commit
// 6783035 — no-op UPDATE was bumping updated_at.)
func (ps *PublisherStore) SetAutoApprovePatch(ctx context.Context, publisherID uuid.UUID, autoApprove bool) (*Publisher, error) {
	if publisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	if autoApprove {
		// Pre-check so the caller gets the structured sentinel
		// rather than the CHECK violation surface (which would
		// land in the 500 fallthrough). The UPDATE below still
		// includes the verified_at guard so a concurrent
		// UnverifyPublisher between this read and the UPDATE is
		// caught by the SQL predicate.
		cur, err := ps.GetPublisher(ctx, publisherID)
		if err != nil {
			return nil, err
		}
		if cur.VerifiedAt == nil {
			return nil, fmt.Errorf("%w: cannot enable auto_approve_patch on unverified publisher", ErrPublisherNotVerified)
		}
	}
	tag, err := ps.store.pool.Exec(ctx,
		// AND (verified_at IS NOT NULL OR $2 = FALSE) is the SQL
		// mirror of the Go-side pre-check: enabling auto-approve
		// on an unverified row is refused atomically (concurrent
		// UnverifyPublisher race), but disabling auto-approve is
		// always permitted regardless of verification state.
		//
		// AND auto_approve_patch IS DISTINCT FROM $2 makes the
		// UPDATE a no-op when the column already equals the
		// requested value — preserves updated_at semantics
		// ("bumped only on meaningful change") which audit-trail
		// consumers rely on. The zero-rows-affected branch below
		// disambiguates no-op from the concurrent-unverify race.
		`UPDATE marketplace_publishers
		    SET auto_approve_patch = $2,
		        updated_at = now()
		  WHERE id = $1
		    AND (verified_at IS NOT NULL OR $2 = FALSE)
		    AND auto_approve_patch IS DISTINCT FROM $2`,
		publisherID, autoApprove,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: set auto_approve_patch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Three reasons the UPDATE matched zero rows:
		//   (a) publisher doesn't exist                  → ErrNotFound
		//   (b) verified→unverified race (autoApprove=true) → ErrPublisherNotVerified
		//   (c) auto_approve_patch already equals $2     → idempotent no-op
		// Disambiguate by re-reading the row.
		cur, err := ps.GetPublisher(ctx, publisherID)
		if err != nil {
			return nil, err
		}
		if autoApprove && cur.VerifiedAt == nil {
			return nil, fmt.Errorf("%w: publisher was unverified by a concurrent caller", ErrPublisherNotVerified)
		}
		// Idempotent no-op: the row already matches the desired
		// state. Return the freshly-read row WITHOUT a write so
		// updated_at is preserved.
		return cur, nil
	}
	return ps.GetPublisher(ctx, publisherID)
}

// UnverifyPublisher clears the verified_at / verified_by /
// auto_approve_patch columns. Used when an operator needs to
// re-verify a publisher (e.g. after a contact email change). The
// CHECK on auto_approve_requires_verified force-clears
// auto_approve_patch alongside verified_at.
func (ps *PublisherStore) UnverifyPublisher(ctx context.Context, publisherID uuid.UUID) error {
	if publisherID == uuid.Nil {
		return fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	tag, err := ps.store.pool.Exec(ctx,
		`UPDATE marketplace_publishers
		    SET verified_at = NULL,
		        verified_by = NULL,
		        verification_notes = NULL,
		        auto_approve_patch = FALSE,
		        updated_at = now()
		  WHERE id = $1`, publisherID,
	)
	if err != nil {
		return fmt.Errorf("marketplace: unverify publisher: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPublishers returns publishers ordered by slug. Used by the
// admin UI listing page.
func (ps *PublisherStore) ListPublishers(ctx context.Context) ([]Publisher, error) {
	rows, err := ps.store.pool.Query(ctx,
		`SELECT id, slug, display_name, contact_email,
		        verified_at, COALESCE(verified_by,''), COALESCE(verification_notes,''),
		        auto_approve_patch, created_at, updated_at
		 FROM marketplace_publishers
		 ORDER BY slug ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list publishers: %w", err)
	}
	defer rows.Close()
	out := make([]Publisher, 0, 32)
	for rows.Next() {
		var p Publisher
		var verifiedAt *time.Time
		var verifiedBy, verifyNotes string
		if err := rows.Scan(
			&p.ID, &p.Slug, &p.DisplayName, &p.ContactEmail,
			&verifiedAt, &verifiedBy, &verifyNotes,
			&p.AutoApprovePatch, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list publishers: scan: %w", err)
		}
		p.VerifiedAt = verifiedAt
		p.VerifiedBy = verifiedBy
		p.VerificationNotes = verifyNotes
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- Publisher keys ----------------------------------------------------

// RegisterPublisherKeyInput is the parameter block for
// RegisterPublisherKey.
type RegisterPublisherKeyInput struct {
	PublisherID  uuid.UUID
	KeyID        string // publisher-chosen id; the signature references this
	PublicKeyB64 string // 32-byte ed25519 public key, base64-standard encoded
	Label        string // optional human-readable label
}

// RegisterPublisherKey inserts a new key row. The DB CHECK on
// public_key_b64 enforces the 44-char/ed25519 base64 shape; the Go
// layer trims whitespace before insert so a paste from a CLI output
// (which often ends with a newline) doesn't fail the regex.
func (ps *PublisherStore) RegisterPublisherKey(ctx context.Context, in RegisterPublisherKeyInput) (*PublisherKey, error) {
	if in.PublisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	keyID := strings.TrimSpace(in.KeyID)
	pub := strings.TrimSpace(in.PublicKeyB64)
	if keyID == "" {
		return nil, fmt.Errorf("%w: key_id required", ErrInvalidManifest)
	}
	if pub == "" {
		return nil, fmt.Errorf("%w: public_key_b64 required", ErrInvalidManifest)
	}
	var id uuid.UUID
	err := ps.store.pool.QueryRow(ctx,
		`INSERT INTO marketplace_publisher_keys (publisher_id, key_id, algorithm, public_key_b64, label)
		 VALUES ($1, $2, 'ed25519', $3, NULLIF($4,''))
		 RETURNING id`,
		in.PublisherID, keyID, pub, in.Label,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("marketplace: register publisher key: %w", err)
	}
	return ps.GetPublisherKey(ctx, id)
}

// GetPublisherKey loads a key by ID.
func (ps *PublisherStore) GetPublisherKey(ctx context.Context, id uuid.UUID) (*PublisherKey, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: key id required", ErrNotFound)
	}
	var out PublisherKey
	var label, revokedReason string
	var revokedAt *time.Time
	err := ps.store.pool.QueryRow(ctx,
		`SELECT id, publisher_id, key_id, algorithm, public_key_b64,
		        COALESCE(label,''), revoked_at, COALESCE(revoked_reason,''), created_at
		 FROM marketplace_publisher_keys WHERE id = $1`, id,
	).Scan(
		&out.ID, &out.PublisherID, &out.KeyID, &out.Algorithm, &out.PublicKeyB64,
		&label, &revokedAt, &revokedReason, &out.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get publisher key: %w", err)
	}
	out.Label = label
	out.RevokedAt = revokedAt
	out.RevokedReason = revokedReason
	return &out, nil
}

// ListPublisherKeys returns every key for a publisher (revoked or
// not). includeRevoked controls whether revoked keys are returned;
// callers wanting only the active set pass false.
//
// The pipeline's SignatureCheck calls ListPublisherKeys with
// includeRevoked=false to gate its signing-required policy.
func (ps *PublisherStore) ListPublisherKeys(ctx context.Context, publisherID uuid.UUID, includeRevoked bool) ([]PublisherKey, error) {
	if publisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	q := `SELECT id, publisher_id, key_id, algorithm, public_key_b64,
	             COALESCE(label,''), revoked_at, COALESCE(revoked_reason,''), created_at
	      FROM marketplace_publisher_keys
	      WHERE publisher_id = $1`
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	q += ` ORDER BY created_at ASC`
	rows, err := ps.store.pool.Query(ctx, q, publisherID)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list publisher keys: %w", err)
	}
	defer rows.Close()
	out := make([]PublisherKey, 0, 4)
	for rows.Next() {
		var k PublisherKey
		var label, revokedReason string
		var revokedAt *time.Time
		if err := rows.Scan(
			&k.ID, &k.PublisherID, &k.KeyID, &k.Algorithm, &k.PublicKeyB64,
			&label, &revokedAt, &revokedReason, &k.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list publisher keys: scan: %w", err)
		}
		k.Label = label
		k.RevokedAt = revokedAt
		k.RevokedReason = revokedReason
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokePublisherKey marks a key as revoked. The DB CHECK requires
// (revoked_at, revoked_reason) to be set together; the Go layer
// passes time.Now() and forwards the reason. Revoking an already-
// revoked key is idempotent (no error, no change).
func (ps *PublisherStore) RevokePublisherKey(ctx context.Context, keyID uuid.UUID, reason string) error {
	if keyID == uuid.Nil {
		return fmt.Errorf("%w: key id required", ErrNotFound)
	}
	r := strings.TrimSpace(reason)
	if r == "" {
		return fmt.Errorf("%w: revoke reason required", ErrInvalidManifest)
	}
	tag, err := ps.store.pool.Exec(ctx,
		`UPDATE marketplace_publisher_keys
		    SET revoked_at = COALESCE(revoked_at, now()),
		        revoked_reason = COALESCE(revoked_reason, $2)
		  WHERE id = $1`,
		keyID, r,
	)
	if err != nil {
		return fmt.Errorf("marketplace: revoke publisher key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
