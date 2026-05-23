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
//     Special case: `SELECT … INTO newtable FROM …` parses as a
//     SelectStmt with a non-nil `IntoClause` and is functionally
//     DDL (creates a table from the result set). It is rejected
//     by the walker (see rule 5d below) so the AST-level contract
//     matches the docstring's "only SELECT" promise both at the
//     root and inside any nested CTE/subquery; READ ONLY tx is
//     the backstop, not the source of truth.
//
//     Special case: `SELECT … FOR UPDATE/SHARE/NO KEY UPDATE/KEY
//     SHARE` parses as a SelectStmt with a non-empty
//     `LockingClause`. PostgreSQL rejects these in a read-only
//     transaction with a runtime error, but the walker rejects
//     them at the AST layer (rule 5d) so the validator surfaces
//     a clean error message both at the root and inside any
//     nested CTE/subquery, and READ ONLY tx remains as defense
//     in depth.
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
//     d. SelectStmt — root or nested — with a non-nil IntoClause
//     (SELECT INTO masquerading as a SELECT) or non-empty
//     LockingClause (FOR UPDATE/SHARE). Both shapes parse as a
//     valid SelectStmt that would otherwise pass rules 5a-5c, so
//     the walker inspects every SelectStmt node and rejects them
//     directly. Running this check inside the walker (rather
//     than only at the root) catches the same shape hidden in
//     CTE bodies, RangeSubselect subqueries, SubLink expression
//     subqueries, and UNION/INTERSECT/EXCEPT arms — all of which
//     would otherwise reach the runtime and produce a less-
//     friendly Postgres error instead of the validator's clean
//     "single source of truth" message.
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
		// Empty/whitespace-only body is "missing input", not
		// "unsafe SQL" — returning ErrUnsafeSQL here would tag a
		// validation typo as an attempted security boundary
		// violation, which is semantically wrong and would skew any
		// monitoring that branches on errors.Is(err, ErrUnsafeSQL).
		// Use the plain ErrValidation surface so the HTTP layer
		// still maps to 400 while keeping the sentinel meaning of
		// ErrUnsafeSQL precise ("AST violation", not "any rejection
		// from the validator").
		return validationErr("raw_sql body required")
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
	// Walk the full tree and apply rules 5a/5b/5c PLUS the per-
	// SelectStmt safety checks (IntoClause = SELECT INTO,
	// LockingClause = FOR UPDATE/SHARE). The walk terminates on
	// the first violation found; the walker honours a `false`
	// return from visit so all parent frames unwind without doing
	// more work.
	//
	// Why the IntoClause / LockingClause checks live in the walker
	// rather than at the top-level `sel := top.GetSelectStmt()`:
	// nested SelectStmts (CTE bodies, RangeSubselect inner selects,
	// SubLink expression subqueries, UNION/INTERSECT/EXCEPT
	// arms) can carry their own IntoClause or LockingClause, and a
	// root-only check would let a query like
	// `WITH x AS (SELECT * FROM krecords FOR UPDATE) SELECT * FROM x`
	// pass the validator and hit the less-friendly Postgres
	// "cannot execute SELECT FOR UPDATE in a read-only transaction"
	// runtime error. Running the check inside the walker uniformly
	// enforces "no SELECT in the tree, root or nested, carries
	// IntoClause or LockingClause" — which is the contract the
	// docstring promises.
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
		case *pg_query.SelectStmt:
			// Applies to root AND every nested SelectStmt (CTE
			// body, subquery in FROM/WHERE/SELECT-target,
			// set-op arm). PostgreSQL itself rejects `SELECT
			// INTO` inside a CTE at execution time with
			// "SELECT ... INTO is not allowed here", but the
			// validator should produce the clean AST-layer
			// message for both root and nested cases —
			// consistency over relying on Postgres to surface
			// the specific error.
			if n.GetIntoClause() != nil {
				rejection = wrapUnsafe("SELECT INTO is not allowed (creates a table from the result set)")
				return false
			}
			if len(n.GetLockingClause()) > 0 {
				rejection = wrapUnsafe("SELECT … FOR UPDATE/SHARE/NO KEY UPDATE/KEY SHARE is not allowed (row locking is not permitted in the read-only editor surface)")
				return false
			}
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
// catalog under the standard search_path. The check covers four
// shapes:
//
//   - Catalog name set (cross-database reference, three-part name
//     like `template1.pg_catalog.pg_authid`) — also rejected, since
//     Postgres only supports same-database references in standard
//     SQL. Cross-database is either a system catalog reference or
//     a configuration error; either way, fail closed.
//   - Explicit schema = pg_catalog or information_schema (any
//     relname).
//   - Any schema (including unset/empty) with a relname that
//     starts with `pg_`. The unqualified case (`pg_authid`)
//     catches the bare reference Postgres resolves via search_path.
//     The schema-qualified case (`public.pg_authid`,
//     `myschema.pg_stat_user_tables`) catches DBA-created wrappers
//     or views that read from real system catalogs — a hostile
//     `CREATE VIEW public.pg_authid AS SELECT * FROM
//     pg_catalog.pg_authid` would otherwise bypass the validator,
//     since `public` is not in the explicit system-schema list.
//     Same fail-closed posture as isSystemFunction's
//     pg_-prefixed-leaf check.
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
	rel := strings.ToLower(rv.GetRelname())
	// Defense in depth: the unqualified `pg_*` check covers
	// `SELECT * FROM pg_authid`. The schema-qualified `pg_*`
	// check below covers `SELECT * FROM public.pg_authid` — a
	// DBA-created view in `public` that wraps `pg_catalog.pg_authid`
	// (or any other system catalog) would otherwise bypass the
	// validator, since `public` is not in the explicit system
	// schema list. PostgreSQL allows user views in the public
	// schema to read from pg_catalog freely; the validator's job
	// is to fail closed at the AST layer rather than rely on the
	// DBA never creating such a view. This mirrors the same
	// pg_-prefixed-leaf check applied to function calls in
	// isSystemFunction — same fail-closed posture for tables and
	// functions.
	if strings.HasPrefix(rel, "pg_") {
		return true
	}
	return false
}

// dangerousFunctions enumerates non-`pg_`-prefixed function names
// that the validator must reject because they bypass one or more
// of the editor sandbox's safety layers, along with how each
// function is classified for the user-facing error message.
// Functions are split between two funcKind values:
//
//   - funcKindSystem: built-in PostgreSQL functions that ship with
//     the core distribution and live in pg_catalog. Their leaf
//     names happen not to start with `pg_` so the prefix check
//     doesn't catch them, but they are genuinely system functions
//     and the error message should say so. set_config, lo_import,
//     and lo_export all fall in this bucket.
//
//   - funcKindExtension: functions provided by a contrib extension
//     that requires CREATE EXTENSION to install. dblink_* is the
//     canonical example. The error message identifies these as
//     "extension functions" so a reader who knows the schema can
//     reason about which extensions to lock down at the role
//     grant layer.
//
// Per-function rationale (alphabetical by family):
//
//   - dblink_* (contrib/dblink, funcKindExtension): opens a NEW
//     Postgres connection from inside the query. That new
//     connection inherits neither the per-tenant
//     `SET app.tenant_id` GUC (so RLS does not filter), nor the
//     outer `SET TRANSACTION READ ONLY`, nor the
//     `SET statement_timeout`. A single `SELECT dblink('…',
//     'SELECT * FROM pg_authid')` would otherwise leak the entire
//     pg_authid table to the editor user.
//
//   - lo_import / lo_export (built-in large-object I/O,
//     funcKindSystem): read/write files on the server's
//     filesystem. Require pg_read_server_files /
//     pg_write_server_files in modern Postgres, which the app role
//     normally lacks — but the validator's job is to fail closed
//     at the AST layer rather than rely on role grants downstream.
//     These are built-in, not from an extension, so they are
//     classified as funcKindSystem.
//
//   - set_config (built-in session GUC mutator, funcKindSystem):
//     `set_config(name, value, is_local)` can change
//     `app.tenant_id` (the GUC RLS policies read) and any other
//     session setting. As a SQL function it runs in the target
//     list of an outer SELECT, so rule 5a (RangeVar) doesn't
//     apply; the only safe move is an explicit denylist. A query
//     like `SELECT set_config('app.tenant_id', 'victim-uuid',
//     true), * FROM krecords` would otherwise attempt to swap the
//     RLS tenant context mid-query. PostgreSQL's RLS qual
//     evaluation is plan-dependent, so relying on "qual runs
//     before target list" is unsafe — fail closed at the AST
//     layer. set_config is a built-in pg_catalog function (just
//     one whose name doesn't start with `pg_`), so the
//     classification is funcKindSystem and the user-facing
//     message says "system function" regardless of whether the
//     caller wrote `set_config(...)` or `pg_catalog.set_config(...)`
//     — same name, same classification.
//
//   - `current_setting(name, missing_ok)` is set_config's
//     read-only cousin and is NOT blocked because reading
//     `app.tenant_id` is benign and useful in queries.
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
var dangerousFunctions = map[string]funcKind{
	// Session GUC mutator — built-in.
	"set_config": funcKindSystem,
	// Large object I/O — built-in.
	"lo_import": funcKindSystem,
	"lo_export": funcKindSystem,
	// contrib/dblink — extension.
	"dblink":                  funcKindExtension,
	"dblink_connect":          funcKindExtension,
	"dblink_connect_u":        funcKindExtension,
	"dblink_disconnect":       funcKindExtension,
	"dblink_exec":             funcKindExtension,
	"dblink_open":             funcKindExtension,
	"dblink_fetch":            funcKindExtension,
	"dblink_close":            funcKindExtension,
	"dblink_cancel_query":     funcKindExtension,
	"dblink_send_query":       funcKindExtension,
	"dblink_get_result":       funcKindExtension,
	"dblink_is_busy":          funcKindExtension,
	"dblink_error_message":    funcKindExtension,
	"dblink_current_query":    funcKindExtension,
	"dblink_get_connections":  funcKindExtension,
	"dblink_get_notify":       funcKindExtension,
	"dblink_get_pkey":         funcKindExtension,
	"dblink_build_sql_insert": funcKindExtension,
	"dblink_build_sql_delete": funcKindExtension,
	"dblink_build_sql_update": funcKindExtension,
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
//     dangerousFunctions on its leaf name → reject with the
//     classification stored in the map (funcKindSystem for
//     built-ins like set_config / lo_import, funcKindExtension
//     for contrib functions like dblink). The leaf is the last
//     component, so `public.dblink(…)` and bare `dblink(…)` both
//     match. Lookup is case-insensitive. The map carries the
//     classification so that bare `set_config(…)` and
//     `pg_catalog.set_config(…)` produce the same user-facing
//     error category instead of one saying "extension function"
//     and the other "system function" for the same underlying
//     built-in.
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
		// Defense in depth: a 2-part name like `public.pg_read_file`
		// would normally resolve to `public.pg_read_file` (which does
		// not exist in a stock Postgres install — the real function
		// lives in pg_catalog) and fail at runtime. But if a DBA
		// ever creates a wrapper `public.pg_read_file(text)` (e.g. a
		// SECURITY DEFINER stub for legitimate admin tooling), an
		// editor user could call it. Treat any `pg_`-prefixed leaf
		// the same regardless of schema — same fail-closed posture
		// as the 1-part check below.
		if strings.HasPrefix(leaf, "pg_") {
			return names[0] + "." + names[1], funcKindSystem, true
		}
		if kind, bad := dangerousFunctions[leaf]; bad {
			return names[0] + "." + names[1], kind, true
		}
		return "", funcKindNone, false
	}
	name := strings.ToLower(names[0])
	if strings.HasPrefix(name, "pg_") {
		return names[0], funcKindSystem, true
	}
	if kind, bad := dangerousFunctions[leaf]; bad {
		return names[0], kind, true
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
