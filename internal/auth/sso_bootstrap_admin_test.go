package auth

import "testing"

// TestBootstrapAdmin_MatchesKChatID locks in the contract that
// KAPP_PLATFORM_ADMIN_USERS is keyed on KChat user identifiers
// (opaque SSO-provider strings), NOT Kapp internal UUIDs. The
// expected-bootstrap branch of upsertUser depends on this — keying
// on the Kapp UUID would force a two-step "first login → look up
// UUID → set env var → log in again" workflow and would make the
// wasInsert=true INFO log path unreachable in practice.
//
// Each row exercises one slice of the surface area:
//   - exact match on a single ID
//   - whitespace tolerance (operators paste lists with spaces)
//   - comma-separated lists with multiple admins
//   - empty entries silently ignored (trailing comma typo)
//   - case sensitivity (KChat IDs are opaque, not normalised)
//   - empty kchat id returns false even with non-empty env var
//   - non-matching id returns false (the negative path operators
//     rely on when retiring the env var)
func TestBootstrapAdmin_MatchesKChatID(t *testing.T) {
	cases := []struct {
		name    string
		envVar  string
		kchatID string
		want    bool
	}{
		{
			name:    "exact match",
			envVar:  "kchat-user-abc",
			kchatID: "kchat-user-abc",
			want:    true,
		},
		{
			name:    "whitespace trimmed",
			envVar:  "  kchat-user-abc  ",
			kchatID: "kchat-user-abc",
			want:    true,
		},
		{
			name:    "multiple admins, second matches",
			envVar:  "kchat-user-abc, kchat-user-def, kchat-user-ghi",
			kchatID: "kchat-user-def",
			want:    true,
		},
		{
			name:    "trailing comma tolerated",
			envVar:  "kchat-user-abc,",
			kchatID: "kchat-user-abc",
			want:    true,
		},
		{
			name:    "case sensitive non-match",
			envVar:  "kchat-user-abc",
			kchatID: "KCHAT-USER-ABC",
			want:    false,
		},
		{
			name:    "non-matching id",
			envVar:  "kchat-user-abc",
			kchatID: "kchat-user-xyz",
			want:    false,
		},
		{
			name:    "empty kchat id returns false",
			envVar:  "kchat-user-abc",
			kchatID: "",
			want:    false,
		},
		{
			name:    "empty env var returns false",
			envVar:  "",
			kchatID: "kchat-user-abc",
			want:    false,
		},
		{
			name:    "env var only commas returns false",
			envVar:  ", , ,",
			kchatID: "kchat-user-abc",
			want:    false,
		},
	}
	s := &SSOService{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAPP_PLATFORM_ADMIN_USERS", tc.envVar)
			got := s.bootstrapAdmin(tc.kchatID)
			if got != tc.want {
				t.Fatalf("bootstrapAdmin(%q) with env=%q = %v, want %v", tc.kchatID, tc.envVar, got, tc.want)
			}
		})
	}
}
