package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
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

// tierUpgradeTables is the lock-step copy of
// services/kapp-backup/main.go::TenantScopedTables. We duplicate the
// list here (instead of importing the package) because the kapp-backup
// service is a CLI binary, not a library — referencing its `package
// main` from here would be a build-time cycle. The integration test
// asserts the two slices stay byte-identical.
var tierUpgradeTables = []string{
	"user_tenants", "roles", "permissions", "sessions",
	"idempotency_keys", "saved_views", "notifications",
	"krecords", "workflows", "workflow_runs", "approvals", "audit_log", "events",
	"accounts", "journal_entries", "journal_lines", "fiscal_periods",
	"tax_codes", "cost_centers", "bank_accounts", "bank_transactions",
	"inventory_warehouses", "inventory_items", "inventory_moves",
	"leave_ledger", "lesson_progress",
	"files", "base_tables", "base_rows", "docs_documents", "docs_document_versions",
	"forms", "import_jobs", "import_staging",
	"exchange_rates", "sla_policies", "ticket_sla_log", "saved_reports", "scheduled_actions",
	"tenant_features", "tenant_usage",
	"webhooks", "webhook_deliveries", "print_templates", "portal_users",
	"tenant_support_domains", "data_retention_policies",
	"report_schedules", "export_jobs",
	"insights_queries", "insights_dashboards", "insights_dashboard_widgets",
	"insights_query_cache", "insights_shares",
}

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
	schemaName := tierSchemaName(id)
	if err := promoteTenantToSchema(r.Context(), h.adminPool, id, schemaName); err != nil {
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

// tierSchemaName returns the canonical dedicated-schema name for a
// tenant. Mirrors the `tenant_${TENANT_ID//-/_}` interpolation in
// scripts/upgrade_tier.sh.
func tierSchemaName(id uuid.UUID) string {
	s := id.String()
	out := make([]byte, 0, len("tenant_")+len(s))
	out = append(out, []byte("tenant_")...)
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			out = append(out, '_')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// promoteTenantToSchema runs the tier upgrade end-to-end inside one
// admin-pool transaction. The steps mirror scripts/upgrade_tier.sh
// 1:1 so an operator running either path lands the tenant in the
// same final state:
//
//  1. CREATE SCHEMA IF NOT EXISTS tenant_<uuid>
//  2. For each TenantScopedTable: CREATE TABLE IF NOT EXISTS … (LIKE
//     public.<table> INCLUDING ALL) and copy the tenant's rows.
//  3. UPDATE public.tenants SET schema = 'tenant_<uuid>' WHERE id = $1.
func promoteTenantToSchema(ctx context.Context, adminPool *pgxpool.Pool, tenantID uuid.UUID, schemaName string) error {
	if !isSafeIdentifier(schemaName) {
		return errors.New("tier upgrade: refusing unsafe schema name")
	}
	tx, err := adminPool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("tier upgrade: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // safe to call after commit; rollback after commit returns ErrTxClosed which we deliberately ignore
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schemaName)); err != nil {
		return fmt.Errorf("tier upgrade: create schema: %w", err)
	}
	for _, table := range tierUpgradeTables {
		if !isSafeIdentifier(table) {
			return fmt.Errorf("tier upgrade: refusing unsafe table name %q", table)
		}
		createSQL := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %q.%q (LIKE public.%q INCLUDING ALL)`,
			schemaName, table, table,
		)
		if _, err := tx.Exec(ctx, createSQL); err != nil {
			return fmt.Errorf("tier upgrade: create %s.%s: %w", schemaName, table, err)
		}
		copySQL := fmt.Sprintf(
			`INSERT INTO %q.%q SELECT * FROM public.%q WHERE tenant_id = $1 ON CONFLICT DO NOTHING`,
			schemaName, table, table,
		)
		if _, err := tx.Exec(ctx, copySQL, tenantID); err != nil {
			return fmt.Errorf("tier upgrade: copy %s: %w", table, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE public.tenants SET schema = $1 WHERE id = $2`,
		schemaName, tenantID,
	); err != nil {
		return fmt.Errorf("tier upgrade: update tenants.schema: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tier upgrade: commit: %w", err)
	}
	return nil
}

// isSafeIdentifier guards the SQL string interpolation above. We
// quote every identifier with %q so PostgreSQL handles escaping, but
// the additional whitelist makes accidental injection impossible
// even if a typo upstream feeds an attacker-controlled name.
func isSafeIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		case c == '_':
		default:
			return false
		}
	}
	return true
}
