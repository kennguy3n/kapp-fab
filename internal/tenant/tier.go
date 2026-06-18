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
// services/kapp-backup/main.go::TenantScopedTables and in
// scripts/upgrade_tier.sh::TABLES. All three copies are covered by
// CI lock-step assertions:
//
//   - services/api/tier_handlers_integration_test.go::
//     TestTierUpgradeTablesMatchBackupSourceList covers the
//     services/kapp-backup/main.go copy.
//   - services/api/upgrade_tier_shell_test.go::
//     TestUpgradeTierShellTablesMatchGoList covers the
//     scripts/upgrade_tier.sh copy.
//
// Edit one, edit all three — the tests will tell you if you forget.
var TenantScopedTables = []string{
	"user_tenants", "user_tenant_roles", "roles", "permissions", "sessions",
	"idempotency_keys", "saved_views", "notifications",
	"krecords", "workflows", "workflow_runs", "approvals", "audit_log", "events",
	"accounts", "journal_entries", "journal_lines", "fiscal_periods",
	"tax_codes", "cost_centers", "bank_accounts", "bank_transactions",
	"budgets", "budget_lines",
	"inventory_warehouses", "inventory_items", "inventory_batches", "inventory_moves",
	// Lot/serial tracking (Workstream 2): inventory_serials FKs
	// items/warehouses/batches; the move<->serial junction FKs
	// inventory_moves + inventory_serials, so it trails both.
	"inventory_serials", "inventory_move_serials",
	"boms", "bom_components",
	// work_centers/routings/routing_operations precede work_orders:
	// work_orders.routing_id FKs routings (migration 000080). job_cards
	// FKs work_orders + work_centers, so it trails them.
	"work_centers", "routings", "routing_operations", "work_orders", "job_cards",
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
	"insights_data_sources", "insights_embeds",
	"email_messages", "email_attachments",
	"helpdesk_imap_state", "helpdesk_mailboxes",
	"tenant_ktypes",
	"landed_cost_vouchers", "landed_cost_charges", "landed_cost_targets",
	"cycle_count_sessions", "cycle_count_lines",
	"tenant_record_counts",
	"marketplace_extension_installations",
	"marketplace_extension_ktypes",
	"marketplace_extension_workflows",
	"marketplace_extension_agent_tools",
	"marketplace_webhook_subscriptions",
	"marketplace_dispatch_log",
	// Tenant-authored marketplace ratings (000102). FKs the GLOBAL
	// marketplace_extensions catalog (operator-managed, not in this
	// slice) plus tenants/users, so no marketplace_*_installations
	// ordering dependency; the default (tenant_id, id) PK applies.
	"marketplace_extension_ratings",
	// Session 17 — LMS deep enhancement. Parents precede children so
	// FKs resolve on restore: learning_paths before its courses /
	// enrollments; lms_badges before lms_user_badges;
	// lms_discussion_threads before lms_discussion_replies.
	"learning_paths",
	"learning_path_courses",
	"learning_path_enrollments",
	"lms_xapi_statements",
	"lms_badges",
	"lms_user_badges",
	"lms_discussion_threads",
	"lms_discussion_replies",
	// Recruitment (000084). job_applications FKs job_openings; interviews
	// and offer_letters each FK job_applications, so the parent precedes
	// its children in the FK-restore walk.
	"job_openings",
	"job_applications",
	"interviews",
	"offer_letters",
	// Bank feed + smart reconciliation (000085/000086). bank_feed_connections
	// FKs bank_accounts and bank_match_suggestions FKs bank_transactions —
	// both parents appear earlier in this list. bank_learned_matches has no
	// FK and carries a natural composite PK.
	"bank_feed_connections",
	"bank_reconciliation_rules",
	"bank_match_suggestions",
	"bank_learned_matches",
	// Transfer detection (000091). bank_transfer_pairs FKs
	// bank_transactions (listed earlier), default (tenant_id, id) PK.
	"bank_transfer_pairs",
	// Split reconciliation (000097). bank_transaction_allocations FKs
	// bank_transactions (listed earlier, ON DELETE CASCADE), default
	// (tenant_id, id) PK.
	"bank_transaction_allocations",
	// Workstream 3 (000090) — FX revaluation run audit trail. Default
	// (tenant_id, id) PK, no FK to other tenant-scoped tables.
	"fx_revaluation_runs",
	// Batch-2 manufacturing — MRP run (000092). mrp_runs is the parent;
	// mrp_demand_lines and mrp_planned_orders both FK mrp_runs (ON DELETE
	// CASCADE), so the parent precedes its children in the FK-restore
	// walk. mrp_demand_lines also FKs inventory_items; mrp_planned_orders
	// FKs inventory_items + boms + routings — all listed earlier. All
	// three carry the default (tenant_id, id) PK.
	"mrp_runs",
	"mrp_demand_lines",
	"mrp_planned_orders",
	// Batch-2 manufacturing — subcontracting (000093). subcontract_orders
	// FKs work_orders (listed earlier) + inventory_items/warehouses;
	// subcontract_components FKs subcontract_orders (ON DELETE CASCADE),
	// so the parent precedes its child. Both carry the default
	// (tenant_id, id) PK (the one-row-per-(order,item) rule is a UNIQUE
	// index, not the PK), so neither needs a tableConflictKeys entry.
	"subcontract_orders",
	"subcontract_components",
	// Workstream 4 (000094) — warehouse/BI export bridge.
	// warehouse_sync_configs FKs insights_data_sources (listed earlier
	// in the Insights block) via (tenant_id, destination_datasource_id);
	// warehouse_sync_runs FKs warehouse_sync_configs (ON DELETE CASCADE),
	// so the parent config precedes its run history in the FK-restore
	// walk. Both carry the default (tenant_id, id) PK.
	"warehouse_sync_configs",
	"warehouse_sync_runs",
	// Payroll depth — P1 (000098–000101). The typed payroll run model
	// promoted from the hr.pay_run/hr.payslip KTypes. FK-safe restore
	// order: payroll_runs is the parent; payroll_payslips FKs
	// payroll_runs (ON DELETE CASCADE) and payroll_payslip_lines FKs
	// payroll_payslips (ON DELETE CASCADE); payroll_pay_inputs FKs
	// payroll_runs; payroll_ytd has no FK to a run (it is keyed per
	// employee/tax_year). payroll_ytd uses a NATURAL composite PK
	// (tenant_id, employee_id, tax_year) — NOT the default
	// (tenant_id, id) — so it requires a tableConflictKeys entry in the
	// backup service (services/kapp-backup/main.go); the other four
	// carry the default (tenant_id, id) PK.
	"payroll_runs",
	"payroll_payslips",
	"payroll_payslip_lines",
	"payroll_pay_inputs",
	"payroll_ytd",
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
