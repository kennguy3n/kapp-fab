package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/authz/condition"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// PGEvaluator resolves an actor's role(s) → permission set from the roles,
// permissions, user_tenants and user_tenant_roles tables, with a short-TTL
// LRU cache keyed by tenant+user so hot paths avoid a round-trip on every
// request.
//
// Phase RBAC introduced three concerns the evaluator now folds together:
//
//  1. Multi-role membership (migration 000049): user_tenant_roles can hold
//     more than one role per (tenant_id, user_id). We union permissions
//     from every role the user holds.
//  2. Role hierarchy (migration 000050): each role can declare a parent;
//     permissions are inherited through the chain (depth-bounded to 5).
//  3. Wildcard actions: a permission action like "finance.*" or "*"
//     matches any specific action under that namespace.
//  4. Conditions: the permissions table carries a JSONB conditions
//     column that AuthorizeRecord evaluates against record attributes
//     (e.g. {"owner_only": true}).
//
// To keep AuthorizeRecord cheap on the hot path the cache holds a
// permissionSet, not a bare []Permission: each conditions blob is
// compiled to a *condition.Compiled exactly once at load time and reused
// across every subsequent Eval until the cache entry expires. The user's
// roles are stashed in the same value so AuthorizeRecord can populate
// actor.roles without a second DB round-trip.
type PGEvaluator struct {
	pool  *pgxpool.Pool
	cache *platform.LRUCache
}

// permissionSet is the in-cache representation of an actor's
// authorization context. It pairs each Permission with its
// pre-compiled condition AST and records the actor's role list so
// the condition evaluator can resolve actor.roles references at
// Eval time without re-fetching from the DB.
type permissionSet struct {
	perms []compiledPermission
	roles []string
}

// compiledPermission keeps the raw permission row alongside its
// pre-compiled condition AST. compiled is nil when the conditions
// blob is unconditional (a non-conditional grant — no AST needed).
type compiledPermission struct {
	perm     Permission
	compiled *condition.Compiled
}

// NewPGEvaluator binds to the shared pool and cache. Supply a cache with a
// short TTL (e.g. 30s) so role changes propagate quickly.
func NewPGEvaluator(pool *pgxpool.Pool, cache *platform.LRUCache) *PGEvaluator {
	return &PGEvaluator{pool: pool, cache: cache}
}

// Authorize looks up the actor's permission set and checks whether the action
// is permitted. Wildcards in the permission action are supported via
// matchAction. A permission with non-empty conditions is treated as
// conditional and never satisfies a non-record check — call AuthorizeRecord
// with the record attributes to evaluate conditional grants.
func (e *PGEvaluator) Authorize(
	ctx context.Context,
	tenantID, userID uuid.UUID,
	action, resource string,
) error {
	perms, err := e.loadPermissions(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	for _, p := range perms {
		if !matchAction(p.Action, action) {
			continue
		}
		if !matchResource(p.Resource, resource) {
			continue
		}
		if !isUnconditional(p.Conditions) {
			continue
		}
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDenied, action)
}

// AuthorizeRecord evaluates the actor's permissions against an attribute bag
// describing the record. Conditional permissions only match when their
// conditions JSONB is satisfied by the supplied attributes; unconditional
// permissions behave exactly like Authorize.
//
// Condition evaluation delegates to the internal/authz/condition package
// which implements the production ABAC AST (typed operators, whitelisted
// actor refs, depth-bounded parser, fail-closed semantics). The legacy
// owner_only / status_in payloads stored in existing permissions.conditions
// rows are auto-translated into the canonical AST at parse time, so this
// switch-over does not require a database migration.
//
// Hot-path note: the conditions AST is compiled once at permission-load
// time and cached alongside the Permission. AuthorizeRecord invokes
// (*condition.Compiled).Eval directly — no JSON re-parse, no regex
// recompile per request — so the marginal cost of a conditional grant
// over a non-conditional grant is one tree-walk.
func (e *PGEvaluator) AuthorizeRecord(
	ctx context.Context,
	tenantID, userID uuid.UUID,
	action, resource string,
	recordAttrs map[string]any,
) error {
	set, err := e.loadPermissionSet(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	actor := condition.Actor{
		UserID:   userID,
		TenantID: tenantID,
		Roles:    set.roles,
	}
	for _, p := range set.perms {
		if !matchAction(p.perm.Action, action) {
			continue
		}
		if !matchResource(p.perm.Resource, resource) {
			continue
		}
		ok, evalErr := evalCompiled(p.compiled, actor, recordAttrs)
		if evalErr != nil || !ok {
			// Eval errors mean the policy AST encountered an
			// edge case the compile-time validator couldn't
			// fully rule out (e.g. a runtime regex panic via a
			// pathological RE2-immune pattern) — treat as deny.
			// We intentionally don't log here; the audit log
			// layer captures the deny outcome with the failing
			// permission row id.
			continue
		}
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDenied, action)
}

// evalCompiled is a thin wrapper that treats a nil *Compiled as
// "unconditional grant". Pre-compiled conditions live alongside each
// Permission in the cached permissionSet; an unconditional permission
// row carries a nil *Compiled so we don't pay for a tree-walk over an
// empty allOf{}.
func evalCompiled(c *condition.Compiled, actor condition.Actor, attrs map[string]any) (bool, error) {
	if c == nil {
		return true, nil
	}
	return c.Eval(actor, attrs)
}

// ListPermissions returns the permission set for the actor within the tenant.
func (e *PGEvaluator) ListPermissions(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) ([]Permission, error) {
	set, err := e.loadPermissionSet(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Permission, len(set.perms))
	for i, p := range set.perms {
		out[i] = p.perm
	}
	return out, nil
}

// ListRoles returns the role names the actor holds in the tenant. Multi-role
// membership is represented in user_tenant_roles; the legacy
// user_tenants.role column is consulted as a fallback so tenants that have
// not yet been backfilled keep working.
func (e *PGEvaluator) ListRoles(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) ([]string, error) {
	var roles []string
	err := platform.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		roles, rerr = loadUserRoles(ctx, tx, tenantID, userID)
		return rerr
	})
	if err != nil {
		return nil, err
	}
	return roles, nil
}

// InvalidateUser drops the cached permission set for a single (tenant, user)
// pair. Call this from role-management writes so the next request sees the
// new grants without waiting for the cache TTL.
func (e *PGEvaluator) InvalidateUser(tenantID, userID uuid.UUID) {
	if e.cache == nil {
		return
	}
	e.cache.Delete(cacheKey(tenantID, userID))
}

// InvalidateTenant drops every cached entry whose key is scoped to the
// supplied tenant. Used on role-definition or permission-row mutations
// where the affected user set is open-ended.
func (e *PGEvaluator) InvalidateTenant(tenantID uuid.UUID) {
	if e.cache == nil {
		return
	}
	e.cache.DeletePrefix(tenantPrefix(tenantID))
}

func (e *PGEvaluator) loadPermissions(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) ([]Permission, error) {
	set, err := e.loadPermissionSet(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Permission, len(set.perms))
	for i, p := range set.perms {
		out[i] = p.perm
	}
	return out, nil
}

// loadPermissionSet is the canonical cache-aware loader. It returns
// the pre-compiled permission set (each condition AST built once) plus
// the actor's role list. Callers wanting the raw []Permission shape
// for back-compat should go through loadPermissions, which projects
// this back to the simpler type.
func (e *PGEvaluator) loadPermissionSet(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) (*permissionSet, error) {
	key := cacheKey(tenantID, userID)
	if cached, ok := e.cache.Get(key); ok {
		if set, ok := cached.(*permissionSet); ok {
			return set, nil
		}
	}
	set, err := e.queryPermissionSet(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	e.cache.Set(key, set)
	return set, nil
}

// maxRoleHierarchyDepth bounds the recursive role-chain walk so a
// pathological cycle (which the application is supposed to prevent at
// write time) cannot stall a request indefinitely.
const maxRoleHierarchyDepth = 5

func (e *PGEvaluator) queryPermissionSet(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) (*permissionSet, error) {
	set := &permissionSet{}
	err := platform.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		roles, err := loadUserRoles(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		set.roles = roles
		if len(roles) == 0 {
			return nil
		}
		// Expand each role through the parent_role chain so children
		// inherit ancestor permissions. Deduplicate so a role reached
		// via multiple chains is only walked once.
		expanded := make(map[string]struct{}, len(roles))
		for _, r := range roles {
			if err := walkRoleChain(ctx, tx, tenantID, r, maxRoleHierarchyDepth, expanded); err != nil {
				return err
			}
		}
		seen := make(map[string]struct{}, 16)
		perms := make([]compiledPermission, 0, 16)
		for role := range expanded {
			rolePerms, err := loadRolePermissions(ctx, tx, tenantID, role)
			if err != nil {
				return err
			}
			for _, p := range rolePerms {
				k := permKey(p)
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
				perms = append(perms, compilePermission(p))
			}
		}
		set.perms = perms
		return nil
	})
	if err != nil {
		return nil, err
	}
	return set, nil
}

// compilePermission pre-builds the condition AST so AuthorizeRecord can
// Eval without re-parsing JSON on every request. Unconditional grants
// (empty / {} / null conditions blob) leave compiled=nil — the hot path
// short-circuits on the nil pointer instead of walking an empty allOf{}
// AST. Note that a parse error here still produces a non-nil compiled
// (the condition package returns a fail-closed *Compiled, not an
// error): a typo in stored conditions denies access rather than
// preventing the evaluator from loading altogether.
func compilePermission(p Permission) compiledPermission {
	if condition.IsUnconditional(p.Conditions) {
		return compiledPermission{perm: p}
	}
	// (*Compiled, error) — the error path is reserved for unrecoverable
	// I/O and isn't reachable from a synchronous Compile; schema-level
	// errors fold into a sticky-false Compiled. We pass the result
	// through either way; AuthorizeRecord treats a fail-closed Compiled
	// the same as a deny.
	c, _ := condition.Compile(p.Conditions)
	return compiledPermission{perm: p, compiled: c}
}

// loadUserRoles returns the set of role names held by the user in the
// tenant. user_tenant_roles is the source of truth; we fall back to the
// legacy user_tenants.role column when the new table has no rows for the
// pair (e.g. a tenant that pre-dates the multi-role migration and has not
// been backfilled).
func loadUserRoles(
	ctx context.Context,
	tx pgx.Tx,
	tenantID, userID uuid.UUID,
) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT role_name FROM user_tenant_roles
		 WHERE tenant_id = $1 AND user_id = $2
		 ORDER BY role_name`,
		tenantID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("authz: lookup user_tenant_roles: %w", err)
	}
	roles := make([]string, 0, 2)
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, fmt.Errorf("authz: scan user_tenant_roles: %w", err)
		}
		if r != "" {
			roles = append(roles, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authz: iterate user_tenant_roles: %w", err)
	}
	if len(roles) > 0 {
		return roles, nil
	}
	// Legacy fallback: single-role membership column.
	var role string
	err = tx.QueryRow(ctx,
		`SELECT role FROM user_tenants
		 WHERE tenant_id = $1 AND user_id = $2 AND status = 'active'`,
		tenantID, userID,
	).Scan(&role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("authz: lookup legacy role: %w", err)
	}
	if role == "" {
		return nil, nil
	}
	return []string{role}, nil
}

// walkRoleChain populates `seen` with the role and every reachable parent up
// to maxDepth ancestors. Cycles are bounded by maxDepth and by the dedup
// check on the seen map.
func walkRoleChain(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	role string,
	depth int,
	seen map[string]struct{},
) error {
	cur := role
	for i := 0; i <= depth; i++ {
		if _, ok := seen[cur]; ok {
			return nil
		}
		seen[cur] = struct{}{}
		var parent *string
		err := tx.QueryRow(ctx,
			`SELECT parent_role FROM roles WHERE tenant_id = $1 AND name = $2`,
			tenantID, cur,
		).Scan(&parent)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("authz: lookup parent_role: %w", err)
		}
		if parent == nil || *parent == "" || *parent == cur {
			return nil
		}
		cur = *parent
	}
	return nil
}

// loadRolePermissions returns the permission set bound to a single role,
// preferring fine-grained rows in `permissions` and falling back to the
// JSONB blob on `roles` when the granular table has no rows for the role.
func loadRolePermissions(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	role string,
) ([]Permission, error) {
	rows, err := tx.Query(ctx,
		`SELECT action, ktype, conditions FROM permissions
		 WHERE tenant_id = $1 AND role_name = $2 AND revoked_at IS NULL`,
		tenantID, role,
	)
	if err != nil {
		return nil, fmt.Errorf("authz: lookup permissions rows: %w", err)
	}
	perms := make([]Permission, 0)
	for rows.Next() {
		var p Permission
		var cond json.RawMessage
		if err := rows.Scan(&p.Action, &p.Resource, &cond); err != nil {
			rows.Close()
			return nil, fmt.Errorf("authz: scan permission: %w", err)
		}
		p.Conditions = cond
		perms = append(perms, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authz: iterate permissions: %w", err)
	}
	if len(perms) > 0 {
		return perms, nil
	}
	// Legacy JSONB fallback.
	var raw json.RawMessage
	err = tx.QueryRow(ctx,
		`SELECT permissions FROM roles WHERE tenant_id = $1 AND name = $2`,
		tenantID, role,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("authz: lookup permissions: %w", err)
	}
	return parsePermissions(raw), nil
}

// parsePermissions accepts either a JSON array of permission objects or a
// flat array of action strings (the format used by ARCHITECTURE.md §6 sample
// YAML).
func parsePermissions(raw json.RawMessage) []Permission {
	if len(raw) == 0 {
		return nil
	}
	var asObjs []Permission
	if err := json.Unmarshal(raw, &asObjs); err == nil && len(asObjs) > 0 {
		return asObjs
	}
	var asStrings []string
	if err := json.Unmarshal(raw, &asStrings); err == nil {
		out := make([]Permission, 0, len(asStrings))
		for _, a := range asStrings {
			out = append(out, Permission{Action: a})
		}
		return out
	}
	return nil
}

// matchAction reports whether the supplied permission pattern matches the
// concrete action string. Wildcards are supported in two flavours:
//
//   - "*" matches any action.
//   - "<prefix>.*" matches any action that begins with "<prefix>." — for
//     example, "finance.*" matches "finance.invoice.write" and
//     "finance.account.read", but does NOT match "finance" alone or
//     "finance_other.read".
//
// Bare patterns (no trailing ".*") are matched verbatim — "finance" does
// NOT match "finance.invoice.write".
func matchAction(pattern, action string) bool {
	if pattern == "*" {
		return true
	}
	if pattern == action {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, ".*")
		if prefix == "" {
			return true
		}
		return strings.HasPrefix(action, prefix+".")
	}
	return false
}

// matchResource keeps the permissive existing semantics: an empty resource on
// either side matches everything. Future ABAC extensions can tighten this.
func matchResource(pattern, resource string) bool {
	if pattern == "" || resource == "" {
		return true
	}
	return pattern == resource
}

func permKey(p Permission) string {
	return p.Action + "\x00" + p.Resource + "\x00" + string(p.Conditions)
}

func cacheKey(tenantID, userID uuid.UUID) string {
	return fmt.Sprintf("authz:%s:%s", tenantID, userID)
}

func tenantPrefix(tenantID uuid.UUID) string {
	return fmt.Sprintf("authz:%s:", tenantID)
}

// isUnconditional reports whether the conditions blob is effectively empty
// (nil, empty, or an empty JSON object).
func isUnconditional(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return true
	}
	return false
}

// matchesConditions is a thin compatibility shim over the
// internal/authz/condition package. It exists for store_test.go only;
// production code goes through AuthorizeRecord which evaluates via the
// pre-compiled *condition.Compiled stored in the permissionSet cache.
// New code should reach for condition.Compile / condition.EvalRaw
// directly.
//
// The signature carries tenantID alongside userID even though the
// legacy two-key DSL only consulted userID, so policies that compare
// against actor.tenant_id can be exercised from tests without the
// helper silently denying on a uuid.Nil tenant. Roles are still nil
// because the test helper has no transaction to consult
// loadUserRoles against — tests that need actor.roles should call
// condition.EvalRaw directly with a hand-built Actor.
func matchesConditions(raw json.RawMessage, userID, tenantID uuid.UUID, attrs map[string]any) bool {
	ok, err := condition.EvalRaw(raw, condition.Actor{UserID: userID, TenantID: tenantID}, attrs)
	if err != nil {
		return false
	}
	return ok
}
