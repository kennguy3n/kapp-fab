-- Phase J/K — Data retention policies.
--
-- One row per (tenant, category). Categories are well-known table
-- groupings the RetentionSweeper knows how to delete safely:
--
--   audit_log         → audit table older than retention_days
--   events            → outbox events (delivered=true) older than days
--   sla_log           → helpdesk SLA log entries
--   webhook_deliveries→ webhook_delivery_log entries
--   notifications     → notifications inbox rows
--   import_staging    → finished import_jobs + staged rows
--
-- The wizard seeds plan-appropriate defaults (free 90d, starter 180d,
-- enterprise 365d for the sensitive audit_log; lower tiers for the
-- chatty event tables). Operators can override via the
-- /tenants/{id}/retention API.

CREATE TABLE IF NOT EXISTS data_retention_policies (
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    category       TEXT NOT NULL,
    retention_days INT  NOT NULL CHECK (retention_days BETWEEN 1 AND 3650),
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, category)
);

ALTER TABLE data_retention_policies ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS data_retention_policies_isolation ON data_retention_policies;
CREATE POLICY data_retention_policies_isolation ON data_retention_policies
    USING (tenant_id::text = current_setting('app.tenant_id', true));

-- The retention sweeper runs as a background worker under the admin
-- pool. The bypass policy lets it scan every tenant's policies in a
-- single pass; per-delete operations re-acquire tenant context via
-- WithTenantTx so the actual DELETEs run under RLS.
DROP POLICY IF EXISTS data_retention_policies_admin_bypass ON data_retention_policies;
CREATE POLICY data_retention_policies_admin_bypass ON data_retention_policies
    FOR SELECT
    USING (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-000000000000');

COMMENT ON TABLE data_retention_policies IS
    'Per-tenant retention windows by table category. The RetentionSweeper deletes rows older than retention_days for each enabled (tenant, category).';
