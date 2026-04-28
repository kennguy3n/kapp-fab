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
type PGEvaluator struct {
	pool  *pgxpool.Pool
	cache *platform.LRUCache
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
func (e *PGEvaluator) AuthorizeRecord(
	ctx context.Context,
	tenantID, userID uuid.UUID,
	action, resource string,
	recordAttrs map[string]any,
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
		if !matchesConditions(p.Conditions, userID, recordAttrs) {
			continue
		}
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDenied, action)
}

// ListPermissions returns the permission set for the actor within the tenant.
func (e *PGEvaluator) ListPermissions(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) ([]Permission, error) {
	return e.loadPermissions(ctx, tenantID, userID)
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
	key := cacheKey(tenantID, userID)
	if cached, ok := e.cache.Get(key); ok {
		if perms, ok := cached.([]Permission); ok {
			return perms, nil
		}
	}
	perms, err := e.queryPermissions(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	e.cache.Set(key, perms)
	return perms, nil
}

// maxRoleHierarchyDepth bounds the recursive role-chain walk so a
// pathological cycle (which the application is supposed to prevent at
// write time) cannot stall a request indefinitely.
const maxRoleHierarchyDepth = 5

func (e *PGEvaluator) queryPermissions(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) ([]Permission, error) {
	var out []Permission
	err := platform.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		roles, err := loadUserRoles(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
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
		perms := make([]Permission, 0, 16)
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
				perms = append(perms, p)
			}
		}
		out = perms
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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

// matchesConditions evaluates a permission's conditions JSONB against the
// supplied record attributes. The grammar is intentionally small — extending
// it requires bumping the case list and matching documentation in
// docs/SECURITY_REVIEW.md.
//
// Supported keys:
//
//   - "owner_only": bool — when true, requires attrs["owner"] or
//     attrs["created_by"] to equal the actor's user id.
//   - "status_in": []string — requires attrs["status"] to appear in the
//     list (string-compared).
//
// An empty/missing conditions blob always matches (unconditional grant).
// Unrecognised keys make the rule fail closed: the platform should not
// silently widen access when faced with conditions it cannot evaluate.
func matchesConditions(raw json.RawMessage, userID uuid.UUID, attrs map[string]any) bool {
	if isUnconditional(raw) {
		return true
	}
	var cond map[string]any
	if err := json.Unmarshal(raw, &cond); err != nil {
		return false
	}
	for key, val := range cond {
		switch key {
		case "owner_only":
			b, ok := val.(bool)
			if !ok || !b {
				continue
			}
			if !attrMatchesUser(attrs["owner"], userID) && !attrMatchesUser(attrs["created_by"], userID) {
				return false
			}
		case "status_in":
			list, ok := val.([]any)
			if !ok {
				return false
			}
			cur, _ := attrs["status"].(string)
			match := false
			for _, item := range list {
				if s, ok := item.(string); ok && s == cur {
					match = true
					break
				}
			}
			if !match {
				return false
			}
		default:
			// Unknown condition key — fail closed.
			return false
		}
	}
	return true
}

func attrMatchesUser(attr any, userID uuid.UUID) bool {
	switch v := attr.(type) {
	case uuid.UUID:
		return v == userID
	case string:
		id, err := uuid.Parse(v)
		if err != nil {
			return false
		}
		return id == userID
	case fmt.Stringer:
		id, err := uuid.Parse(v.String())
		if err != nil {
			return false
		}
		return id == userID
	default:
		return false
	}
}
