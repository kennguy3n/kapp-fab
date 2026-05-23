package insights

import (
	"errors"
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v5"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ErrUnsafeSQL tags an insights-editor SQL body that the parser-based
// validator rejected — multi-statement, non-SELECT (DML/DDL), or
// touches a system catalog. Distinct from ErrValidation so callers
// who want to surface "unsafe SQL, not a generic input typo" can do
// a single errors.Is check; wrapped with ErrValidation as well so
// the HTTP layer's existing 400 mapping continues to work.
//
// Wrapped errors include the reason in their string form ("…:
// multi-statement bodies are not allowed", "…: only SELECT is
// permitted, got InsertStmt", "…: reference to system catalog
// pg_catalog.pg_authid is not allowed", etc.) so the user can see
// what to change.
var ErrUnsafeSQL = errors.New("insights: unsafe sql")

// validateRawSQL is the AST-level guard that gates every insights
// editor query before pgx ever sees it. Three rules:
//
//  1. Exactly one top-level statement. The previous textual
//     `strings.Contains(rawSQL, ";")` check rejected harmless
//     semicolons inside string literals (`SELECT 'a;b'`) while
//     missing comment-hidden injection (`SELECT 1 /* */ ; DROP …`).
//     The AST has a single source of truth: the length of
//     parsed.Stmts. One statement passes, zero or more than one
//     fails.
//
//  2. The statement must be a SELECT. The parse tree's top node is
//     a `*Node_SelectStmt` for SELECT (including UNION/INTERSECT/
//     EXCEPT, set-op trees, VALUES lists, and CTEs whose primary
//     statement is SELECT). Anything else — InsertStmt, UpdateStmt,
//     DeleteStmt, CreateStmt, DropStmt, AlterTableStmt, CopyStmt,
//     CallStmt, ExplainStmt with non-SELECT inner, TransactionStmt,
//     VariableSetStmt, etc. — fails. The existing
//     `SET TRANSACTION READ ONLY` guard in RunRawSQL is kept as a
//     defense-in-depth backstop in case a future Postgres release
//     adds a new statement type we don't yet know about.
//
//  3. No reference to system catalogs. Postgres tables in the
//     `pg_catalog` schema (or `information_schema`) expose role
//     names, password hashes, replication state, and other
//     metadata an editor user has no business reading even with
//     RLS. The walker visits every RangeVar in the parse tree —
//     including subqueries, CTEs, set-ops, and lateral joins —
//     and rejects any with `Schemaname` in {`pg_catalog`,
//     `information_schema`} or a `Relname` that starts with
//     `pg_` and has no explicit schema (so `pg_tables`,
//     `pg_stat_activity`, etc. are blocked even when the user
//     omits the schema qualifier, as Postgres resolves them via
//     the search_path).
//
// Any parse error is surfaced as ErrUnsafeSQL too — the editor's
// contract is "parses cleanly as a single read-only SELECT", and
// "doesn't parse" fails that contract.
func validateRawSQL(rawSQL string) error {
	rawSQL = strings.TrimSpace(rawSQL)
	if rawSQL == "" {
		return wrapUnsafe("raw_sql body required")
	}
	parsed, err := pg_query.Parse(rawSQL)
	if err != nil {
		return wrapUnsafe("sql parse failed: %s", err.Error())
	}
	if len(parsed.Stmts) == 0 {
		return wrapUnsafe("empty parse tree")
	}
	if len(parsed.Stmts) > 1 {
		return wrapUnsafe("multi-statement bodies are not allowed (parsed %d statements)", len(parsed.Stmts))
	}
	top := parsed.Stmts[0].GetStmt()
	if top == nil {
		return wrapUnsafe("empty top-level statement node")
	}
	if top.GetSelectStmt() == nil {
		// Surface the concrete oneof name so the user can see
		// what we parsed it as. The oneof name is `*Node_<X>`
		// where X is the AST node type ("InsertStmt",
		// "UpdateStmt", "AlterTableStmt", etc.) — exactly the
		// label a Postgres docs reader recognises.
		kind := concreteNodeName(top)
		return wrapUnsafe("only SELECT is permitted, got %s", kind)
	}
	// Walk every RangeVar (table reference) in the tree and reject
	// any whose schema/name matches a Postgres system catalog. The
	// walker handles nested SELECTs in CTEs, set-ops, subqueries,
	// lateral joins, sublinks, and EXISTS — all of which can
	// otherwise hide a `pg_authid` reference behind an outer
	// SELECT that looks innocuous on its own.
	var bad *pg_query.RangeVar
	walkProtoForRangeVars(top.ProtoReflect(), func(rv *pg_query.RangeVar) bool {
		if isSystemCatalog(rv) {
			bad = rv
			return false
		}
		return true
	})
	if bad != nil {
		ref := bad.GetRelname()
		if s := bad.GetSchemaname(); s != "" {
			ref = s + "." + ref
		}
		return wrapUnsafe("reference to system catalog %s is not allowed", ref)
	}
	return nil
}

// isSystemCatalog returns true when rv resolves to a Postgres system
// catalog under the standard search_path. The check covers three
// shapes:
//
//   - Explicit schema = pg_catalog or information_schema (any
//     relname).
//   - No explicit schema, relname starts with `pg_` (matches the
//     entire pg_catalog table family that Postgres resolves
//     without qualification when search_path includes it — and it
//     always does for the public user).
//   - Catalog name set (cross-database reference, three-part name
//     like `template1.pg_catalog.pg_authid`) — also rejected, since
//     the only way it parses is as a system catalog reference.
//
// Tenant tables in this repo all use lowercase non-`pg_` names
// (krecords, krecord_versions, journal_entries, etc.), so the
// prefix check has no false positives. If a future user-table is
// ever named `pg_…`, the migration review will catch it; the
// security trade-off (silently fail-open on a future migration
// vs. let an editor user query pg_authid today) lands firmly on
// fail-closed.
func isSystemCatalog(rv *pg_query.RangeVar) bool {
	if rv == nil {
		return false
	}
	if rv.GetCatalogname() != "" {
		return true
	}
	schema := strings.ToLower(rv.GetSchemaname())
	switch schema {
	case "pg_catalog", "information_schema":
		return true
	}
	if schema == "" && strings.HasPrefix(strings.ToLower(rv.GetRelname()), "pg_") {
		return true
	}
	return false
}

// walkProtoForRangeVars walks every nested *RangeVar in the
// protobuf message tree rooted at msg, invoking visit for each one.
// The walk uses protoreflect rather than a hand-rolled switch over
// every Node oneof variant — there are 200+ Node concrete types in
// libpg_query's grammar, and a hand-rolled switch would drift the
// moment Postgres adds a new statement node. Reflection keeps the
// validator stable across pg_query_go upgrades.
//
// visit returns false to terminate the walk early (used by the
// caller to stop on the first system-catalog reference found).
func walkProtoForRangeVars(msg protoreflect.Message, visit func(*pg_query.RangeVar) bool) {
	walkProtoForRangeVarsImpl(msg, visit)
}

// walkProtoForRangeVarsImpl is the recursive worker.
// It returns false the moment visit returns false so all parent
// recursion frames also unwind without doing extra work.
func walkProtoForRangeVarsImpl(msg protoreflect.Message, visit func(*pg_query.RangeVar) bool) bool {
	if msg == nil || !msg.IsValid() {
		return true
	}
	// If this message is a RangeVar, hand it to visit.
	if rv, ok := msg.Interface().(*pg_query.RangeVar); ok {
		return visit(rv)
	}
	cont := true
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			if fd.Kind() != protoreflect.MessageKind {
				return true
			}
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				if !walkProtoForRangeVarsImpl(list.Get(i).Message(), visit) {
					cont = false
					return false
				}
			}
		case fd.IsMap():
			if fd.MapValue().Kind() != protoreflect.MessageKind {
				return true
			}
			mp := v.Map()
			cont2 := true
			mp.Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				if !walkProtoForRangeVarsImpl(mv.Message(), visit) {
					cont2 = false
					return false
				}
				return true
			})
			if !cont2 {
				cont = false
				return false
			}
		case fd.Kind() == protoreflect.MessageKind:
			if !walkProtoForRangeVarsImpl(v.Message(), visit) {
				cont = false
				return false
			}
		}
		return true
	})
	return cont
}

// concreteNodeName returns the protobuf oneof short name of the
// concrete node wrapped by a *pg_query.Node — e.g. "SelectStmt",
// "InsertStmt", "DropStmt". Used only for error messages so the
// user sees a label matching the Postgres docs.
func concreteNodeName(n *pg_query.Node) string {
	if n == nil {
		return "<nil>"
	}
	msg := n.ProtoReflect()
	oneofs := msg.Descriptor().Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		oneof := oneofs.Get(i)
		fd := msg.WhichOneof(oneof)
		if fd == nil {
			continue
		}
		// The oneof field name in Node is the snake_case of the
		// wrapped message type (e.g. "select_stmt"); the actual
		// proto message name on the wrapped value is the
		// CamelCase form ("SelectStmt"). Prefer the wrapped
		// message's own name so the label matches the Postgres
		// node-type vocabulary the user expects.
		v := msg.Get(fd)
		if v.IsValid() {
			if v.Message().Descriptor() != nil {
				return string(v.Message().Descriptor().Name())
			}
		}
		return string(fd.Name())
	}
	return "Unknown"
}

// wrapUnsafe formats an ErrUnsafeSQL-wrapped error that also
// satisfies errors.Is(err, ErrValidation). Stacking both sentinels
// lets the HTTP error mapper keep its existing 400 path while
// future call-sites that care specifically about unsafe-SQL can
// detect it without string matching.
func wrapUnsafe(format string, args ...any) error {
	merged := make([]any, 0, len(args)+2)
	merged = append(merged, ErrValidation, ErrUnsafeSQL)
	merged = append(merged, args...)
	return fmt.Errorf("%w: %w: "+format, merged...)
}
