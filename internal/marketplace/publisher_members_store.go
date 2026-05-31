package marketplace

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// publisher_members_store.go — store methods for the B7.1
// publisher membership join table (marketplace_publisher_members).
//
// All methods on PublisherStore so the existing
// Store.Publishers() accessor surfaces them alongside the
// publisher + key methods. The invariant "any publisher with
// members must have ≥1 owner" is enforced inside a tx with
// SELECT … FOR UPDATE on the publisher row (rationale lives in
// migrations/000075).

// AddPublisherMemberInput is the parameter struct for AddMember.
// AddedBy is optional — set to uuid.Nil if the membership is
// being seeded by the admin chain (the operator acts on behalf
// of the platform and is not themselves a publisher member).
type AddPublisherMemberInput struct {
	PublisherID uuid.UUID
	UserID      uuid.UUID
	Role        PublisherMemberRole
	AddedBy     uuid.UUID
}

// AddMember inserts a (publisher, user, role) row. Returns
// ErrConflict if the user is already a member of this publisher
// (callers should use SetMemberRole to change an existing
// member's role instead).
//
// AddMember itself does NOT check whether the caller is allowed
// to add a member — that gate lives in the handler layer via
// RequireMemberRole(owner). The store enforces only the
// uniqueness invariant.
func (ps *PublisherStore) AddMember(ctx context.Context, in AddPublisherMemberInput) (*PublisherMember, error) {
	if in.PublisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	if in.UserID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id required", ErrInvalidManifest)
	}
	if !in.Role.Valid() {
		return nil, fmt.Errorf("%w: role must be 'owner' or 'member'", ErrInvalidManifest)
	}
	var addedBy any
	if in.AddedBy != uuid.Nil {
		addedBy = in.AddedBy
	}
	_, err := ps.store.pool.Exec(ctx,
		`INSERT INTO marketplace_publisher_members
		    (publisher_id, user_id, role, added_by)
		 VALUES ($1, $2, $3, $4)`,
		in.PublisherID, in.UserID, string(in.Role), addedBy,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("marketplace: add publisher member: %w", err)
	}
	return ps.GetMember(ctx, in.PublisherID, in.UserID)
}

// GetMember loads one membership row by (publisher, user). The
// returned struct includes the joined users.email +
// users.display_name fields so the API layer can render member
// rows without a second round-trip. ErrNotFound if no row
// exists.
func (ps *PublisherStore) GetMember(ctx context.Context, publisherID, userID uuid.UUID) (*PublisherMember, error) {
	if publisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id required", ErrNotFound)
	}
	var out PublisherMember
	var role string
	var addedBy *uuid.UUID
	var email, displayName *string
	err := ps.store.pool.QueryRow(ctx,
		`SELECT m.publisher_id, m.user_id, m.role, m.added_by,
		        m.created_at, m.updated_at, u.email, u.display_name
		   FROM marketplace_publisher_members m
		   JOIN users u ON u.id = m.user_id
		  WHERE m.publisher_id = $1 AND m.user_id = $2`,
		publisherID, userID,
	).Scan(
		&out.PublisherID, &out.UserID, &role, &addedBy,
		&out.CreatedAt, &out.UpdatedAt, &email, &displayName,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get publisher member: %w", err)
	}
	out.Role = PublisherMemberRole(role)
	out.AddedBy = addedBy
	if email != nil {
		out.UserEmail = *email
	}
	if displayName != nil {
		out.UserDisplayName = *displayName
	}
	return &out, nil
}

// ListMembers returns every member of a publisher, ordered by
// role then created_at (owners listed first). Joins users for
// email + display_name.
func (ps *PublisherStore) ListMembers(ctx context.Context, publisherID uuid.UUID) ([]PublisherMember, error) {
	if publisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	rows, err := ps.store.pool.Query(ctx,
		`SELECT m.publisher_id, m.user_id, m.role, m.added_by,
		        m.created_at, m.updated_at, u.email, u.display_name
		   FROM marketplace_publisher_members m
		   JOIN users u ON u.id = m.user_id
		  WHERE m.publisher_id = $1
		  ORDER BY CASE m.role WHEN 'owner' THEN 0 ELSE 1 END,
		           m.created_at ASC, m.user_id ASC`,
		publisherID,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list publisher members: %w", err)
	}
	defer rows.Close()
	out := make([]PublisherMember, 0, 8)
	for rows.Next() {
		var m PublisherMember
		var role string
		var addedBy *uuid.UUID
		var email, displayName *string
		if err := rows.Scan(
			&m.PublisherID, &m.UserID, &role, &addedBy,
			&m.CreatedAt, &m.UpdatedAt, &email, &displayName,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list publisher members: scan: %w", err)
		}
		m.Role = PublisherMemberRole(role)
		m.AddedBy = addedBy
		if email != nil {
			m.UserEmail = *email
		}
		if displayName != nil {
			m.UserDisplayName = *displayName
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListPublishersForUser returns the publishers that the given
// user is a member of, paired with that user's role. Used by the
// "GET /api/v1/publisher" self-service index so the caller can
// see every publisher they can manage. Ordered by publisher slug
// for a stable UI.
func (ps *PublisherStore) ListPublishersForUser(ctx context.Context, userID uuid.UUID) ([]PublisherWithMembership, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id required", ErrNotFound)
	}
	rows, err := ps.store.pool.Query(ctx,
		`SELECT p.id, p.slug, p.display_name, p.contact_email,
		        p.verified_at, COALESCE(p.verified_by,''),
		        COALESCE(p.verification_notes,''),
		        p.auto_approve_patch, p.created_at, p.updated_at,
		        m.role
		   FROM marketplace_publisher_members m
		   JOIN marketplace_publishers p ON p.id = m.publisher_id
		  WHERE m.user_id = $1
		  ORDER BY p.slug ASC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list publishers for user: %w", err)
	}
	defer rows.Close()
	out := make([]PublisherWithMembership, 0, 4)
	for rows.Next() {
		var row PublisherWithMembership
		var role string
		if err := rows.Scan(
			&row.Publisher.ID, &row.Publisher.Slug, &row.Publisher.DisplayName,
			&row.Publisher.ContactEmail, &row.Publisher.VerifiedAt,
			&row.Publisher.VerifiedBy, &row.Publisher.VerificationNotes,
			&row.Publisher.AutoApprovePatch, &row.Publisher.CreatedAt,
			&row.Publisher.UpdatedAt, &role,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list publishers for user: scan: %w", err)
		}
		row.Role = PublisherMemberRole(role)
		out = append(out, row)
	}
	return out, rows.Err()
}

// RequireMemberRole returns the caller's role on the publisher if
// they are a member AND their role is at least minRole. Used by
// the handler layer to gate self-service endpoints:
//
//	role, err := store.Publishers().RequireMemberRole(ctx, pubID, userID, marketplace.PublisherMemberRoleOwner)
//	if err != nil { writeError(...); return }
//
// Returns ErrForbidden if the user is not a member OR their role
// is insufficient. Returns ErrNotFound if publisherID / userID is
// uuid.Nil. The caller can use errors.Is to distinguish the two
// states — the HTTP layer collapses both into 404 on the
// self-service surface to avoid leaking publisher existence to
// non-members.
func (ps *PublisherStore) RequireMemberRole(
	ctx context.Context,
	publisherID, userID uuid.UUID,
	minRole PublisherMemberRole,
) (PublisherMemberRole, error) {
	if publisherID == uuid.Nil {
		return "", fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	if userID == uuid.Nil {
		return "", fmt.Errorf("%w: user id required", ErrNotFound)
	}
	if !minRole.Valid() {
		return "", fmt.Errorf("%w: minimum role must be 'owner' or 'member'", ErrInvalidManifest)
	}
	var role string
	err := ps.store.pool.QueryRow(ctx,
		`SELECT role FROM marketplace_publisher_members
		  WHERE publisher_id = $1 AND user_id = $2`,
		publisherID, userID,
	).Scan(&role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrForbidden
		}
		return "", fmt.Errorf("marketplace: require member role: %w", err)
	}
	have := PublisherMemberRole(role)
	if !have.AtLeast(minRole) {
		return have, ErrForbidden
	}
	return have, nil
}

// SetMemberRoleInput is the parameter struct for SetMemberRole.
// AllowLastOwnerDemotion is the admin-override escape hatch — set
// it true when calling from the admin chain (operator forcibly
// reorganising publisher membership). The self-service surface
// always passes false.
type SetMemberRoleInput struct {
	PublisherID            uuid.UUID
	UserID                 uuid.UUID
	NewRole                PublisherMemberRole
	AllowLastOwnerDemotion bool
}

// SetMemberRole updates an existing member's role. ErrNotFound
// if no membership row exists. ErrLastOwnerRemoval if demoting
// the last owner would leave other (non-owner) members behind
// without an owner — unless AllowLastOwnerDemotion is true.
//
// Runs inside a tx with SELECT … FOR UPDATE on the publisher row
// to serialise concurrent role changes against the
// last-owner-removal check.
func (ps *PublisherStore) SetMemberRole(ctx context.Context, in SetMemberRoleInput) (*PublisherMember, error) {
	if in.PublisherID == uuid.Nil {
		return nil, fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	if in.UserID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id required", ErrInvalidManifest)
	}
	if !in.NewRole.Valid() {
		return nil, fmt.Errorf("%w: role must be 'owner' or 'member'", ErrInvalidManifest)
	}
	tx, err := ps.store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("marketplace: set member role: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the publisher row. The lock orders concurrent role
	// changes; readers don't block, but two operators trying to
	// demote the last owner from different sessions are
	// serialised so only one sees the "≥1 owner" check passing.
	var pubID uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM marketplace_publishers WHERE id = $1 FOR UPDATE`,
		in.PublisherID,
	).Scan(&pubID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: set member role: lock publisher: %w", err)
	}

	var currentRole string
	if err := tx.QueryRow(ctx,
		`SELECT role FROM marketplace_publisher_members
		  WHERE publisher_id = $1 AND user_id = $2`,
		in.PublisherID, in.UserID,
	).Scan(&currentRole); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: set member role: read current: %w", err)
	}
	if PublisherMemberRole(currentRole) == in.NewRole {
		// No-op. Re-read and return so the caller gets the
		// joined struct without an unnecessary UPDATE bumping
		// updated_at for nothing.
		return ps.GetMember(ctx, in.PublisherID, in.UserID)
	}

	// Last-owner-demotion guard. Only relevant when demoting
	// (owner → member). If the publisher has any non-owner
	// members AND this is the last owner, refuse — unless the
	// admin override is set.
	if PublisherMemberRole(currentRole) == PublisherMemberRoleOwner &&
		in.NewRole != PublisherMemberRoleOwner &&
		!in.AllowLastOwnerDemotion {
		if err := assertNotLastOwner(ctx, tx, in.PublisherID, in.UserID); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE marketplace_publisher_members
		    SET role = $3
		  WHERE publisher_id = $1 AND user_id = $2`,
		in.PublisherID, in.UserID, string(in.NewRole),
	); err != nil {
		return nil, fmt.Errorf("marketplace: set member role: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("marketplace: set member role: commit: %w", err)
	}
	return ps.GetMember(ctx, in.PublisherID, in.UserID)
}

// RemoveMemberInput parameters for RemoveMember. AllowLastOwnerRemoval
// is the admin override — when true, the last-owner invariant is
// not enforced. Self-service callers always pass false.
type RemoveMemberInput struct {
	PublisherID            uuid.UUID
	UserID                 uuid.UUID
	AllowLastOwnerRemoval  bool
}

// RemoveMember deletes a (publisher, user) row. ErrNotFound if
// no row exists. ErrLastOwnerRemoval if removing the row would
// leave other (non-owner) members behind without an owner —
// unless AllowLastOwnerRemoval is true. Removing the sole
// remaining member (whatever their role) succeeds: the
// publisher reverts to admin-only management.
//
// Runs inside a tx with SELECT … FOR UPDATE on the publisher row
// for the same reason as SetMemberRole.
func (ps *PublisherStore) RemoveMember(ctx context.Context, in RemoveMemberInput) error {
	if in.PublisherID == uuid.Nil {
		return fmt.Errorf("%w: publisher id required", ErrNotFound)
	}
	if in.UserID == uuid.Nil {
		return fmt.Errorf("%w: user id required", ErrInvalidManifest)
	}
	tx, err := ps.store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("marketplace: remove member: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var pubID uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM marketplace_publishers WHERE id = $1 FOR UPDATE`,
		in.PublisherID,
	).Scan(&pubID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("marketplace: remove member: lock publisher: %w", err)
	}

	var currentRole string
	if err := tx.QueryRow(ctx,
		`SELECT role FROM marketplace_publisher_members
		  WHERE publisher_id = $1 AND user_id = $2`,
		in.PublisherID, in.UserID,
	).Scan(&currentRole); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("marketplace: remove member: read current: %w", err)
	}

	if PublisherMemberRole(currentRole) == PublisherMemberRoleOwner && !in.AllowLastOwnerRemoval {
		if err := assertNotLastOwner(ctx, tx, in.PublisherID, in.UserID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM marketplace_publisher_members
		  WHERE publisher_id = $1 AND user_id = $2`,
		in.PublisherID, in.UserID,
	); err != nil {
		return fmt.Errorf("marketplace: remove member: delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("marketplace: remove member: commit: %w", err)
	}
	return nil
}

// assertNotLastOwner is the shared "≥1 owner left after removal"
// check used by SetMemberRole(demote) and RemoveMember(owner).
// Counts the OTHER owners (i.e. excluding the user being
// demoted/removed) and the OTHER non-owner members. The
// invariant: if any non-owner members remain after the change,
// at least one other owner must also remain. If 0 members would
// remain in total, the operation is allowed (publisher reverts
// to admin-only management).
//
// Runs inside the caller's tx with the publisher row already
// locked FOR UPDATE so concurrent demotions / removals are
// serialised.
func assertNotLastOwner(ctx context.Context, tx pgx.Tx, publisherID, userID uuid.UUID) error {
	var otherOwners, otherMembers int
	if err := tx.QueryRow(ctx,
		`SELECT
		   COUNT(*) FILTER (WHERE role = 'owner') AS other_owners,
		   COUNT(*) FILTER (WHERE role <> 'owner') AS other_members
		 FROM marketplace_publisher_members
		 WHERE publisher_id = $1 AND user_id <> $2`,
		publisherID, userID,
	).Scan(&otherOwners, &otherMembers); err != nil {
		return fmt.Errorf("marketplace: assert not last owner: %w", err)
	}
	if otherOwners == 0 && otherMembers > 0 {
		return ErrLastOwnerRemoval
	}
	return nil
}
