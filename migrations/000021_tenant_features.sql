-- Phase J — per-tenant feature flags.
--
-- Each row is a (tenant, feature_key) gate that the API's
-- feature-flag middleware consults before dispatching to a KApp
-- domain handler. A feature defaults to "enabled" when no row
-- exists for the tenant so a fresh deployment has every feature
-- live; the tenant setup wizard seeds a plan-appropriate subset so
-- free-plan tenants start with only the CRM surface and have to
-- upgrade to unlock finance/inventory/hr/lms/helpdesk/reporting.
--
-- Follows the canonical multi-tenancy pattern: tenant_id column,
-- composite PK with tenant_id first, RLS + tenant_isolation policy,
-- GRANT to kapp_app.

CREATE TABLE IF NOT EXISTS tenant_features (
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    feature_key  TEXT NOT NULL,
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, feature_key)
);

ALTER TABLE tenant_features ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON tenant_features;
CREATE POLICY tenant_isolation ON tenant_features
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_features TO kapp_app;
