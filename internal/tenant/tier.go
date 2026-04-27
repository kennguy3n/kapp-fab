package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase G — tier upgrade as a reusable library function.
//
// scripts/upgrade_tier.sh is the operator runbook and the API
// handler at services/api/tier_handlers.go is the REST surface;
// both used to reach into the same private function in the API
// service. Promote moves the implementation here so the runbook,
// the API handler, and any future tenant-service RPC all share one
// path.
//
// The actual schema mutation is done by the SECURITY DEFINER
// function `public.promote_tenant_to_schema(uuid, text, text[])`
// installed by migrations/000042_tier_admin_role.sql. Calling code
// just opens a connection (any role with EXECUTE on the function —
// kapp_admin in the default install) and invokes the function.

// TenantScopedTables is the canonical list of tables that hold
// per-tenant data and must be copied into the dedicated schema on
// upgrade. The order matters for the kapp-backup restore path
// (foreign keys point backwards), so it is duplicated in
// services/kapp-backup/main.go::TenantScopedTables and
// services/api/tier_handlers.go::tierUpgradeTables behind a
// lock-step integration test. Edit one, edit all three.
var TenantScopedTables = []string{
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

// SchemaName returns the canonical dedicated-schema name for a
// tenant. Mirrors the `tenant_${TENANT_ID//-/_}` interpolation in
// scripts/upgrade_tier.sh.
func SchemaName(id uuid.UUID) string {
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

// IsSafeIdentifier guards SQL string interpolation. Used by callers
// that build statements outside the SECURITY DEFINER function (e.g.
// debug tooling or migration test fixtures). The function itself
// re-checks identifiers in plpgsql so this is defence in depth.
func IsSafeIdentifier(s string) bool {
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

// Promote runs the tier upgrade end-to-end against the given pool.
// The caller is expected to provide a pool whose role has EXECUTE
// on public.promote_tenant_to_schema — the api service's adminPool
// (kapp_admin) does today; a scoped operator pool that only has
// EXECUTE on the function works just as well and is the reason the
// SECURITY DEFINER wrapper exists.
//
// The function is idempotent: the SECURITY DEFINER function uses
// CREATE TABLE IF NOT EXISTS and ON CONFLICT DO NOTHING, so calling
// Promote twice is safe.
func Promote(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, schemaName string) error {
	if pool == nil {
		return errors.New("tenant: tier upgrade requires admin db pool")
	}
	if tenantID == uuid.Nil {
		return errors.New("tenant: tier upgrade requires tenant id")
	}
	if !IsSafeIdentifier(schemaName) {
		return errors.New("tenant: tier upgrade refusing unsafe schema name")
	}
	if _, err := pool.Exec(ctx,
		`SELECT public.promote_tenant_to_schema($1, $2, $3)`,
		tenantID, schemaName, TenantScopedTables,
	); err != nil {
		return fmt.Errorf("tenant: tier upgrade: %w", err)
	}
	return nil
}
