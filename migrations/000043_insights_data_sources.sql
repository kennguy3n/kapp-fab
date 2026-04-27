-- Phase L deferred — Insights external data sources.
--
-- Tenants can register read-only connections to external databases
-- (Postgres only in v1, by design — adding MySQL / Redshift / etc.
-- is a per-dialect handler, not a schema change). Saved queries and
-- dashboards address them via `source: "external:<datasource_id>"`
-- in the reporting Definition; the insights Runner resolves the id,
-- looks up the row under tenant RLS, decrypts the connection string
-- with the per-tenant HKDF key, and routes the query through a
-- bounded LRU pool cache so cold-start cost is paid once per tenant
-- per pool TTL.
--
-- Connection strings and any aux secret blob are stored encrypted
-- with the canonical `kapp:enc:v1:` envelope (see
-- internal/tenant/encryption.go). The DB never sees plaintext.
--
-- Reference: frappe/insights data-source registry.

CREATE TABLE IF NOT EXISTS insights_data_sources (
    tenant_id           UUID    NOT NULL REFERENCES tenants(id),
    id                  UUID    NOT NULL,
    name                TEXT    NOT NULL,
    description         TEXT    NOT NULL DEFAULT '',
    dialect             TEXT    NOT NULL CHECK (dialect IN ('postgres')),
    connection_string   TEXT    NOT NULL,
    secret_blob         TEXT    NOT NULL DEFAULT '',
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    created_by          UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS insights_data_sources_enabled_idx
    ON insights_data_sources (tenant_id, enabled)
    WHERE enabled = TRUE;

ALTER TABLE insights_data_sources ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON insights_data_sources;
CREATE POLICY tenant_isolation ON insights_data_sources
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON insights_data_sources TO kapp_app;
