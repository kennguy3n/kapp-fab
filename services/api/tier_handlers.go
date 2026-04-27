package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// tierTargetSchema and tierTargetDB are the only target tiers the
// upgrade endpoint accepts. `dedicated_schema` is what
// scripts/upgrade_tier.sh implements today; `dedicated_db` is reserved
// for the cell-router promotion path that lives outside this service
// (see SECURITY_REVIEW.md §8). Anything else is rejected with 400.
const (
	tierTargetSchema = "dedicated_schema"
	tierTargetDB     = "dedicated_db"
)

// tierUpgradeTables is a thin alias over tenant.TenantScopedTables so
// the integration test that asserts byte-identity with the
// kapp-backup slice keeps working without churn. The canonical list
// now lives in internal/tenant/tier.go alongside the Promote
// function that consumes it.
var tierUpgradeTables = tenant.TenantScopedTables

// tierUpgradeHandlers wraps the tier-upgrade HTTP surface. It mirrors
// scripts/upgrade_tier.sh but as a REST endpoint so operators can
// promote a tenant from a runbook without shelling into the database
// host. The handler runs every mutation inside one transaction; on
// any error the operation rolls back and the public schema is left
// untouched, matching the bash script's safety contract.
type tierUpgradeHandlers struct {
	tenants   *tenantHandlers
	adminPool *pgxpool.Pool
	auditor   *audit.PGLogger
}

type tierUpgradeRequest struct {
	TargetTier string `json:"target_tier"`
}

type tierUpgradeResponse struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	TargetTier string    `json:"target_tier"`
	Schema     string    `json:"schema"`
}

func (h *tierUpgradeHandlers) upgrade(w http.ResponseWriter, r *http.Request) {
	if h.adminPool == nil {
		http.Error(w, "tier upgrade requires admin db pool", http.StatusNotImplemented)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	var req tierUpgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	switch req.TargetTier {
	case tierTargetSchema:
		// Implemented inline below.
	case tierTargetDB:
		http.Error(w, "dedicated_db tier upgrade is handled by the cell-router; see SECURITY_REVIEW.md §8", http.StatusNotImplemented)
		return
	default:
		http.Error(w, fmt.Sprintf("unsupported target_tier %q (want %q or %q)", req.TargetTier, tierTargetSchema, tierTargetDB), http.StatusBadRequest)
		return
	}
	t, err := h.tenants.svc.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	schemaName := tenant.SchemaName(id)
	if err := tenant.Promote(r.Context(), h.adminPool, id, schemaName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Best-effort audit. The tier upgrade itself ran in a separate
	// transaction on adminPool, so the audit row goes onto the
	// regular RLS-bound pool — a missing audit entry must not roll
	// the upgrade back.
	if h.auditor != nil {
		afterPayload, _ := json.Marshal(map[string]any{
			"target_tier": tierTargetSchema,
			"schema":      schemaName,
		})
		_ = h.auditor.Log(r.Context(), audit.Entry{
			TenantID:  id,
			ActorKind: audit.ActorSystem,
			Action:    "tenant.tier_upgrade",
			After:     afterPayload,
		})
	}
	writeJSON(w, http.StatusOK, tierUpgradeResponse{
		TenantID:   id,
		TargetTier: tierTargetSchema,
		Schema:     schemaName,
	})
	_ = t // tenant payload unused — kept for the .Get nil-check above.
}

// tierSchemaName is preserved as a thin alias over tenant.SchemaName
// for the existing test fixtures and the few internal callers that
// referenced it before the extraction. New code should call
// tenant.SchemaName directly.
func tierSchemaName(id uuid.UUID) string { return tenant.SchemaName(id) }

// isSafeIdentifier mirrors tenant.IsSafeIdentifier so the legacy
// API-package callers (the integration test fixtures, the audit
// payload formatter) keep compiling without an explicit import.
func isSafeIdentifier(s string) bool { return tenant.IsSafeIdentifier(s) }
