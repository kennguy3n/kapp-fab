-- Phase I — report builder.
--
-- Persists user-authored report definitions so the generic report
-- runner (internal/reporting) can execute them against krecords,
-- journal_lines, stock_levels, etc. The definition JSON is schema-less
-- from PostgreSQL's perspective; the Go layer validates data source,
-- columns, filters, aggregations, and chart metadata before every run.
--
-- Follows the canonical multi-tenancy pattern: tenant_id column,
-- composite PK with tenant_id, RLS, tenant_isolation policy, GRANT to
-- kapp_app.

CREATE TABLE IF NOT EXISTS saved_reports (
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    id           UUID NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    definition   JSONB NOT NULL,
    created_by   UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS saved_reports_owner_idx
    ON saved_reports (tenant_id, created_by)
    WHERE created_by IS NOT NULL;

ALTER TABLE saved_reports ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON saved_reports;
CREATE POLICY tenant_isolation ON saved_reports
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON saved_reports TO kapp_app;
