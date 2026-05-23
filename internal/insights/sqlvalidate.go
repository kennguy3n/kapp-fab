package insights

import (
	"errors"
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v5"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ErrUnsafeSQL tags an insights-editor SQL body that the parser-based
// validator rejected — multi-statement, non-SELECT (DML/DDL),
// touches a system catalog, calls a system function, or hides a
// non-SELECT statement inside a CTE / subquery. Distinct from
// ErrValidation so callers who want to surface "unsafe SQL, not a
// generic input typo" can do a single errors.Is check; wrapped with
// ErrValidation as well so the HTTP layer's existing 400 mapping
// continues to work.
//
// Wrapped errors include the reason in their string form ("…:
// multi-statement bodies are not allowed", "…: only SELECT is
// permitted, got InsertStmt", "…: reference to system catalog
// pg_catalog.pg_authid is not allowed", "…: data-modifying CTE
// is not allowed (UpdateStmt inside WITH)", "…: call to system
// function pg_read_file is not allowed", etc.) so the user can see
// what to change.
var ErrUnsafeSQL = errors.New("insights: unsafe sql")

// validateRawSQL is the AST-level guard that gates every insights
// editor query before pgx ever sees it. Five rules, in order:
//
//  1. Body must be non-empty (after TrimSpace).
//
//  2. Body must parse via libpg_query. A parse error fails the
//     "submit one statement" contract; surface as ErrUnsafeSQL so
//     the HTTP layer renders a 400 with the parser's own message.
//
//  3. Exactly one top-level statement. The previous textual
//     `strings.Contains(rawSQL, ";")` check rejected harmless
//     semicolons inside string literals (`SELECT 'a;b'`) while
//     missing comment-hidden injection (`SELECT 1 /* */ ; DROP …`).
//     The AST has a single source of truth: the length of
//     parsed.Stmts. One statement passes, zero or more than one
//     fails.
//
//  4. The top-level statement must be a SELECT (including UNION/
//     INTERSECT/EXCEPT, set-op trees, VALUES, and CTEs whose
//     top-level statement is SELECT). Everything else —
//     InsertStmt, UpdateStmt, DeleteStmt, CreateStmt, DropStmt,
//     AlterTableStmt, CopyStmt, CallStmt, ExplainStmt with non-
//     SELECT inner, TransactionStmt, VariableSetStmt, etc. —
//     fails. The existing `SET TRANSACTION READ ONLY` guard in
//     RunRawSQL is kept as defense-in-depth in case a future
//     Postgres release adds a new statement node we don't yet
//     classify.
//
//  5. Walk the entire parse tree and reject any of:
//
//     a. RangeVar pointing at a system catalog (pg_catalog.*,
//     information_schema.*, unqualified pg_*, or any non-empty
//     catalogname). Catches `SELECT * FROM pg_authid` and every
//     way to hide that reference (subquery, CTE, set-op, lateral
//     join, sublink).
//
//     b. Non-SELECT statement node nested below the root. This
//     catches data-modifying CTEs (`WITH x AS
//     (DELETE FROM tbl RETURNING *) SELECT * FROM x`) which parse
//     as a top-level SelectStmt with the DML hidden inside
//     WithClause.Ctes[i].Ctequery. PostgreSQL allows
//     INSERT/UPDATE/DELETE/MERGE inside WITH; we don't. The
//     check is generic — any nested `*Node` whose active oneof
//     field name ends with `_stmt` other than `select_stmt` is
//     rejected — so future stmt-shaped nodes (e.g. a
//     hypothetical TruncateStmt-inside-WITH) are caught without
//     a code change.
//
//     c. FuncCall whose name resolves to a Postgres system
//     function (pg_catalog.*, unqualified pg_*, any non-empty
//     catalog qualifier) OR to a known-dangerous extension
//     function (dblink_*, large-object I/O, etc.). Blocks
//     `SELECT pg_read_file('/etc/passwd')` and
//     `SELECT dblink('…', 'SELECT * FROM pg_authid')` — both
//     have no RangeVar argument that rule 5a would catch, both
//     can bypass RLS/READ ONLY/statement_timeout from inside
//     the function, and neither is something an editor user
//     should be able to invoke. RLS doesn't cover function
//     output, dblink opens a brand-new connection that is not
//     bound by the outer tx's READ ONLY, and the application
//     DB user in production may not be granted
//     pg_read_server_files — but the validator's job is to fail
//     closed at the AST layer rather than rely on role grants
//     downstream.
//
// Scope notes:
//
//   - The contract covers *data access* (RangeVar) and *function
//     execution* (FuncCall). It does not inspect TypeName nodes —
//     a cast like `'1'::pg_catalog.int4` parses with `pg_catalog`
//     in a TypeName.Names list, but the cast neither fetches a
//     row nor calls a server-side function, so it is functionally
//     benign and intentionally outside the validator's scope.
//
//   - The function denylist (rule 5c, isSystemFunction) is a
//     known-extension list, not exhaustive coverage of every
//     possible dangerous extension. New extensions added to the
//     production DB image should be reviewed against this list
//     (see dangerousExtensionFunctions).
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
		// what we parsed it as. The label is the AST node type
		// ("InsertStmt", "UpdateStmt", "AlterTableStmt", etc.) —
		// exactly the label a Postgres docs reader recognises.
		kind := concreteNodeName(top)
		return wrapUnsafe("only SELECT is permitted, got %s", kind)
	}
	// Walk the full tree and apply rules 5a/5b/5c. The walk
	// terminates on the first violation found; the walker honours
	// a `false` return from visit so all parent frames unwind
	// without doing more work.
	//
	// `atRoot` exists to skip the outermost *pg_query.Node — which
	// was already validated as a SelectStmt above — so rule 5b
	// (nested-stmt rejection) only fires on inner Nodes. The walker
	// starts at top.ProtoReflect() and `top` is itself a
	// *pg_query.Node, so walkProtoMessages guarantees the very
	// first `case *pg_query.Node` hit is the root; flipping atRoot
	// to false on first hit means every subsequent Node visit
	// (target_list entries, from_clause entries, CTE bodies, etc.)
	// goes through nodeStmtOneofName.
	var rejection error
	atRoot := true
	walkProtoMessages(top.ProtoReflect(), func(m protoreflect.Message) bool {
		switch n := m.Interface().(type) {
		case *pg_query.RangeVar:
			if isSystemCatalog(n) {
				ref := n.GetRelname()
				if s := n.GetSchemaname(); s != "" {
					ref = s + "." + ref
				}
				rejection = wrapUnsafe("reference to system catalog %s is not allowed", ref)
				return false
			}
		case *pg_query.FuncCall:
			if ref, kind, ok := isSystemFunction(n); ok {
				switch kind {
				case funcKindSystem:
					rejection = wrapUnsafe("call to system function %s is not allowed", ref)
				case funcKindExtension:
					rejection = wrapUnsafe("call to disallowed extension function %s is not allowed", ref)
				default:
					rejection = wrapUnsafe("call to disallowed function %s is not allowed", ref)
				}
				return false
			}
		case *pg_query.Node:
			// The top-level Node was already validated to be a
			// SelectStmt above; skip it so we only inspect
			// nested statement-shaped nodes (e.g. CTE bodies,
			// RangeSubselect inner selects). For nested Nodes,
			// reject any oneof whose field name ends in `_stmt`
			// other than `select_stmt` — that's the generic
			// "no DML/DDL hiding in CTEs/subqueries" rule.
			if atRoot {
				atRoot = false
				return true
			}
			if name, isStmt := nodeStmtOneofName(n); isStmt && name != "select_stmt" {
				kind := concreteNodeName(n)
				rejection = wrapUnsafe("nested non-SELECT statement %s is not allowed (CTEs and subqueries may only contain SELECT)", kind)
				return false
			}
		}
		return true
	})
	if rejection != nil {
		return rejection
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
//     Postgres only supports same-database references in standard
//     SQL. Cross-database is either a system catalog reference or
//     a configuration error; either way, fail closed.
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

// dangerousExtensionFunctions enumerates non-`pg_`-prefixed function
// names from PostgreSQL extensions that the validator must reject
// because they bypass one or more of the editor sandbox's safety
// layers:
//
//   - dblink_* (contrib/dblink): opens a NEW Postgres connection
//     from inside the query. That new connection inherits neither
//     the per-tenant `SET app.tenant_id` GUC (so RLS does not
//     filter), nor the outer `SET TRANSACTION READ ONLY`, nor the
//     `SET statement_timeout`. A single `SELECT dblink('…',
//     'SELECT * FROM pg_authid')` would otherwise leak the entire
//     pg_authid table to the editor user.
//
//   - lo_import / lo_export (large objects): read/write files on
//     the server's filesystem. Require pg_read_server_files /
//     pg_write_server_files in modern Postgres, which the app role
//     normally lacks — but the validator's job is to fail closed at
//     the AST layer rather than rely on role grants downstream.
//
// The list is a denylist (not exhaustive) because the universe of
// extensions is open-ended; new extensions added to the production
// DB image should be reviewed against this map. See the
// validateRawSQL docstring for the scope contract.
//
// Keys are lowercase; isSystemFunction lowercases the leaf name
// before lookup. The map is small enough that linear scan vs.
// map lookup doesn't matter, but a map keeps additions tidy and
// makes the membership test obviously O(1).
var dangerousExtensionFunctions = map[string]struct{}{
	// contrib/dblink — opens new connections, bypassing
	// RLS / READ ONLY / statement_timeout from the outer tx.
	"dblink":                  {},
	"dblink_connect":          {},
	"dblink_connect_u":        {},
	"dblink_disconnect":       {},
	"dblink_exec":             {},
	"dblink_open":             {},
	"dblink_fetch":            {},
	"dblink_close":            {},
	"dblink_cancel_query":     {},
	"dblink_send_query":       {},
	"dblink_get_result":       {},
	"dblink_is_busy":          {},
	"dblink_error_message":    {},
	"dblink_current_query":    {},
	"dblink_get_connections":  {},
	"dblink_get_notify":       {},
	"dblink_get_pkey":         {},
	"dblink_build_sql_insert": {},
	"dblink_build_sql_delete": {},
	"dblink_build_sql_update": {},
	// Large object I/O — read/write server-side files.
	"lo_import": {},
	"lo_export": {},
}

// funcKind distinguishes the two reasons isSystemFunction rejects a
// function call: a Postgres-built-in system function (pg_catalog.*,
// information_schema.*, unqualified pg_*, cross-database calls), or
// a known-dangerous extension function (dblink_*, lo_import/lo_export,
// …). The runner uses this to produce a user-facing error message
// that names the actual reason rather than lumping every blocked
// function under the misleading "system function" label.
type funcKind int

const (
	funcKindNone funcKind = iota
	funcKindSystem
	funcKindExtension
)

// isSystemFunction returns the canonical dotted reference, the kind
// of block (system vs. dangerous extension), and true when fc names
// either a Postgres system function or a known-dangerous extension
// function. Mirrors isSystemCatalog but for function-call AST nodes —
// pg_read_file('/etc/passwd'), pg_catalog.pg_ls_dir('/'),
// pg_stat_get_activity(NULL), dblink('…', '…'), lo_import('…'), etc.
//
// pg_query represents a function name as []*Node where each Node
// wraps a String node holding one dotted component. So
// `pg_read_file` has Funcname [{String "pg_read_file"}] and
// `pg_catalog.pg_read_file` has [{String "pg_catalog"} {String
// "pg_read_file"}]. Three components (catalog.schema.func) only
// parse for cross-database calls, which we also reject.
//
// The function-name rules, in order, return (qualified-name, kind, true):
//
//  1. 3+ parts → reject as system (cross-database call).
//  2. 2 parts, schema is pg_catalog or information_schema → reject
//     as system.
//  3. 1 part starting with `pg_` → reject as system.
//  4. 1 part (or 2 parts with arbitrary schema) matches
//     dangerousExtensionFunctions on its leaf name → reject as
//     extension. The leaf is the last component, so `public.dblink(…)`
//     and bare `dblink(…)` both match. Lookup is case-insensitive.
//
// In production the application DB user normally lacks
// `pg_read_server_files` (so pg_read_file fails at runtime) and
// dblink may not be installed at all, but the validator's job is to
// fail closed at the AST layer rather than rely on role grants or
// extension availability downstream. Same fail-closed rationale as
// isSystemCatalog.
func isSystemFunction(fc *pg_query.FuncCall) (string, funcKind, bool) {
	if fc == nil {
		return "", funcKindNone, false
	}
	parts := fc.GetFuncname()
	if len(parts) == 0 {
		return "", funcKindNone, false
	}
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := p.GetString_(); s != nil {
			names = append(names, s.GetSval())
		}
	}
	if len(names) == 0 {
		return "", funcKindNone, false
	}
	if len(names) >= 3 {
		return strings.Join(names, "."), funcKindSystem, true
	}
	leaf := strings.ToLower(names[len(names)-1])
	if len(names) == 2 {
		schema := strings.ToLower(names[0])
		if schema == "pg_catalog" || schema == "information_schema" {
			return names[0] + "." + names[1], funcKindSystem, true
		}
		if _, bad := dangerousExtensionFunctions[leaf]; bad {
			return names[0] + "." + names[1], funcKindExtension, true
		}
		return "", funcKindNone, false
	}
	name := strings.ToLower(names[0])
	if strings.HasPrefix(name, "pg_") {
		return names[0], funcKindSystem, true
	}
	if _, bad := dangerousExtensionFunctions[leaf]; bad {
		return names[0], funcKindExtension, true
	}
	return "", funcKindNone, false
}

// nodeStmtOneofName returns the active oneof field name for n's
// `node` oneof (e.g. "select_stmt", "insert_stmt", "func_call") and
// true when the field name ends in "_stmt". The "_stmt" suffix is
// libpg_query's convention for top-level statement nodes
// (SelectStmt, InsertStmt, UpdateStmt, …) — every statement-shaped
// concrete type ends in that suffix, so the suffix check is a
// generic way to detect "this is a statement, not an expression"
// without enumerating every concrete type.
//
// Returns ("", false) when n's oneof is not set or n is nil.
func nodeStmtOneofName(n *pg_query.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	msg := n.ProtoReflect()
	oneofs := msg.Descriptor().Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		fd := msg.WhichOneof(oneofs.Get(i))
		if fd == nil {
			continue
		}
		name := string(fd.Name())
		if strings.HasSuffix(name, "_stmt") {
			return name, true
		}
		return name, false
	}
	return "", false
}

// walkProtoMessages walks every nested protobuf message in the tree
// rooted at msg, invoking visit for each one. The walk uses
// protoreflect rather than a hand-rolled switch over every Node
// oneof variant — there are 200+ Node concrete types in
// libpg_query's grammar, and a hand-rolled switch would drift the
// moment Postgres adds a new statement type. Reflection keeps the
// validator stable across pg_query_go upgrades.
//
// visit returns false to terminate the walk early.
func walkProtoMessages(msg protoreflect.Message, visit func(protoreflect.Message) bool) {
	walkProtoMessagesImpl(msg, visit)
}

// walkProtoMessagesImpl is the recursive worker. It returns false
// the moment visit returns false so all parent recursion frames
// also unwind without doing extra work.
func walkProtoMessagesImpl(msg protoreflect.Message, visit func(protoreflect.Message) bool) bool {
	if msg == nil || !msg.IsValid() {
		return true
	}
	if !visit(msg) {
		return false
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
				if !walkProtoMessagesImpl(list.Get(i).Message(), visit) {
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
				if !walkProtoMessagesImpl(mv.Message(), visit) {
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
			if !walkProtoMessagesImpl(v.Message(), visit) {
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
