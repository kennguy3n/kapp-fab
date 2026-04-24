package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// PGEvaluator resolves an actor's role → permission set from the roles and
// user_tenants tables, with a short-TTL LRU cache keyed by tenant+user so hot
// paths avoid a round-trip on every request.
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
// is permitted. Resource is accepted for interface parity with the ABAC path
// but permission matching is action-only for Phase A.
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
		if p.Action == action && (p.Resource == "" || resource == "" || p.Resource == resource) {
			return nil
		}
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

func (e *PGEvaluator) queryPermissions(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) ([]Permission, error) {
	var out []Permission
	err := platform.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx,
			`SELECT role FROM user_tenants
			 WHERE tenant_id = $1 AND user_id = $2 AND status = 'active'`,
			tenantID, userID,
		).Scan(&role)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("authz: lookup role: %w", err)
		}
		// Phase H: the permissions table carries fine-grained grants
		// per (role, ktype, action). When rows exist we use those as
		// the authoritative set; when the table is empty for this
		// role we fall back to the legacy roles.permissions JSONB
		// blob so existing tenants keep working without a backfill.
		rows, err := tx.Query(ctx,
			`SELECT action, ktype FROM permissions
			 WHERE tenant_id = $1 AND role_name = $2 AND revoked_at IS NULL`,
			tenantID, role,
		)
		if err != nil {
			return fmt.Errorf("authz: lookup permissions rows: %w", err)
		}
		perms := make([]Permission, 0)
		for rows.Next() {
			var p Permission
			if err := rows.Scan(&p.Action, &p.Resource); err != nil {
				rows.Close()
				return fmt.Errorf("authz: scan permission: %w", err)
			}
			perms = append(perms, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("authz: iterate permissions: %w", err)
		}
		if len(perms) > 0 {
			out = perms
			return nil
		}
		var raw json.RawMessage
		err = tx.QueryRow(ctx,
			`SELECT permissions FROM roles WHERE tenant_id = $1 AND name = $2`,
			tenantID, role,
		).Scan(&raw)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("authz: lookup permissions: %w", err)
		}
		out = parsePermissions(raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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

func cacheKey(tenantID, userID uuid.UUID) string {
	return fmt.Sprintf("authz:%s:%s", tenantID, userID)
}
