package authz

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestMatchAction(t *testing.T) {
	cases := []struct {
		pattern string
		action  string
		want    bool
	}{
		// Catch-all wildcard.
		{"*", "anything.read", true},
		{"*", "finance.invoice.write", true},
		{"*", "", true},
		// Namespaced wildcard.
		{"finance.*", "finance.invoice.write", true},
		{"finance.*", "finance.account.read", true},
		{"finance.*", "hr.employee.read", false},
		{"krecord.*", "krecord.read", true},
		{"krecord.*", "krecord.write", true},
		{"krecord.*", "krecord", false},
		// Exact match.
		{"finance.invoice.write", "finance.invoice.write", true},
		{"finance.invoice.write", "finance.invoice.read", false},
		// Bare prefix without ".*" must not match a deeper namespace.
		{"finance", "finance.invoice.write", false},
		// Empty pattern should never match a non-empty action.
		{"", "finance.invoice.write", false},
	}
	for _, c := range cases {
		got := matchAction(c.pattern, c.action)
		if got != c.want {
			t.Errorf("matchAction(%q, %q) = %v, want %v", c.pattern, c.action, got, c.want)
		}
	}
}

func TestIsUnconditional(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, true},
		{`{}`, true},
		{`null`, true},
		{`{"owner_only":true}`, false},
		{`{"status_in":["draft"]}`, false},
	}
	for _, c := range cases {
		got := isUnconditional(json.RawMessage(c.raw))
		if got != c.want {
			t.Errorf("isUnconditional(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestMatchesConditions(t *testing.T) {
	owner := uuid.New()
	other := uuid.New()
	cases := []struct {
		name  string
		raw   string
		attrs map[string]any
		want  bool
	}{
		{
			name: "empty conditions match",
			raw:  `{}`,
			want: true,
		},
		{
			name:  "owner_only matches owner attr",
			raw:   `{"owner_only":true}`,
			attrs: map[string]any{"owner": owner.String()},
			want:  true,
		},
		{
			name:  "owner_only matches created_by attr",
			raw:   `{"owner_only":true}`,
			attrs: map[string]any{"created_by": owner},
			want:  true,
		},
		{
			name:  "owner_only fails when neither matches",
			raw:   `{"owner_only":true}`,
			attrs: map[string]any{"owner": other.String(), "created_by": other.String()},
			want:  false,
		},
		{
			name:  "status_in match",
			raw:   `{"status_in":["draft","pending"]}`,
			attrs: map[string]any{"status": "draft"},
			want:  true,
		},
		{
			name:  "status_in miss",
			raw:   `{"status_in":["draft"]}`,
			attrs: map[string]any{"status": "posted"},
			want:  false,
		},
		{
			name:  "unknown condition fails closed",
			raw:   `{"made_up":true}`,
			attrs: map[string]any{},
			want:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchesConditions(json.RawMessage(c.raw), owner, c.attrs)
			if got != c.want {
				t.Errorf("matchesConditions = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParsePermissions(t *testing.T) {
	// Object array form (with conditions).
	raw := json.RawMessage(`[{"action":"finance.*","resource":""},{"action":"krecord.read"}]`)
	got := parsePermissions(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 permissions, got %d", len(got))
	}
	if got[0].Action != "finance.*" {
		t.Errorf("got[0].Action = %q", got[0].Action)
	}

	// String array form — each entry becomes Permission{Action: s}.
	raw = json.RawMessage(`["tenant.member","krecord.read"]`)
	got = parsePermissions(raw)
	if len(got) != 2 || got[1].Action != "krecord.read" {
		t.Fatalf("string-array parse: %+v", got)
	}
}
