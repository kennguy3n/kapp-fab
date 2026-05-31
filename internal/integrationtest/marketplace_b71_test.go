//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestMarketplacePublisherMembers_AddListGetRequire is the happy-path
// exercise of the B7.1 self-service membership surface against a
// live PG instance.
//
// Asserts:
//
//  1. AddMember on a fresh publisher records the (publisher, user, role)
//     row and joins users.email + users.display_name via GetMember.
//  2. AddMember with the same (publisher, user) pair returns ErrConflict
//     (PRIMARY KEY violation translated by the store).
//  3. ListMembers returns owners first, then members, then stable
//     ascending order by created_at.
//  4. ListPublishersForUser returns every publisher the user is a
//     member of, paired with that user's role.
//  5. RequireMemberRole accepts a sufficient role and rejects an
//     insufficient one with ErrForbidden (a member cannot pass
//     an owner-only gate).
//  6. RequireMemberRole returns ErrForbidden (not ErrNotFound) for a
//     user with no membership row — the handler layer is
//     responsible for collapsing that to 404 on self-service
//     surfaces.
func TestMarketplacePublisherMembers_AddListGetRequire(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	pubA := mustCreatePublisher(t, ctx, pubs, "b71_owner_a")
	pubB := mustCreatePublisher(t, ctx, pubs, "b71_owner_b")

	alice := mustCreateUser(t, ctx, h, "alice")
	bob := mustCreateUser(t, ctx, h, "bob")
	carol := mustCreateUser(t, ctx, h, "carol")

	// (1) AddMember success — Alice owns pubA, added by herself
	// (in the test we pass uuid.Nil because there is no
	// "inviting owner" yet for the bootstrap row; the admin
	// chain leaves added_by NULL).
	added, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pubA.ID,
		UserID:      alice.ID,
		Role:        marketplace.PublisherMemberRoleOwner,
	})
	if err != nil {
		t.Fatalf("AddMember(alice, owner): %v", err)
	}
	if added.Role != marketplace.PublisherMemberRoleOwner {
		t.Fatalf("AddMember stored wrong role: %q", added.Role)
	}
	if added.AddedBy != nil {
		t.Errorf("admin-added membership should have nil added_by, got %v", added.AddedBy)
	}
	if added.UserEmail != alice.Email {
		t.Errorf("GetMember-via-AddMember JOIN: want email %q, got %q",
			alice.Email, added.UserEmail)
	}

	// (2) AddMember conflict.
	if _, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pubA.ID,
		UserID:      alice.ID,
		Role:        marketplace.PublisherMemberRoleMember,
	}); !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("duplicate AddMember should return ErrConflict, got %v", err)
	}

	// Now add Bob as a member of pubA (added by Alice), Carol as
	// a member of pubA (also added by Alice), and Alice as an
	// owner of pubB so we have a multi-publisher test setup.
	if _, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pubA.ID,
		UserID:      bob.ID,
		Role:        marketplace.PublisherMemberRoleMember,
		AddedBy:     alice.ID,
	}); err != nil {
		t.Fatalf("AddMember(bob, member): %v", err)
	}
	if _, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pubA.ID,
		UserID:      carol.ID,
		Role:        marketplace.PublisherMemberRoleMember,
		AddedBy:     alice.ID,
	}); err != nil {
		t.Fatalf("AddMember(carol, member): %v", err)
	}
	if _, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pubB.ID,
		UserID:      alice.ID,
		Role:        marketplace.PublisherMemberRoleOwner,
	}); err != nil {
		t.Fatalf("AddMember(alice, pubB owner): %v", err)
	}

	// (3) ListMembers ordering — owners first, then members.
	members, err := pubs.ListMembers(ctx, pubA.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("ListMembers: want 3 rows, got %d", len(members))
	}
	if members[0].UserID != alice.ID || members[0].Role != marketplace.PublisherMemberRoleOwner {
		t.Errorf("ListMembers[0] should be owner alice, got %+v", members[0])
	}
	// Bob and Carol are both members; their order is by created_at
	// ASC then user_id ASC — Bob was inserted before Carol so we
	// expect bob then carol regardless of UUID comparison.
	if members[1].UserID != bob.ID || members[2].UserID != carol.ID {
		t.Errorf("ListMembers order wrong: [1]=%v [2]=%v want [bob, carol]",
			members[1].UserID, members[2].UserID)
	}
	// Bob was added by Alice — the join populates added_by.
	if members[1].AddedBy == nil || *members[1].AddedBy != alice.ID {
		t.Errorf("Bob's added_by should be alice, got %v", members[1].AddedBy)
	}

	// (4) ListPublishersForUser — Alice is a member of both pubA
	// and pubB, ordered by slug ASC.
	aliceRows, err := pubs.ListPublishersForUser(ctx, alice.ID)
	if err != nil {
		t.Fatalf("ListPublishersForUser(alice): %v", err)
	}
	if len(aliceRows) != 2 {
		t.Fatalf("ListPublishersForUser: want 2 rows, got %d", len(aliceRows))
	}
	// pubA's slug starts with b71_owner_a, pubB's with b71_owner_b — so
	// pubA comes first regardless of any post-prefix UUID jitter.
	if aliceRows[0].Publisher.ID != pubA.ID || aliceRows[1].Publisher.ID != pubB.ID {
		t.Errorf("ListPublishersForUser order wrong (want pubA then pubB by slug ASC), got %+v",
			[]uuid.UUID{aliceRows[0].Publisher.ID, aliceRows[1].Publisher.ID})
	}
	if aliceRows[0].Role != marketplace.PublisherMemberRoleOwner {
		t.Errorf("alice's role on pubA should be owner, got %q", aliceRows[0].Role)
	}

	// (5) RequireMemberRole — Alice can pass an owner gate on
	// pubA, Bob cannot.
	if _, err := pubs.RequireMemberRole(ctx, pubA.ID, alice.ID, marketplace.PublisherMemberRoleOwner); err != nil {
		t.Errorf("alice should pass owner-required gate on pubA: %v", err)
	}
	if _, err := pubs.RequireMemberRole(ctx, pubA.ID, bob.ID, marketplace.PublisherMemberRoleOwner); !errors.Is(err, marketplace.ErrForbidden) {
		t.Errorf("bob should be ErrForbidden on owner gate, got %v", err)
	}
	// Bob can still pass a member gate.
	if _, err := pubs.RequireMemberRole(ctx, pubA.ID, bob.ID, marketplace.PublisherMemberRoleMember); err != nil {
		t.Errorf("bob should pass member-required gate on pubA: %v", err)
	}

	// (6) RequireMemberRole for non-member.
	dave := mustCreateUser(t, ctx, h, "dave")
	if _, err := pubs.RequireMemberRole(ctx, pubA.ID, dave.ID, marketplace.PublisherMemberRoleMember); !errors.Is(err, marketplace.ErrForbidden) {
		t.Errorf("non-member should be ErrForbidden, got %v", err)
	}
}

// TestMarketplacePublisherMembers_LastOwnerInvariant pins the
// "any publisher with members must have ≥1 owner" rule.
//
// Scenarios:
//
//   - SetMemberRole(only-owner → member) when other (non-owner)
//     members exist → ErrLastOwnerRemoval.
//   - RemoveMember(only-owner) when other members exist → same.
//   - RemoveMember(only-owner) when NO other members exist →
//     succeeds (publisher reverts to admin-only management).
//   - SetMemberRole / RemoveMember with AllowLastOwner* = true
//     bypasses the guard (admin-override path).
//   - Promoting another member to owner first, then demoting the
//     original owner, succeeds.
func TestMarketplacePublisherMembers_LastOwnerInvariant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	pub := mustCreatePublisher(t, ctx, pubs, "b71_lastowner")
	alice := mustCreateUser(t, ctx, h, "lastowner_alice")
	bob := mustCreateUser(t, ctx, h, "lastowner_bob")

	// Setup: alice is owner, bob is member.
	if _, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
		Role:        marketplace.PublisherMemberRoleOwner,
	}); err != nil {
		t.Fatalf("seed alice as owner: %v", err)
	}
	if _, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pub.ID,
		UserID:      bob.ID,
		Role:        marketplace.PublisherMemberRoleMember,
	}); err != nil {
		t.Fatalf("seed bob as member: %v", err)
	}

	// (a) Demote the only owner — refused because bob (member)
	// remains.
	if _, err := pubs.SetMemberRole(ctx, marketplace.SetMemberRoleInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
		NewRole:     marketplace.PublisherMemberRoleMember,
	}); !errors.Is(err, marketplace.ErrLastOwnerRemoval) {
		t.Errorf("demoting only owner with other members should be ErrLastOwnerRemoval, got %v", err)
	}

	// (b) Remove the only owner — same refusal.
	if err := pubs.RemoveMember(ctx, marketplace.RemoveMemberInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
	}); !errors.Is(err, marketplace.ErrLastOwnerRemoval) {
		t.Errorf("removing only owner with other members should be ErrLastOwnerRemoval, got %v", err)
	}

	// (c) Promote bob to owner — now demote alice, which should
	// succeed because bob is still an owner.
	if _, err := pubs.SetMemberRole(ctx, marketplace.SetMemberRoleInput{
		PublisherID: pub.ID,
		UserID:      bob.ID,
		NewRole:     marketplace.PublisherMemberRoleOwner,
	}); err != nil {
		t.Fatalf("promote bob to owner: %v", err)
	}
	if _, err := pubs.SetMemberRole(ctx, marketplace.SetMemberRoleInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
		NewRole:     marketplace.PublisherMemberRoleMember,
	}); err != nil {
		t.Fatalf("demote alice (bob still owner): %v", err)
	}

	// (d) Remove the sole remaining member (bob is owner, alice is
	// member). Removing bob: alice would remain as a non-owner —
	// invariant violated.
	if err := pubs.RemoveMember(ctx, marketplace.RemoveMemberInput{
		PublisherID: pub.ID,
		UserID:      bob.ID,
	}); !errors.Is(err, marketplace.ErrLastOwnerRemoval) {
		t.Errorf("removing bob (sole owner) with alice still a member should refuse, got %v", err)
	}

	// (e) Admin override — same call with AllowLastOwnerRemoval=true
	// succeeds.
	if err := pubs.RemoveMember(ctx, marketplace.RemoveMemberInput{
		PublisherID:           pub.ID,
		UserID:                bob.ID,
		AllowLastOwnerRemoval: true,
	}); err != nil {
		t.Fatalf("admin override RemoveMember should succeed: %v", err)
	}
	// After admin override, only alice remains (as member). She
	// can be removed normally — publisher reverts to admin-only.
	if err := pubs.RemoveMember(ctx, marketplace.RemoveMemberInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
	}); err != nil {
		t.Fatalf("remove sole remaining (non-owner) member should succeed: %v", err)
	}

	// (f) Verify the publisher is now member-less.
	left, err := pubs.ListMembers(ctx, pub.ID)
	if err != nil {
		t.Fatalf("ListMembers final: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("expected empty membership after teardown, got %d members", len(left))
	}
}

// TestMarketplacePublisherMembers_SetRole_Idempotent verifies that
// SetMemberRole with the same role the user already holds is a
// no-op (does not UPDATE the row, does not bump updated_at). This
// matches the pattern SetAutoApprovePatch already uses — see
// publisher_store.go for the rationale on why we guard no-op
// UPDATEs.
func TestMarketplacePublisherMembers_SetRole_Idempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	pub := mustCreatePublisher(t, ctx, pubs, "b71_idem")
	alice := mustCreateUser(t, ctx, h, "idem_alice")

	added, err := pubs.AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
		Role:        marketplace.PublisherMemberRoleOwner,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	firstUpdated := added.UpdatedAt

	// Set the same role.
	resaved, err := pubs.SetMemberRole(ctx, marketplace.SetMemberRoleInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
		NewRole:     marketplace.PublisherMemberRoleOwner,
	})
	if err != nil {
		t.Fatalf("SetMemberRole no-op: %v", err)
	}
	if !resaved.UpdatedAt.Equal(firstUpdated) {
		t.Errorf("no-op SetMemberRole should not bump updated_at: first=%v second=%v",
			firstUpdated, resaved.UpdatedAt)
	}
}

// TestMarketplacePublisherMembers_RoleAtLeast pins the role
// comparator's ordering: owner ≥ member, owner ≥ owner, member ≥
// member, member ≱ owner. The handler layer uses this to gate
// owner-only endpoints without making it possible to accidentally
// invert the comparison.
func TestMarketplacePublisherMembers_RoleAtLeast(t *testing.T) {
	cases := []struct {
		have   marketplace.PublisherMemberRole
		need   marketplace.PublisherMemberRole
		expect bool
	}{
		{marketplace.PublisherMemberRoleOwner, marketplace.PublisherMemberRoleOwner, true},
		{marketplace.PublisherMemberRoleOwner, marketplace.PublisherMemberRoleMember, true},
		{marketplace.PublisherMemberRoleMember, marketplace.PublisherMemberRoleMember, true},
		{marketplace.PublisherMemberRoleMember, marketplace.PublisherMemberRoleOwner, false},
	}
	for _, c := range cases {
		if got := c.have.AtLeast(c.need); got != c.expect {
			t.Errorf("AtLeast(have=%q, need=%q) = %v, want %v",
				c.have, c.need, got, c.expect)
		}
	}
}

// --- Helpers --------------------------------------------------

// mustCreatePublisher creates a publisher with a unique slug derived
// from the prefix. Slugs must match the strict regex
// ^[a-z][a-z0-9_]{2,31}$ (see migration 000073), so we replace
// uniqueSlug's '-' with '_'.
func mustCreatePublisher(t *testing.T, ctx context.Context, pubs *marketplace.PublisherStore, prefix string) *marketplace.Publisher {
	t.Helper()
	slug := strings.ReplaceAll(uniqueSlug(prefix), "-", "_")
	pub, err := pubs.CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         slug,
		DisplayName:  prefix + " test publisher",
		ContactEmail: slug + "@example.invalid",
	})
	if err != nil {
		t.Fatalf("CreatePublisher(%q): %v", slug, err)
	}
	return pub
}

// mustCreateUser creates a global users row. The KChatUserID must
// be unique; we derive it from the role + a UUID suffix.
func mustCreateUser(t *testing.T, ctx context.Context, h *harness, role string) *tenant.User {
	t.Helper()
	u, err := h.users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-" + role + "-" + uuid.NewString()[:8],
		Email:       role + "@b71.test",
		DisplayName: strings.ToUpper(role[:1]) + role[1:],
	})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", role, err)
	}
	return u
}
