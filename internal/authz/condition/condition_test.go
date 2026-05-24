package condition

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// actor builds a deterministic Actor for tests. UserID + TenantID
// stay the same across all tests so equality / set-membership
// comparisons are reproducible.
func actor() Actor {
	uid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	tid := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	now, _ := time.Parse(time.RFC3339, "2025-01-15T12:00:00Z")
	return Actor{
		UserID:   uid,
		TenantID: tid,
		Roles:    []string{"member", "finance-reviewer"},
		Now:      now,
	}
}

func mustEval(t *testing.T, raw string, attrs map[string]any) bool {
	t.Helper()
	c, err := Compile(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Compile(%q) error: %v", raw, err)
	}
	got, err := c.Eval(actor(), attrs)
	if err != nil {
		t.Fatalf("Eval(%q) error: %v", raw, err)
	}
	return got
}

func TestUnconditional(t *testing.T) {
	cases := []string{"", "{}", "null", "   "}
	for _, raw := range cases {
		if !mustEval(t, raw, nil) {
			t.Errorf("unconditional %q should match", raw)
		}
	}
}

func TestLeafEqByLiteral(t *testing.T) {
	raw := `{"leaf":{"field":"status","op":"eq","value":"open"}}`
	if !mustEval(t, raw, map[string]any{"status": "open"}) {
		t.Error("status=open should match")
	}
	if mustEval(t, raw, map[string]any{"status": "closed"}) {
		t.Error("status=closed should not match")
	}
}

func TestLeafEqByActorRef(t *testing.T) {
	a := actor()
	raw := `{"leaf":{"field":"owner","op":"eq","ref":"actor.user_id"}}`
	attrs := map[string]any{"owner": a.UserID.String()}
	if !mustEval(t, raw, attrs) {
		t.Error("owner=actor.user_id should match")
	}
	other := uuid.New().String()
	if mustEval(t, raw, map[string]any{"owner": other}) {
		t.Error("owner=different uuid should not match")
	}
}

func TestLeafNumericComparison(t *testing.T) {
	raw := `{"leaf":{"field":"amount","op":"lt","value":100}}`
	if !mustEval(t, raw, map[string]any{"amount": 50.0}) {
		t.Error("50 < 100")
	}
	if mustEval(t, raw, map[string]any{"amount": 150.0}) {
		t.Error("150 < 100 should be false")
	}
	if mustEval(t, raw, map[string]any{"amount": "fifty"}) {
		t.Error("string < number should not match (mismatched types)")
	}
}

func TestLeafIn(t *testing.T) {
	raw := `{"leaf":{"field":"status","op":"in","value":["open","pending","review"]}}`
	for _, s := range []string{"open", "pending", "review"} {
		if !mustEval(t, raw, map[string]any{"status": s}) {
			t.Errorf("status=%q should be in set", s)
		}
	}
	if mustEval(t, raw, map[string]any{"status": "closed"}) {
		t.Error("closed should not be in set")
	}
}

func TestLeafInActorRoles(t *testing.T) {
	raw := `{"leaf":{"field":"required_role","op":"in","ref":"actor.roles"}}`
	if !mustEval(t, raw, map[string]any{"required_role": "finance-reviewer"}) {
		t.Error("required_role=finance-reviewer should be in actor.roles")
	}
	if mustEval(t, raw, map[string]any{"required_role": "admin"}) {
		t.Error("required_role=admin should not be in actor.roles")
	}
}

func TestLeafPrefixSuffixContains(t *testing.T) {
	cases := []struct {
		raw   string
		attrs map[string]any
		want  bool
	}{
		{`{"leaf":{"field":"path","op":"prefix","value":"/portal/"}}`, map[string]any{"path": "/portal/tickets"}, true},
		{`{"leaf":{"field":"path","op":"prefix","value":"/portal/"}}`, map[string]any{"path": "/admin"}, false},
		{`{"leaf":{"field":"path","op":"suffix","value":".pdf"}}`, map[string]any{"path": "report.pdf"}, true},
		{`{"leaf":{"field":"path","op":"suffix","value":".pdf"}}`, map[string]any{"path": "report.txt"}, false},
		{`{"leaf":{"field":"path","op":"contains","value":"invoice"}}`, map[string]any{"path": "/finance/invoice/123"}, true},
		{`{"leaf":{"field":"tags","op":"contains","value":"vip"}}`, map[string]any{"tags": []any{"vip", "priority"}}, true},
		{`{"leaf":{"field":"tags","op":"contains","value":"normal"}}`, map[string]any{"tags": []any{"vip", "priority"}}, false},
	}
	for i, c := range cases {
		got := mustEval(t, c.raw, c.attrs)
		if got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}

func TestLeafExists(t *testing.T) {
	rawT := `{"leaf":{"field":"owner","op":"exists","value":true}}`
	rawF := `{"leaf":{"field":"owner","op":"exists","value":false}}`
	if !mustEval(t, rawT, map[string]any{"owner": "x"}) {
		t.Error("owner present, exists=true should match")
	}
	if mustEval(t, rawT, map[string]any{}) {
		t.Error("owner missing, exists=true should not match")
	}
	if !mustEval(t, rawF, map[string]any{}) {
		t.Error("owner missing, exists=false should match")
	}
	// Bare exists (no value) defaults to true.
	if !mustEval(t, `{"leaf":{"field":"owner","op":"exists"}}`, map[string]any{"owner": "x"}) {
		t.Error("bare exists should default to true")
	}
}

func TestLeafMatchesRegex(t *testing.T) {
	raw := `{"leaf":{"field":"path","op":"matches","value":"^/tenants/[0-9a-f-]+/files$"}}`
	if !mustEval(t, raw, map[string]any{"path": "/tenants/abc-def/files"}) {
		t.Error("regex should match")
	}
	if mustEval(t, raw, map[string]any{"path": "/tenants/abc/admin"}) {
		t.Error("regex should not match")
	}
}

func TestAllOfAnyOfNot(t *testing.T) {
	raw := `{
		"all_of":[
			{"leaf":{"field":"status","op":"eq","value":"open"}},
			{"any_of":[
				{"leaf":{"field":"owner","op":"eq","ref":"actor.user_id"}},
				{"leaf":{"field":"team","op":"contains","ref":"actor.user_id"}}
			]},
			{"not":{"leaf":{"field":"locked","op":"eq","value":true}}}
		]
	}`
	a := actor()
	uid := a.UserID.String()
	good := map[string]any{"status": "open", "owner": uid, "locked": false}
	if !mustEval(t, raw, good) {
		t.Error("all conditions met should pass")
	}
	bad := map[string]any{"status": "open", "owner": uid, "locked": true}
	if mustEval(t, raw, bad) {
		t.Error("locked=true should fail via not")
	}
}

func TestEmptyAllOfAnyOf(t *testing.T) {
	if !mustEval(t, `{"all_of":[]}`, nil) {
		t.Error("empty all_of is true (AND identity)")
	}
	if mustEval(t, `{"any_of":[]}`, nil) {
		t.Error("empty any_of is false (OR identity)")
	}
}

func TestNestedPath(t *testing.T) {
	raw := `{"leaf":{"field":"customer.tier","op":"eq","value":"enterprise"}}`
	if !mustEval(t, raw, map[string]any{
		"customer": map[string]any{"tier": "enterprise"},
	}) {
		t.Error("nested path should resolve")
	}
	// Missing intermediate denies.
	if mustEval(t, raw, map[string]any{}) {
		t.Error("missing intermediate should deny")
	}
}

func TestLegacyOwnerOnly(t *testing.T) {
	a := actor()
	uid := a.UserID.String()
	raw := `{"owner_only":true}`
	if !mustEval(t, raw, map[string]any{"owner": uid}) {
		t.Error("legacy owner_only via owner=uid")
	}
	if !mustEval(t, raw, map[string]any{"created_by": uid}) {
		t.Error("legacy owner_only via created_by=uid")
	}
	if mustEval(t, raw, map[string]any{"owner": uuid.New().String()}) {
		t.Error("legacy owner_only should deny on mismatch")
	}
}

func TestLegacyStatusIn(t *testing.T) {
	raw := `{"status_in":["draft","review"]}`
	if !mustEval(t, raw, map[string]any{"status": "draft"}) {
		t.Error("legacy status_in match")
	}
	if mustEval(t, raw, map[string]any{"status": "posted"}) {
		t.Error("legacy status_in miss")
	}
}

func TestLegacyCombined(t *testing.T) {
	a := actor()
	raw := `{"owner_only":true,"status_in":["draft"]}`
	if !mustEval(t, raw, map[string]any{
		"owner":  a.UserID.String(),
		"status": "draft",
	}) {
		t.Error("legacy combined match")
	}
	if mustEval(t, raw, map[string]any{
		"owner":  a.UserID.String(),
		"status": "posted",
	}) {
		t.Error("legacy combined: status mismatch should deny")
	}
	if mustEval(t, raw, map[string]any{
		"owner":  uuid.New().String(),
		"status": "draft",
	}) {
		t.Error("legacy combined: owner mismatch should deny")
	}
}

// -------------------------------------------------------------
// Security / privilege-escalation attack-vector tests.
//
// These exercise every "what if the policy author tried to..." vector
// that came out of the security-review threat model. Each should
// either fail closed (Compile yields a fail-closed Compiled whose
// Eval returns false) or be parsed safely into a known shape that
// doesn't grant access.
// -------------------------------------------------------------

func TestSecurity_UnknownTopLevelKeyDenies(t *testing.T) {
	// Pre-DSL form had unknown keys fall through to "deny". The new
	// DSL must preserve that: a typo in the operator name shouldn't
	// open a record up.
	if mustEval(t, `{"made_up_operator":true}`, map[string]any{}) {
		t.Error("unknown top-level key should fail closed")
	}
}

func TestSecurity_UnknownLeafOpDenies(t *testing.T) {
	if mustEval(t, `{"leaf":{"field":"status","op":"glob","value":"o*"}}`, map[string]any{"status": "open"}) {
		t.Error("unknown leaf op should fail closed")
	}
}

func TestSecurity_RefOutsideWhitelistDenies(t *testing.T) {
	// Attempting to reference an actor or environment field we
	// haven't allow-listed must deny — the whitelist is the
	// primary defence against exfiltration.
	for _, badRef := range []string{
		"actor.password",
		"actor.email",
		"env.JWT_SECRET",
		"process.env.JWT_SECRET",
		"__proto__",
		"actor",
		"actor.user_id.then_keep_going",
	} {
		raw := `{"leaf":{"field":"owner","op":"eq","ref":"` + badRef + `"}}`
		if mustEval(t, raw, map[string]any{"owner": "anything"}) {
			t.Errorf("bad ref %q should fail closed", badRef)
		}
	}
}

func TestSecurity_BothValueAndRefDenies(t *testing.T) {
	// Specifying both is a syntactic error — the schema requires
	// exactly one. Don't pick one over the other arbitrarily;
	// reject and fail closed.
	raw := `{"leaf":{"field":"owner","op":"eq","value":"x","ref":"actor.user_id"}}`
	if mustEval(t, raw, map[string]any{"owner": "x"}) {
		t.Error("both value and ref should fail closed")
	}
}

func TestSecurity_FieldPathInjectionDenies(t *testing.T) {
	// The field path is a dotted whitelist of identifier
	// segments. Slashes, backslashes, quotes, brackets, hash
	// signs, dollar signs, control chars, etc. must all be
	// rejected at compile time.
	for _, badField := range []string{
		"../etc/passwd",
		"owner; DROP TABLE permissions;--",
		"owner['$ne']",
		"owner.__proto__",
		`owner."quoted"`,
		"owner\x00null",
		"\x00",
		"owner&&actor.user_id",
	} {
		raw := `{"leaf":{"field":"` + badField + `","op":"eq","value":"x"}}`
		if mustEval(t, raw, map[string]any{"owner": "x"}) {
			t.Errorf("bad field %q should fail closed", badField)
		}
	}
}

func TestSecurity_DepthLimit(t *testing.T) {
	// Construct a tree deeper than MaxDepth and verify Compile
	// fails closed — this is the DoS defence. We build it by
	// nesting "not" wrappers.
	var b strings.Builder
	for i := 0; i < MaxDepth+5; i++ {
		b.WriteString(`{"not":`)
	}
	b.WriteString(`{"leaf":{"field":"x","op":"eq","value":"y"}}`)
	for i := 0; i < MaxDepth+5; i++ {
		b.WriteString(`}`)
	}
	c, err := Compile(json.RawMessage(b.String()))
	if err != nil {
		t.Fatalf("Compile returned error (should fail-closed silently): %v", err)
	}
	got, _ := c.Eval(actor(), map[string]any{"x": "y"})
	if got {
		t.Error("over-deep tree should fail closed, not match")
	}
}

func TestSecurity_NestedListValueDenies(t *testing.T) {
	// We deliberately don't support nested lists in a value
	// literal. A policy that tried to express a set-of-sets
	// should not be silently coerced.
	raw := `{"leaf":{"field":"status","op":"in","value":[["a","b"],["c"]]}}`
	if mustEval(t, raw, map[string]any{"status": "a"}) {
		t.Error("nested list value should fail closed")
	}
}

func TestSecurity_ListOnScalarOpDenies(t *testing.T) {
	// Scalar operators must not accept a list — otherwise a
	// policy "field op eq value [a,b]" could be misinterpreted as
	// "in".
	raw := `{"leaf":{"field":"status","op":"eq","value":["open","closed"]}}`
	if mustEval(t, raw, map[string]any{"status": "open"}) {
		t.Error("eq with list value should fail closed")
	}
}

func TestSecurity_ScalarOnSetOpDenies(t *testing.T) {
	// Symmetrically, in/not_in with a scalar literal is rejected.
	raw := `{"leaf":{"field":"status","op":"in","value":"open"}}`
	if mustEval(t, raw, map[string]any{"status": "open"}) {
		t.Error("in with scalar value should fail closed")
	}
}

func TestSecurity_BadRegexDenies(t *testing.T) {
	raw := `{"leaf":{"field":"path","op":"matches","value":"["}}`
	if mustEval(t, raw, map[string]any{"path": "anything"}) {
		t.Error("invalid regex should fail closed at compile time")
	}
}

func TestSecurity_RegexCatastrophicBacktrack(t *testing.T) {
	// Go's regexp package uses RE2 — no catastrophic backtracking
	// by design. This test documents the property rather than
	// proves it; a fuzz target would be appropriate for deeper
	// coverage but is out of scope here.
	raw := `{"leaf":{"field":"v","op":"matches","value":"^(a+)+$"}}`
	long := strings.Repeat("a", 5000) + "X"
	start := time.Now()
	c, err := Compile(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := c.Eval(actor(), map[string]any{"v": long})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Error("trailing X should not match")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("evaluation took %s — RE2 should run in linear time", d)
	}
}

func TestSecurity_MissingLHSDenies(t *testing.T) {
	// Null-safe deny: missing LHS attribute deny on every
	// operator except "exists". A typo in attribute name must
	// NOT silently match a permissive rule.
	for _, op := range []string{"eq", "ne", "lt", "le", "gt", "ge", "in", "not_in", "prefix", "suffix", "contains", "matches"} {
		val := `"foo"`
		if op == "in" || op == "not_in" {
			val = `["foo"]`
		}
		if op == "lt" || op == "le" || op == "gt" || op == "ge" {
			val = `1`
		}
		raw := `{"leaf":{"field":"missing","op":"` + op + `","value":` + val + `}}`
		// Even ne should NOT match on missing — pre-DSL behaviour
		// surprised callers by treating "ne" as "field is absent
		// OR field != value". Null-safe deny is the conservative
		// choice and matches SQL three-valued logic where
		// NULL != x → NULL → not true.
		if mustEval(t, raw, map[string]any{}) {
			t.Errorf("op %q with missing LHS should deny", op)
		}
	}
}

func TestSecurity_UnknownActorRefInGroup(t *testing.T) {
	// A policy hides a bad ref inside an any_of so the parser must
	// still fail closed (not partially compile and silently grant
	// via the OTHER any_of branch).
	raw := `{"any_of":[
		{"leaf":{"field":"status","op":"eq","value":"never_matches"}},
		{"leaf":{"field":"owner","op":"eq","ref":"actor.is_god"}}
	]}`
	if mustEval(t, raw, map[string]any{"owner": "anything", "status": "never_matches"}) {
		t.Error("bad ref inside any_of should fail closed for the whole policy")
	}
}

func TestSecurity_LegacyOwnerOnlyTypoDenies(t *testing.T) {
	// A pre-DSL policy author who typos'd "ownerly" or "owners"
	// would have previously hit the "unknown condition fails
	// closed" branch. New DSL must preserve that.
	if mustEval(t, `{"ownerly":true}`, map[string]any{"owner": "x"}) {
		t.Error("legacy typo'd key should fail closed")
	}
}

func TestSecurity_TwoTopLevelOperatorsDenies(t *testing.T) {
	// Putting two operators at the same level is a context-free
	// grammar violation — the parser requires exactly one key per
	// node. Otherwise we'd have to define precedence between
	// all_of and any_of when both appear, which would invite
	// confusion.
	raw := `{"all_of":[{"leaf":{"field":"x","op":"eq","value":"y"}}],"any_of":[]}`
	if mustEval(t, raw, map[string]any{"x": "y"}) {
		t.Error("two top-level operators should fail closed")
	}
}

func TestSecurity_UUIDStringNormalisation(t *testing.T) {
	// Both sides of an actor.user_id comparison must compare as
	// strings — passing the attribute as uuid.UUID type from Go
	// code should equal passing it as a string. Without this the
	// caller couldn't safely pass either form.
	a := actor()
	raw := `{"leaf":{"field":"owner","op":"eq","ref":"actor.user_id"}}`
	if !mustEval(t, raw, map[string]any{"owner": a.UserID}) {
		t.Error("uuid.UUID attribute should equal actor.user_id string ref")
	}
	if !mustEval(t, raw, map[string]any{"owner": a.UserID.String()}) {
		t.Error("string-form uuid attribute should equal actor.user_id ref")
	}
}

func TestSecurity_NowComparison(t *testing.T) {
	// "now" ref allows time-bounded policies (e.g. "this rule is
	// only effective until 2026-01-01"). Verify the comparison
	// works with RFC3339 strings on both sides.
	raw := `{"leaf":{"field":"deadline","op":"gt","ref":"now"}}`
	if !mustEval(t, raw, map[string]any{"deadline": "2026-01-01T00:00:00Z"}) {
		t.Error("future deadline should be > now")
	}
	if mustEval(t, raw, map[string]any{"deadline": "2024-01-01T00:00:00Z"}) {
		t.Error("past deadline should not be > now")
	}
}

func TestSecurity_NoSilentlyMatchingNullValues(t *testing.T) {
	// A "null" literal on the RHS of eq must not silently match
	// missing attributes — only explicit-null attributes (which
	// the bag almost never carries; missing keys are the norm).
	raw := `{"leaf":{"field":"status","op":"eq","value":null}}`
	if mustEval(t, raw, map[string]any{}) {
		t.Error("missing LHS should deny (not silently match null literal)")
	}
}

func TestSecurity_BothValueAndRefAtCompileTime(t *testing.T) {
	// Belt-and-braces: also assert at the compile level (not just
	// behaviour via mustEval) that a both-value-and-ref leaf
	// produces a fail-closed Compiled.
	c, err := Compile(json.RawMessage(`{"leaf":{"field":"x","op":"eq","value":"y","ref":"actor.user_id"}}`))
	if err != nil {
		t.Fatalf("Compile should not return error: %v", err)
	}
	got, _ := c.Eval(actor(), map[string]any{"x": "y"})
	if got {
		t.Error("fail-closed Compiled must always Eval false")
	}
}

func TestEvalRawConvenience(t *testing.T) {
	a := actor()
	got, err := EvalRaw(
		json.RawMessage(`{"leaf":{"field":"owner","op":"eq","ref":"actor.user_id"}}`),
		a,
		map[string]any{"owner": a.UserID.String()},
	)
	if err != nil {
		t.Fatalf("EvalRaw: %v", err)
	}
	if !got {
		t.Error("EvalRaw should match")
	}
}

func TestCompileOnceEvalMany(t *testing.T) {
	// Hot-path expectation: Compile once, Eval many times. Verify
	// Eval is idempotent and doesn't mutate the AST.
	a := actor()
	c, err := Compile(json.RawMessage(`{"leaf":{"field":"status","op":"in","value":["open","pending"]}}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := c.Eval(a, map[string]any{"status": "open"})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !got {
			t.Fatalf("iter %d: expected match", i)
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
		{`   `, true},
		{`{"owner_only":true}`, false},
		{`{"leaf":{"field":"x","op":"eq","value":"y"}}`, false},
	}
	for _, c := range cases {
		got := IsUnconditional(json.RawMessage(c.raw))
		if got != c.want {
			t.Errorf("IsUnconditional(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}
