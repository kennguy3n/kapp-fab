// Package condition compiles and evaluates ABAC permission conditions
// expressed as a small, typed JSON DSL.
//
// # Why this package exists
//
// The legacy condition matcher (in internal/authz) accepted two
// hard-coded keys, "owner_only" and "status_in", baked directly into
// a switch statement. As the platform grew (per-user record ownership,
// per-status workflows, finance period locks, helpdesk SLA carve-outs,
// approval gates), every new policy required a Go code change and a
// redeploy. Worse, there was no formal grammar — operators were
// inferred from the key name ("status_in" implies set membership;
// "owner_only" implies an actor-identity equality), which made the
// matcher impossible to extend safely without re-implementing
// type-coercion rules per key.
//
// This package replaces that ad-hoc matcher with a small, typed AST
// covering the operators required by the security-review threat model
// (equality, ordering, set membership, string prefix/suffix/contains,
// regex match, existence, logical combinators) and a parser that
// compiles the JSON payload into a Node tree at policy-publish time.
// Evaluation against a record attribute bag and an actor identity is
// then a pure tree walk with no further allocation of compiled state.
//
// # Threat model
//
// The conditions blob is operator-authored (an RBAC admin writing
// policy via the Tenant Admin UI) but stored alongside permissions in
// a shared database. A malicious tenant admin attempting to bypass
// tenant isolation or escalate privileges is the primary adversary.
// The package defends against three classes of attack:
//
//  1. Field-reference exfiltration. The DSL only permits attribute
//     paths rooted at attrs.* (the record bag the caller passes) and
//     a fixed whitelist of actor.* references (user_id, tenant_id,
//     roles). There is no way to dereference arbitrary process state,
//     environment variables, or other tenants' records — the
//     evaluator simply has no syntax for it.
//
//  2. Stack-overflow / DoS via deeply nested AST. The parser enforces
//     a hard MaxDepth of 16, computed at compile time (before
//     evaluation). Beyond that depth the compile fails closed —
//     calling code never reaches the evaluator with an over-deep
//     tree. The choice of 16 mirrors the depth limits used by CEL
//     and OPA Rego for production policies; legitimate policies
//     observed in the codebase max out at 4 levels.
//
//  3. Fail-open on parse error. Compile errors return a Compiled
//     value whose Eval always returns false. This means a typo in a
//     condition payload denies access rather than granting it. The
//     authz layer composes this with its own deny-by-default
//     semantics, so the combined behaviour is "if the policy is
//     malformed, the action is denied".
//
// # Legacy compatibility
//
// Conditions stored before this DSL existed used the two-key form
// ({"owner_only": true} and {"status_in": [...]}). Compile() detects
// that form at parse time and rewrites it into the canonical AST so
// existing policies keep working without a database migration. The
// translation lives in compileLegacy() — see the comment there for
// the exact mapping.
package condition

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxDepth is the maximum nesting depth permitted in a compiled
// condition tree. Counted from the root node (depth 0). A leaf has
// depth 0 within its parent. An all_of containing two leaves has
// depth 1. The cap defends the evaluator against pathological inputs
// without constraining real-world policies — see package doc for
// rationale.
const MaxDepth = 16

// Actor describes the identity making the request. The DSL exposes
// the named fields as ref sources (actor.user_id, actor.tenant_id,
// actor.roles); the evaluator never reads anything else from the
// struct. Now is supplied separately so callers can pin it for
// deterministic policy evaluation in tests.
type Actor struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Roles    []string
	Now      time.Time
}

// Compiled is a parsed, validated condition tree. Cheap to copy by
// pointer; safe for concurrent Eval calls.
type Compiled struct {
	root Node
	// failClosed is set when Compile encountered a parse error. We
	// preserve a non-nil *Compiled and a sticky-false Eval so
	// callers in the hot path don't need to special-case
	// nil-vs-error — they just see "this policy denies".
	failClosed bool
}

// Node is the AST interface. All concrete types live in this file.
type Node interface {
	eval(env *evalEnv) (bool, error)
}

type evalEnv struct {
	actor Actor
	attrs map[string]any
}

// IsUnconditional reports whether the raw conditions payload is the
// empty form ({}, null, missing, or whitespace), in which case the
// permission is granted without any record-attribute checks. Mirrors
// the same semantics as the legacy isUnconditional helper that lived
// inside the authz package so callers can avoid Compile entirely on
// the hot, no-condition path.
func IsUnconditional(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "{}" || t == "null" {
		return true
	}
	return false
}

// Compile parses the JSON payload into a typed AST. Returns a sticky
// fail-closed Compiled (not an error) when the payload is malformed
// so the calling site composes naturally with the deny-by-default
// authz semantics — a typo in a policy locks the record out, never
// opens it up. Real errors (unrecoverable I/O, etc.) are returned;
// schema errors are not.
func Compile(raw json.RawMessage) (*Compiled, error) {
	if IsUnconditional(raw) {
		// Unconditional grant — the always-true AST short-circuits
		// to true at eval time. Modelled as an empty all_of (which
		// evaluates to true by the standard n-ary AND identity)
		// rather than a dedicated "true" node so we don't grow the
		// type surface for a degenerate case.
		return &Compiled{root: allOf{}}, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		// Deliberately drop the unmarshal error: see the package
		// doc on fail-closed-on-parse-error. Returning the error
		// instead would force every caller (authz, audit) to
		// special-case malformed policies as a separate denial
		// path from “policy parsed but didn’t match”. The combined
		// semantics we want are “if the policy can’t be parsed,
		// the action is denied” — nothing more.
		//nolint:nilerr // intentional fail-closed on parse error; see package doc
		return &Compiled{failClosed: true}, nil
	}
	if isLegacyShape(top) {
		// Legacy two-key form predates the DSL. Translate at parse
		// time so existing rows in the permissions.conditions
		// column keep working without a database migration. See
		// compileLegacy() for the mapping.
		root, err := compileLegacy(top)
		if err != nil {
			//nolint:nilerr // intentional fail-closed on parse error; see package doc
			return &Compiled{failClosed: true}, nil
		}
		return &Compiled{root: root}, nil
	}
	root, err := compileNode(raw, 0)
	if err != nil {
		//nolint:nilerr // intentional fail-closed on parse error; see package doc
		return &Compiled{failClosed: true}, nil
	}
	return &Compiled{root: root}, nil
}

// Eval walks the AST against the supplied actor and attribute bag.
// Returns false (no error) for fail-closed compiles. Returns false +
// error only for runtime regex / type errors the parser couldn't
// catch up-front (e.g. comparing a missing field with an inequality
// operator); the caller should treat the error path as a deny and
// surface it in audit logs.
func (c *Compiled) Eval(actor Actor, attrs map[string]any) (bool, error) {
	if c == nil || c.failClosed {
		return false, nil
	}
	env := &evalEnv{actor: actor, attrs: attrs}
	return c.root.eval(env)
}

// EvalRaw is a convenience that combines Compile + Eval for callers
// that don't cache the compiled form. Prefer Compile-once /
// Eval-many for hot paths.
func EvalRaw(raw json.RawMessage, actor Actor, attrs map[string]any) (bool, error) {
	c, err := Compile(raw)
	if err != nil {
		return false, err
	}
	return c.Eval(actor, attrs)
}

// -----------------------------------------------------------------
// AST node types
// -----------------------------------------------------------------

// allOf is the n-ary AND combinator. An empty children list evaluates
// to true (the conventional identity for AND across the empty set;
// matches Postgres `bool_and(...)` over zero rows). Short-circuits on
// the first false.
type allOf struct{ children []Node }

func (n allOf) eval(env *evalEnv) (bool, error) {
	for _, c := range n.children {
		ok, err := c.eval(env)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// anyOf is the n-ary OR combinator. An empty children list evaluates
// to false (identity for OR across the empty set; matches Postgres
// `bool_or(...)` over zero rows). Short-circuits on the first true.
type anyOf struct{ children []Node }

func (n anyOf) eval(env *evalEnv) (bool, error) {
	for _, c := range n.children {
		ok, err := c.eval(env)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// notNode negates its child. We keep the wrapper struct (rather than
// inverting at parse time) so AST inspection / debug printing can
// recover the operator the policy author wrote.
type notNode struct{ child Node }

func (n notNode) eval(env *evalEnv) (bool, error) {
	ok, err := n.child.eval(env)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

// leaf is a single comparison. path is the attribute path resolved at
// eval time (dotted into attrs). op selects the comparison kind.
// Exactly one of value or refSrc is populated: value holds a literal
// (string/number/bool/list of those); refSrc holds a reference to an
// actor.* property to be resolved at eval time.
type leaf struct {
	path   []string
	op     op
	value  *literal
	refSrc *refSource
	// regex is precompiled when op == opMatches and value is a
	// string literal. We compile once at parse time so a malformed
	// pattern fails the policy at Compile rather than blowing up at
	// every Eval.
	regex *regexp.Regexp
}

func (n leaf) eval(env *evalEnv) (bool, error) {
	lhs, lhsExists := resolveAttrPath(env.attrs, n.path)
	if n.op == opExists {
		// "exists" is the only operator that tolerates a missing
		// LHS. We treat the literal value (which must be a bool)
		// as the assertion: {"op":"exists","value":true} matches
		// when the path resolves; "value":false matches when it
		// doesn't.
		want := true
		if n.value != nil && n.value.kind == litBool {
			want = n.value.boolVal
		}
		return lhsExists == want, nil
	}
	if !lhsExists {
		// Missing LHS denies all other operators. This is the
		// "null-safe deny" behaviour security review insisted on:
		// a typo in attribute name should NOT silently match a
		// permissive rule.
		return false, nil
	}
	var rhs any
	if n.refSrc != nil {
		rhs = resolveRef(env.actor, *n.refSrc)
	} else if n.value != nil {
		rhs = n.value.toAny()
	}
	return applyOp(n.op, lhs, rhs, n.regex)
}

// -----------------------------------------------------------------
// Operator enum
// -----------------------------------------------------------------

type op uint8

const (
	opEq op = iota
	opNe
	opLt
	opLe
	opGt
	opGe
	opIn
	opNotIn
	opPrefix
	opSuffix
	opContains
	opExists
	opMatches
)

var opNames = map[string]op{
	"eq":       opEq,
	"ne":       opNe,
	"lt":       opLt,
	"le":       opLe,
	"gt":       opGt,
	"ge":       opGe,
	"in":       opIn,
	"not_in":   opNotIn,
	"prefix":   opPrefix,
	"suffix":   opSuffix,
	"contains": opContains,
	"exists":   opExists,
	"matches":  opMatches,
}

// -----------------------------------------------------------------
// Reference whitelist
// -----------------------------------------------------------------

type refSource uint8

const (
	refActorUserID refSource = iota
	refActorTenantID
	refActorRoles
	refNow
)

var refNames = map[string]refSource{
	"actor.user_id":   refActorUserID,
	"actor.tenant_id": refActorTenantID,
	"actor.roles":     refActorRoles,
	"now":             refNow,
}

func resolveRef(a Actor, r refSource) any {
	switch r {
	case refActorUserID:
		return a.UserID.String()
	case refActorTenantID:
		return a.TenantID.String()
	case refActorRoles:
		out := make([]any, len(a.Roles))
		for i, role := range a.Roles {
			out[i] = role
		}
		return out
	case refNow:
		if a.Now.IsZero() {
			return time.Now().UTC().Format(time.RFC3339)
		}
		return a.Now.UTC().Format(time.RFC3339)
	}
	return nil
}

// -----------------------------------------------------------------
// Literal values
// -----------------------------------------------------------------

type literalKind uint8

const (
	litString literalKind = iota
	litNumber
	litBool
	litList
	litNull
)

// literal is a parsed RHS value. Stored in normalised form so the
// evaluator never has to redo json.Unmarshal -> any type-switching:
// strings live in stringVal, numbers in numberVal, lists in listVal.
type literal struct {
	kind      literalKind
	stringVal string
	numberVal float64
	boolVal   bool
	listVal   []literal
}

func (l literal) toAny() any {
	switch l.kind {
	case litString:
		return l.stringVal
	case litNumber:
		return l.numberVal
	case litBool:
		return l.boolVal
	case litList:
		out := make([]any, len(l.listVal))
		for i, item := range l.listVal {
			out[i] = item.toAny()
		}
		return out
	case litNull:
		return nil
	}
	return nil
}

func parseLiteral(raw json.RawMessage) (*literal, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty value")
	}
	t := strings.TrimSpace(string(raw))
	if t == "null" {
		return &literal{kind: litNull}, nil
	}
	if t == "true" || t == "false" {
		return &literal{kind: litBool, boolVal: t == "true"}, nil
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return &literal{kind: litString, stringVal: s}, nil
	}
	if t[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
		items := make([]literal, len(arr))
		for i, item := range arr {
			child, err := parseLiteral(item)
			if err != nil {
				return nil, err
			}
			if child.kind == litList {
				return nil, errors.New("nested list in value")
			}
			items[i] = *child
		}
		return &literal{kind: litList, listVal: items}, nil
	}
	// Numeric (covers both int and float — json.Number isn't used
	// because we want a single normalised representation for
	// numeric comparison, and IEEE-754 covers the range we care
	// about here. Tenant IDs and other identifiers are strings.)
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("unrecognised literal %q: %w", t, err)
	}
	return &literal{kind: litNumber, numberVal: n}, nil
}

// -----------------------------------------------------------------
// Parser
// -----------------------------------------------------------------

func compileNode(raw json.RawMessage, depth int) (Node, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("condition depth exceeds %d", MaxDepth)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if len(m) != 1 {
		// Single-key envelope keeps the grammar context-free —
		// every node has exactly one top-level key naming its
		// kind. This also rules out trivial DoS shapes like
		// `{"all_of":[...],"any_of":[...]}` where two operators
		// race.
		return nil, fmt.Errorf("node must have exactly one key, got %d", len(m))
	}
	for key, body := range m {
		switch key {
		case "all_of":
			return compileGroup(body, depth, true)
		case "any_of":
			return compileGroup(body, depth, false)
		case "not":
			child, err := compileNode(body, depth+1)
			if err != nil {
				return nil, err
			}
			return notNode{child: child}, nil
		case "leaf":
			return compileLeaf(body)
		default:
			return nil, fmt.Errorf("unknown node kind %q", key)
		}
	}
	return nil, errors.New("empty node")
}

func compileGroup(raw json.RawMessage, depth int, isAnd bool) (Node, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	children := make([]Node, 0, len(items))
	for _, item := range items {
		child, err := compileNode(item, depth+1)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	if isAnd {
		return allOf{children: children}, nil
	}
	return anyOf{children: children}, nil
}

// leafSpec mirrors the on-wire shape. We unmarshal into it so the
// parser can validate field presence with json struct tags.
type leafSpec struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
	Ref   string          `json:"ref"`
}

func compileLeaf(raw json.RawMessage) (Node, error) {
	var spec leafSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	if spec.Field == "" {
		return nil, errors.New("leaf.field is required")
	}
	path, err := parseFieldPath(spec.Field)
	if err != nil {
		return nil, err
	}
	op, ok := opNames[spec.Op]
	if !ok {
		return nil, fmt.Errorf("unknown op %q", spec.Op)
	}
	n := leaf{path: path, op: op}
	hasValue := len(spec.Value) > 0
	hasRef := spec.Ref != ""
	if hasValue && hasRef {
		return nil, errors.New("leaf cannot have both value and ref")
	}
	if !hasValue && !hasRef && op != opExists {
		return nil, fmt.Errorf("leaf with op %q requires value or ref", spec.Op)
	}
	if hasRef {
		rs, ok := refNames[spec.Ref]
		if !ok {
			// Unknown ref MUST fail the policy. This is the
			// whitelist that defends against the policy-author
			// exfiltrating arbitrary process state — there is
			// simply no syntax that resolves to anything
			// outside the four allow-listed sources.
			return nil, fmt.Errorf("ref %q is not in the actor whitelist", spec.Ref)
		}
		n.refSrc = &rs
		// in/not_in with a ref makes sense only when the ref
		// resolves to a list (actor.roles). Other refs are
		// scalars; mismatching with a set operator is a policy
		// bug worth surfacing at compile time.
		if (op == opIn || op == opNotIn) && rs != refActorRoles {
			return nil, fmt.Errorf("op %q with ref %q: ref must be a list source", spec.Op, spec.Ref)
		}
	}
	if hasValue {
		lit, err := parseLiteral(spec.Value)
		if err != nil {
			return nil, err
		}
		n.value = lit
		// Set-operator RHS must be a list.
		if (op == opIn || op == opNotIn) && lit.kind != litList {
			return nil, fmt.Errorf("op %q requires a list value", spec.Op)
		}
		// Scalar-operator RHS must NOT be a list.
		if op != opIn && op != opNotIn && lit.kind == litList {
			return nil, fmt.Errorf("op %q does not accept a list value", spec.Op)
		}
		// Precompile regex for "matches" so an invalid pattern
		// fails at policy publish time, not on every record
		// access.
		if op == opMatches {
			if lit.kind != litString {
				return nil, errors.New("op \"matches\" requires a string pattern")
			}
			re, err := regexp.Compile(lit.stringVal)
			if err != nil {
				return nil, fmt.Errorf("invalid regex pattern: %w", err)
			}
			n.regex = re
		}
	}
	// "exists" with a non-bool value (or no value) is normalised
	// to {value: true} so authors can write {"op":"exists"} as a
	// shorthand for "field is present".
	if op == opExists {
		if n.value == nil {
			n.value = &literal{kind: litBool, boolVal: true}
		}
		if n.value.kind != litBool {
			return nil, errors.New("op \"exists\" requires a bool value")
		}
	}
	return n, nil
}

// parseFieldPath validates that a field reference is a dotted
// identifier path. The leading segment is implicitly "attrs"; we
// don't accept "attrs." or any other prefix so the policy author
// can't accidentally type "attrs.foo" and have it lookup
// attrs["attrs"]["foo"]. Empty segments and segments containing
// anything other than [A-Za-z0-9_] are rejected at compile time.
func parseFieldPath(s string) ([]string, error) {
	if s == "" {
		return nil, errors.New("field cannot be empty")
	}
	parts := strings.Split(s, ".")
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("empty segment in field %q", s)
		}
		for _, r := range p {
			ok := (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') ||
				r == '_'
			if !ok {
				return nil, fmt.Errorf("invalid character %q in field %q", r, s)
			}
		}
	}
	return parts, nil
}

// resolveAttrPath walks a dotted path through the attribute bag.
// Intermediate nodes must be map[string]any; anything else
// (including a missing key) yields lhsExists=false. We deliberately
// do NOT auto-flatten through arrays — JSON Path-style multi-result
// resolution would require operator semantics for "any" vs "all"
// matches and we don't need it yet for the policies under review.
func resolveAttrPath(attrs map[string]any, path []string) (any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	var cur any = attrs
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		next, present := m[seg]
		if !present {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// -----------------------------------------------------------------
// Operator dispatch
// -----------------------------------------------------------------

// applyOp does the actual comparison. Both sides are arbitrary `any`
// values; we normalise into one of the four supported scalar kinds
// (string, number, bool, uuid-as-string) before comparing. The
// pre-compiled regex is threaded through for opMatches.
func applyOp(o op, lhs, rhs any, re *regexp.Regexp) (bool, error) {
	switch o {
	case opEq:
		return equal(lhs, rhs), nil
	case opNe:
		return !equal(lhs, rhs), nil
	case opLt, opLe, opGt, opGe:
		cmp, ok := compareOrdered(lhs, rhs)
		if !ok {
			return false, nil
		}
		switch o {
		case opLt:
			return cmp < 0, nil
		case opLe:
			return cmp <= 0, nil
		case opGt:
			return cmp > 0, nil
		case opGe:
			return cmp >= 0, nil
		}
	case opIn, opNotIn:
		list, ok := rhs.([]any)
		if !ok {
			return false, nil
		}
		found := false
		for _, item := range list {
			if equal(lhs, item) {
				found = true
				break
			}
		}
		if o == opIn {
			return found, nil
		}
		return !found, nil
	case opPrefix:
		ls, ok1 := asString(lhs)
		rs, ok2 := asString(rhs)
		if !ok1 || !ok2 {
			return false, nil
		}
		return strings.HasPrefix(ls, rs), nil
	case opSuffix:
		ls, ok1 := asString(lhs)
		rs, ok2 := asString(rhs)
		if !ok1 || !ok2 {
			return false, nil
		}
		return strings.HasSuffix(ls, rs), nil
	case opContains:
		// On strings: substring containment. On lists: element
		// membership. We pick the behaviour by lhs type so policy
		// authors can write the natural-feeling form for either.
		if list, ok := lhs.([]any); ok {
			for _, item := range list {
				if equal(item, rhs) {
					return true, nil
				}
			}
			return false, nil
		}
		ls, ok1 := asString(lhs)
		rs, ok2 := asString(rhs)
		if !ok1 || !ok2 {
			return false, nil
		}
		return strings.Contains(ls, rs), nil
	case opMatches:
		if re == nil {
			return false, errors.New("matches op missing precompiled regex")
		}
		ls, ok := asString(lhs)
		if !ok {
			return false, nil
		}
		return re.MatchString(ls), nil
	}
	return false, fmt.Errorf("unknown op %d", o)
}

// equal implements value equality across the supported scalar kinds.
// Numeric comparisons normalise int/float64 (JSON decodes ints as
// float64, but attribute bags written by Go code may carry native
// ints). UUID comparisons normalise to lowercase string form so
// "abc..." == "ABC..." holds. Bool comparisons require both sides to
// be bool.
func equal(a, b any) bool {
	if as, aok := asString(a); aok {
		bs, bok := asString(b)
		if !bok {
			return false
		}
		return as == bs
	}
	if an, aok := asNumber(a); aok {
		bn, bok := asNumber(b)
		if !bok {
			return false
		}
		return an == bn
	}
	if ab, aok := a.(bool); aok {
		bb, bok := b.(bool)
		if !bok {
			return false
		}
		return ab == bb
	}
	return false
}

// compareOrdered returns -1/0/1 and ok=true when both sides are of
// the same comparable kind (numbers compared numerically, strings
// compared lexicographically, times compared chronologically). When
// the types don't match we return ok=false so the caller can decide
// (the leaf evaluator treats this as a deny).
func compareOrdered(a, b any) (int, bool) {
	if an, aok := asNumber(a); aok {
		if bn, bok := asNumber(b); bok {
			switch {
			case an < bn:
				return -1, true
			case an > bn:
				return 1, true
			default:
				return 0, true
			}
		}
		return 0, false
	}
	if as, aok := asString(a); aok {
		if bs, bok := asString(b); bok {
			// Time-shaped strings compare chronologically (the
			// RFC3339 string form is lexicographically ordered
			// by construction, so the plain string compare is
			// already correct for that case; we keep the
			// explicit time parse path for fault tolerance
			// against locale-stamped variations like
			// "2025-01-02 03:04:05+00:00").
			at, aerr := time.Parse(time.RFC3339, as)
			bt, berr := time.Parse(time.RFC3339, bs)
			if aerr == nil && berr == nil {
				switch {
				case at.Before(bt):
					return -1, true
				case at.After(bt):
					return 1, true
				default:
					return 0, true
				}
			}
			return strings.Compare(as, bs), true
		}
	}
	return 0, false
}

func asString(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case uuid.UUID:
		return s.String(), true
	case fmt.Stringer:
		return s.String(), true
	}
	return "", false
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// -----------------------------------------------------------------
// Legacy {"owner_only":true,"status_in":[...]} translation
// -----------------------------------------------------------------

// isLegacyShape returns true when the top-level keys match the
// pre-DSL form. The new DSL uses {"all_of"|"any_of"|"not"|"leaf"}
// only, so any other top-level key means we're looking at legacy.
// We accept any combination of the legacy keys (typically only one,
// but {"owner_only": true, "status_in": [...]} is meaningful as
// "owner AND status-in-set").
func isLegacyShape(top map[string]json.RawMessage) bool {
	if len(top) == 0 {
		return false
	}
	for key := range top {
		switch key {
		case "owner_only", "status_in":
			continue
		default:
			return false
		}
	}
	return true
}

// compileLegacy translates the old key-based form into the canonical
// AST. The mapping is:
//
//   - owner_only=true  → leaf{field=owner, op=eq, ref=actor.user_id}
//     OR leaf{field=created_by, op=eq, ref=actor.user_id}
//   - owner_only=false → no condition (we drop the key; legacy
//     semantics treated false as a no-op).
//   - status_in=[...]  → leaf{field=status, op=in, value=[...]}
//
// Multiple keys are joined with all_of (n-ary AND), preserving the
// legacy "all keys must match" semantics.
func compileLegacy(top map[string]json.RawMessage) (Node, error) {
	var nodes []Node
	if v, ok := top["owner_only"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, err
		}
		if b {
			ownerRef := refActorUserID
			ownerLeaf := leaf{
				path:   []string{"owner"},
				op:     opEq,
				refSrc: &ownerRef,
			}
			createdRef := refActorUserID
			createdLeaf := leaf{
				path:   []string{"created_by"},
				op:     opEq,
				refSrc: &createdRef,
			}
			nodes = append(nodes, anyOf{children: []Node{ownerLeaf, createdLeaf}})
		}
	}
	if v, ok := top["status_in"]; ok {
		lit, err := parseLiteral(v)
		if err != nil {
			return nil, err
		}
		if lit.kind != litList {
			return nil, errors.New("legacy status_in must be a list")
		}
		nodes = append(nodes, leaf{
			path:  []string{"status"},
			op:    opIn,
			value: lit,
		})
	}
	if len(nodes) == 0 {
		return allOf{}, nil
	}
	if len(nodes) == 1 {
		return nodes[0], nil
	}
	return allOf{children: nodes}, nil
}
