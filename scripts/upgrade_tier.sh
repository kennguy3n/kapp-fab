#!/usr/bin/env bash
#
# upgrade_tier.sh — move one tenant from the shared schema to a
# dedicated schema in the same PostgreSQL cluster.
#
# As of Phase G the actual SQL lives inside the SECURITY DEFINER
# function `public.promote_tenant_to_schema(uuid, text, text[])`
# installed by migrations/000042_tier_admin_role.sql. The function
# is owned by `kapp_tier_admin` (no superuser, no BYPASSRLS) so the
# operator running this script no longer needs cluster-wide DDL
# rights — they just need EXECUTE on the wrapper. The default
# install grants EXECUTE to `kapp_admin`, which is what
# scripts/upgrade_tier.sh and services/api/tier_handlers.go connect
# as.
#
# This script's job is therefore reduced to:
#   1. Validating the tenant UUID
#   2. Building the canonical schema name (`tenant_<uuid_with_dashes_to_underscores>`)
#   3. Sending the matching list of tenant-scoped tables (kept in
#      sync with internal/tenant/tier.go::TenantScopedTables and
#      services/kapp-backup/main.go::TenantScopedTables — drift is
#      caught by services/api/tier_handlers_integration_test.go)
#   4. Calling the SECURITY DEFINER function inside one transaction
#
# Usage:
#   DATABASE_URL=postgres://…  ./scripts/upgrade_tier.sh <tenant_uuid>
#
# Prereqs:
#   - psql on PATH
#   - The operator's connection role has EXECUTE on
#     public.promote_tenant_to_schema(uuid, text, text[]). The
#     default install grants this to kapp_admin.
#   - A recent `kapp-backup extract --tenant <tenant_uuid>` dump on
#     hand, in case the migration must be rolled back by replaying
#     the dump into a fresh tenant_id.

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <tenant_uuid>" >&2
  exit 2
fi
TENANT_ID="$1"
if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "DATABASE_URL must be set" >&2
  exit 2
fi
# $1 is interpolated into single-quoted SQL literals below, so refuse
# anything that isn't a canonical UUID before we build the statement.
if ! [[ "$TENANT_ID" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
  echo "error: invalid tenant UUID: $TENANT_ID" >&2
  exit 2
fi

SCHEMA="tenant_${TENANT_ID//-/_}"

# Table list mirrors internal/tenant/tier.go::TenantScopedTables and
# services/kapp-backup/main.go::TenantScopedTables. The byte-identity
# check in services/api/tier_handlers_integration_test.go fails CI if
# any of the three drift.
TABLES=(
  user_tenants roles permissions sessions
  idempotency_keys saved_views notifications
  krecords workflows workflow_runs approvals audit_log events
  accounts journal_entries journal_lines fiscal_periods tax_codes
  cost_centers bank_accounts bank_transactions
  inventory_warehouses inventory_items inventory_batches inventory_moves
  leave_ledger lesson_progress
  files base_tables base_rows docs_documents docs_document_versions
  forms import_jobs import_staging
  exchange_rates sla_policies ticket_sla_log saved_reports scheduled_actions
  tenant_features tenant_usage
  webhooks webhook_deliveries print_templates portal_users
  tenant_support_domains data_retention_policies
  report_schedules export_jobs
  insights_queries insights_dashboards insights_dashboard_widgets
  insights_query_cache insights_shares
  insights_data_sources insights_embeds
)

# Build a Postgres TEXT[] literal: ARRAY['t1','t2',...]
TABLES_LITERAL="ARRAY["
first=1
for t in "${TABLES[@]}"; do
  if [[ $first -eq 1 ]]; then
    TABLES_LITERAL+="'${t}'"
    first=0
  else
    TABLES_LITERAL+=",'${t}'"
  fi
done
TABLES_LITERAL+="]::text[]"

SQL="BEGIN;
SELECT public.promote_tenant_to_schema('${TENANT_ID}'::uuid, '${SCHEMA}', ${TABLES_LITERAL});
COMMIT;"

echo "upgrade_tier: promoting ${TENANT_ID} into ${SCHEMA}" >&2
echo "${SQL}" | psql "${DATABASE_URL}" -v ON_ERROR_STOP=1

echo "upgrade_tier: done — restart the API gateway so routing picks up the new schema" >&2
