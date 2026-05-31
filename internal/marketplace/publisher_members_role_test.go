package marketplace

import "testing"

// Pure-Go unit tests for PublisherMemberRole.Valid + AtLeast. Lives
// alongside the store so it runs as part of the standard go test
// pass (no integration build tag). The DB-backed lifecycle tests
// for the store methods themselves live in
// internal/integrationtest/marketplace_b71_test.go.

func TestPublisherMemberRole_Valid(t *testing.T) {
	cases := []struct {
		role  PublisherMemberRole
		valid bool
	}{
		{PublisherMemberRoleOwner, true},
		{PublisherMemberRoleMember, true},
		{PublisherMemberRole(""), false},
		{PublisherMemberRole("admin"), false},
		{PublisherMemberRole("OWNER"), false}, // case-sensitive
	}
	for _, c := range cases {
		if got := c.role.Valid(); got != c.valid {
			t.Errorf("(%q).Valid() = %v, want %v", c.role, got, c.valid)
		}
	}
}

func TestPublisherMemberRole_AtLeast(t *testing.T) {
	cases := []struct {
		have   PublisherMemberRole
		need   PublisherMemberRole
		expect bool
	}{
		// owner >= everything
		{PublisherMemberRoleOwner, PublisherMemberRoleOwner, true},
		{PublisherMemberRoleOwner, PublisherMemberRoleMember, true},

		// member >= member only
		{PublisherMemberRoleMember, PublisherMemberRoleMember, true},
		{PublisherMemberRoleMember, PublisherMemberRoleOwner, false},

		// unknown roles rank 0; only >= other unknown rank-0
		// values would pass, and we never construct one in
		// practice. Defensive: the zero value is "not at
		// least anything valid".
		{PublisherMemberRole(""), PublisherMemberRoleMember, false},
		{PublisherMemberRole(""), PublisherMemberRoleOwner, false},
	}
	for _, c := range cases {
		if got := c.have.AtLeast(c.need); got != c.expect {
			t.Errorf("(%q).AtLeast(%q) = %v, want %v",
				c.have, c.need, got, c.expect)
		}
	}
}
