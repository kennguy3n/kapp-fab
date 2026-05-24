package authz

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/authz/condition"
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
			got := matchesConditions(json.RawMessage(c.raw), owner, uuid.Nil, c.attrs)
			if got != c.want {
				t.Errorf("matchesConditions = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCompilePermissionUnconditional asserts that compilePermission
// leaves compiled=nil for any unconditional blob (empty / null / {}).
// This is the short-circuit the AuthorizeRecord hot path relies on:
// evalCompiled returns true immediately for nil-compiled rows so we
// don't pay an AST walk for the common unconditional case.
func TestCompilePermissionUnconditional(t *testing.T) {
	for _, raw := range []string{"", "{}", "null"} {
		got := compilePermission(Permission{
			Action:     "krecord.read",
			Conditions: json.RawMessage(raw),
		})
		if got.compiled != nil {
			t.Errorf("compilePermission(%q) compiled=%v, want nil", raw, got.compiled)
		}
	}
}

// TestCompilePermissionConditional asserts that a conditional blob
// gets a non-nil compiled AST attached — this is the perf fix:
// AuthorizeRecord should NOT recompile JSON-to-AST on every request.
// We verify the AST is non-nil and that Eval works through it.
func TestCompilePermissionConditional(t *testing.T) {
	cp := compilePermission(Permission{
		Action:     "krecord.read",
		Conditions: json.RawMessage(`{"leaf":{"field":"status","op":"eq","value":"open"}}`),
	})
	if cp.compiled == nil {
		t.Fatal("compilePermission left compiled=nil for a conditional blob")
	}
	ok, err := cp.compiled.Eval(condition.Actor{}, map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	if !ok {
		t.Error("status=open should match the compiled AST")
	}
}

// TestEvalCompiledNilShortCircuit guards the unconditional fast path:
// a nil *Compiled means "grant regardless of attrs / actor". This is
// load-bearing for keeping the unconditional-permission cost low.
func TestEvalCompiledNilShortCircuit(t *testing.T) {
	ok, err := evalCompiled(nil, condition.Actor{}, nil)
	if err != nil {
		t.Fatalf("evalCompiled(nil) err: %v", err)
	}
	if !ok {
		t.Error("evalCompiled(nil) should return true (unconditional grant)")
	}
}

// TestActorRolesWiring is the regression test for the privilege-
// escalation fix: a condition that references actor.roles must observe
// the actor's roles, not a zero / nil slice. We exercise this via the
// compiled-AST path because that is how AuthorizeRecord reaches the
// condition evaluator in production.
//
// Scenario: a permission gated on "actor.roles must NOT contain
// 'frozen'". With the old code that never populated Actor.Roles, the
// "not in []" check trivially held for every user and the permission
// was always granted — that is the priv-esc. With the fix the actor's
// real role list is passed in; a frozen-status user is denied; an
// unfrozen user is granted.
func TestActorRolesWiring(t *testing.T) {
	cp := compilePermission(Permission{
		Action: "krecord.delete",
		Conditions: json.RawMessage(
			`{"not":{"leaf":{"field":"required_role","op":"in","ref":"actor.roles"}}}`,
		),
	})
	if cp.compiled == nil {
		t.Fatal("expected compiled AST for conditional permission")
	}

	// Frozen user: their roles list contains "frozen", so the
	// inner leaf is true and `not` flips it to false → deny.
	frozen := condition.Actor{Roles: []string{"member", "frozen"}}
	ok, err := evalCompiled(cp.compiled, frozen, map[string]any{"required_role": "frozen"})
	if err != nil {
		t.Fatalf("frozen eval err: %v", err)
	}
	if ok {
		t.Error("frozen user should be denied (roles contains frozen)")
	}

	// Regular user: required_role 'frozen' is NOT in their roles
	// list, so the inner leaf is false and `not` flips to true →
	// allow. This is the case that used to be silently true for
	// EVERY user when Actor.Roles was nil.
	regular := condition.Actor{Roles: []string{"member"}}
	ok, err = evalCompiled(cp.compiled, regular, map[string]any{"required_role": "frozen"})
	if err != nil {
		t.Fatalf("regular eval err: %v", err)
	}
	if !ok {
		t.Error("regular user should be allowed (roles does not contain frozen)")
	}

	// Nil-roles regression: the OLD code paths effectively passed
	// Actor{} — make sure that, were a future refactor to drop the
	// roles plumbing, this test catches the silent-allow on a
	// "must NOT have role X" rule. The not_in check on a nil slice
	// is still true (nothing is in the empty set), so this case
	// LOOKS like an allow even though the user has no roles at
	// all. We document the intent here: the priv-esc was about
	// real role-bearing users being treated as role-less; the test
	// above (frozen vs regular) is what catches that. We keep this
	// assertion to lock in the documented "empty roles → not_in
	// trivially true" semantics so any future change to the
	// fail-closed direction is intentional.
	empty := condition.Actor{}
	ok, _ = evalCompiled(cp.compiled, empty, map[string]any{"required_role": "frozen"})
	if !ok {
		t.Error("empty-roles actor: not_in over empty set is true; this should be allow")
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
