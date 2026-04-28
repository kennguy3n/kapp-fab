package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/authz"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// rolesHandlers exposes CRUD over the per-tenant roles table, the
// granular permissions rows, and the user→role assignments. Mounted
// under /api/v1/roles and /api/v1/users (see services/api/main.go) and
// gated behind authz.Middleware so only tenant.admin actors can manage
// roles.
//
// Mutations always invalidate the authz cache for the affected tenant
// (or the affected (tenant, user) pair) so the next request sees the
// new grants without waiting for the cache TTL.
type rolesHandlers struct {
	pool *pgxpool.Pool
	eval *authz.PGEvaluator
}

type roleDTO struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Permissions json.RawMessage `json:"permissions"`
	ParentRole  string          `json:"parent_role,omitempty"`
}

type permissionDTO struct {
	ID         uuid.UUID       `json:"id"`
	RoleName   string          `json:"role_name"`
	KType      string          `json:"ktype"`
	Action     string          `json:"action"`
	Conditions json.RawMessage `json:"conditions,omitempty"`
	GrantedAt  string          `json:"granted_at,omitempty"`
}

func (h *rolesHandlers) listRoles(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out := make([]roleDTO, 0)
	err := platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT name, COALESCE(parent_role, ''), permissions
			   FROM roles
			  WHERE tenant_id = $1
			  ORDER BY name`,
			t.ID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d roleDTO
			if err := rows.Scan(&d.Name, &d.ParentRole, &d.Permissions); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("list roles: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *rolesHandlers) createRole(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req roleDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "role name required", http.StatusBadRequest)
		return
	}
	if len(req.Permissions) == 0 {
		req.Permissions = json.RawMessage(`[]`)
	}
	err := platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		if req.ParentRole != "" {
			if err := assertNoCycle(ctx, tx, t.ID, req.Name, req.ParentRole); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO roles (tenant_id, name, permissions, parent_role)
			 VALUES ($1, $2, $3, NULLIF($4, ''))
			 ON CONFLICT (tenant_id, name) DO NOTHING`,
			t.ID, req.Name, req.Permissions, req.ParentRole,
		)
		return err
	})
	if err != nil {
		if errors.Is(err, errCycleDetected) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("create role: %v", err), http.StatusInternalServerError)
		return
	}
	h.invalidateTenant(t.ID)
	writeJSON(w, http.StatusCreated, req)
}

func (h *rolesHandlers) updateRole(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "role name required", http.StatusBadRequest)
		return
	}
	var req roleDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	err := platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		if req.ParentRole != "" {
			if err := assertNoCycle(ctx, tx, t.ID, name, req.ParentRole); err != nil {
				return err
			}
		}
		ct, err := tx.Exec(ctx,
			`UPDATE roles
			    SET permissions = COALESCE($3, permissions),
			        parent_role = NULLIF($4, '')
			  WHERE tenant_id = $1 AND name = $2`,
			t.ID, name, req.Permissions, req.ParentRole,
		)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return errRoleNotFound
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errRoleNotFound):
			http.Error(w, "role not found", http.StatusNotFound)
		case errors.Is(err, errCycleDetected):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, fmt.Sprintf("update role: %v", err), http.StatusInternalServerError)
		}
		return
	}
	h.invalidateTenant(t.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *rolesHandlers) deleteRole(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	name := chi.URLParam(r, "name")
	if name == "owner" {
		http.Error(w, "cannot delete the owner role", http.StatusBadRequest)
		return
	}
	err := platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM roles WHERE tenant_id = $1 AND name = $2`,
			t.ID, name,
		)
		return err
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("delete role: %v", err), http.StatusInternalServerError)
		return
	}
	h.invalidateTenant(t.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *rolesHandlers) listPermissions(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	role := chi.URLParam(r, "name")
	out := make([]permissionDTO, 0)
	err := platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, role_name, ktype, action, conditions, granted_at::text
			   FROM permissions
			  WHERE tenant_id = $1 AND role_name = $2 AND revoked_at IS NULL
			  ORDER BY granted_at`,
			t.ID, role,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p permissionDTO
			if err := rows.Scan(&p.ID, &p.RoleName, &p.KType, &p.Action, &p.Conditions, &p.GrantedAt); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("list permissions: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *rolesHandlers) grantPermission(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	role := chi.URLParam(r, "name")
	var req permissionDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Action == "" {
		http.Error(w, "action required", http.StatusBadRequest)
		return
	}
	if len(req.Conditions) == 0 {
		req.Conditions = json.RawMessage(`{}`)
	}
	id := uuid.New()
	err := platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO permissions (id, tenant_id, role_name, ktype, action, conditions, granted_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, role_name, ktype, action)
			 DO UPDATE SET conditions = EXCLUDED.conditions, revoked_at = NULL`,
			id, t.ID, role, req.KType, req.Action, req.Conditions, actorOrDefault(ctx),
		)
		return err
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("grant permission: %v", err), http.StatusInternalServerError)
		return
	}
	h.invalidateTenant(t.ID)
	req.ID = id
	req.RoleName = role
	writeJSON(w, http.StatusCreated, req)
}

func (h *rolesHandlers) revokePermission(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid permission id", http.StatusBadRequest)
		return
	}
	err = platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE permissions SET revoked_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			t.ID, id,
		)
		return err
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("revoke permission: %v", err), http.StatusInternalServerError)
		return
	}
	h.invalidateTenant(t.ID)
	w.WriteHeader(http.StatusNoContent)
}

// listUserRoles returns all role names a user holds in the current tenant.
func (h *rolesHandlers) listUserRoles(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	roles, err := h.eval.ListRoles(r.Context(), t.ID, userID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list user roles: %v", err), http.StatusInternalServerError)
		return
	}
	if roles == nil {
		roles = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": roles})
}

func (h *rolesHandlers) assignUserRole(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Role == "" {
		http.Error(w, "role required", http.StatusBadRequest)
		return
	}
	err = platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO user_tenant_roles (tenant_id, user_id, role_name, granted_by)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT DO NOTHING`,
			t.ID, userID, req.Role, actorOrDefault(ctx),
		)
		return err
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("assign role: %v", err), http.StatusInternalServerError)
		return
	}
	h.invalidateUser(t.ID, userID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *rolesHandlers) removeUserRole(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	role := chi.URLParam(r, "role")
	if role == "" {
		http.Error(w, "role required", http.StatusBadRequest)
		return
	}
	err = platform.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM user_tenant_roles
			  WHERE tenant_id = $1 AND user_id = $2 AND role_name = $3`,
			t.ID, userID, role,
		)
		return err
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("remove role: %v", err), http.StatusInternalServerError)
		return
	}
	h.invalidateUser(t.ID, userID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *rolesHandlers) invalidateTenant(id uuid.UUID) {
	if h.eval == nil {
		return
	}
	h.eval.InvalidateTenant(id)
}

func (h *rolesHandlers) invalidateUser(tenantID, userID uuid.UUID) {
	if h.eval == nil {
		return
	}
	h.eval.InvalidateUser(tenantID, userID)
}

var (
	errRoleNotFound  = errors.New("role not found")
	errCycleDetected = errors.New("parent_role would create a cycle")
)

// assertNoCycle walks the proposed parent chain to ensure assigning
// `parent` as the parent of `name` does not introduce a cycle. The
// migration adds the column nullable so most rows have no parent yet
// and the walk terminates immediately.
func assertNoCycle(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name, parent string) error {
	cur := parent
	for i := 0; i < 16; i++ {
		if cur == "" {
			return nil
		}
		if cur == name {
			return errCycleDetected
		}
		var next *string
		err := tx.QueryRow(ctx,
			`SELECT parent_role FROM roles WHERE tenant_id = $1 AND name = $2`,
			tenantID, cur,
		).Scan(&next)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if next == nil {
			return nil
		}
		cur = *next
	}
	// Bound exceeded — treat as a cycle.
	return errCycleDetected
}
