#!/usr/bin/env bash
#
# upgrade_tier.sh — move one tenant from the shared schema to a
# dedicated schema in the same PostgreSQL cluster.
#
# The Kapp architecture ships every tenant in `public.*` tables
# protected by RLS. A tenant on a premium plan can be promoted to a
# dedicated schema (e.g. `tenant_acme.*`) so noisy-neighbour risk is
# reduced at the cost of a heavier per-tenant footprint. This script
# performs that promotion: it creates the target schema, copies every
# tenant-scoped row, applies an RLS-free variant of each table, and
# rewrites the tenant's routing record to point the API gateway at
# the new schema.
#
# The dedicated-DB and dedicated-cell tiers build on this same flow
# but live in separate scripts; keeping this one schema-only keeps
# the blast radius small.
#
# Usage:
#   DATABASE_URL=postgres://…  ./scripts/upgrade_tier.sh <tenant_uuid>
#
# Prereqs:
#   - psql on PATH
#   - The operator has DB superuser on the cluster (needed to CREATE
#     SCHEMA + copy RLS-protected data across the boundary).
#   - A recent `kapp-backup extract --tenant <tenant_uuid>` dump on
#     hand, in case the migration must be rolled back by replaying
#     the dump into a fresh tenant_id.
#
# Safety: the script runs every mutation in a single transaction so a
# partial copy is never visible to the application. On failure the
# script exits non-zero and leaves the public schema untouched.

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
# Defence-in-depth: the operator already has DB superuser, but the
# tenant ID often flows through a dashboard / ticket before landing
# here and we don't want a stray apostrophe to break out of the
# literal.
if ! [[ "$TENANT_ID" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
  echo "error: invalid tenant UUID: $TENANT_ID" >&2
  exit 2
fi

SCHEMA="tenant_${TENANT_ID//-/_}"

# Table list mirrors services/kapp-backup/main.go#TenantScopedTables.
# Keep these two files in sync when adding a new tenant-scoped table.
# `ktypes` is intentionally excluded — it has no tenant_id column and
# is re-registered at API boot, so copying it per-tenant would crash
# the WHERE clause below.
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
)

read -r -d '' SQL <<SQL || true
BEGIN;
CREATE SCHEMA IF NOT EXISTS "${SCHEMA}";
SQL

for t in "${TABLES[@]}"; do
  SQL+=$'\n'"CREATE TABLE IF NOT EXISTS \"${SCHEMA}\".\"${t}\" (LIKE public.\"${t}\" INCLUDING ALL);"
  SQL+=$'\n'"INSERT INTO \"${SCHEMA}\".\"${t}\" SELECT * FROM public.\"${t}\" WHERE tenant_id = '${TENANT_ID}'::uuid ON CONFLICT DO NOTHING;"
done

SQL+=$'\n'"UPDATE public.tenants SET schema = '${SCHEMA}' WHERE id = '${TENANT_ID}'::uuid;"
SQL+=$'\n'"COMMIT;"

echo "upgrade_tier: promoting ${TENANT_ID} into ${SCHEMA}" >&2
echo "${SQL}" | psql "${DATABASE_URL}" -v ON_ERROR_STOP=1

echo "upgrade_tier: done — restart the API gateway so routing picks up the new schema" >&2
