-- Phase L — Insights engine.
--
-- Tenant-scoped BI layer (saved queries, dashboards, dashboard
-- widgets, query result cache, sharing grants). Every table follows
-- the canonical multi-tenancy invariants: composite tenant_id-led
-- primary key, ENABLE ROW LEVEL SECURITY, tenant_isolation policy,
-- and GRANT to kapp_app. Reference: ARCHITECTURE.md §12 and
-- frappe/insights query/dashboard models.

-- ---------------------------------------------------------------------------
-- Saved query definitions. Definition is JSONB and round-trips through
-- internal/insights.Query (extends reporting.Definition with calculated
-- columns and other BI-grade extensions). cache_ttl_seconds is the
-- per-query TTL applied when materializing into insights_query_cache;
-- 0 disables caching for the query.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS insights_queries (
    tenant_id           UUID    NOT NULL REFERENCES tenants(id),
    id                  UUID    NOT NULL,
    name                TEXT    NOT NULL,
    description         TEXT    NOT NULL DEFAULT '',
    definition          JSONB   NOT NULL,
    cache_ttl_seconds   INT     NOT NULL DEFAULT 300,
    created_by          UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS insights_queries_owner_idx
    ON insights_queries (tenant_id, created_by)
    WHERE created_by IS NOT NULL;

ALTER TABLE insights_queries ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON insights_queries;
CREATE POLICY tenant_isolation ON insights_queries
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON insights_queries TO kapp_app;

-- ---------------------------------------------------------------------------
-- Dashboards. Layout is the JSONB grid definition (see ARCHITECTURE.md §12).
-- auto_refresh_seconds=0 disables auto-refresh; positive values are honoured
-- by the frontend.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS insights_dashboards (
    tenant_id              UUID    NOT NULL REFERENCES tenants(id),
    id                     UUID    NOT NULL,
    name                   TEXT    NOT NULL,
    description            TEXT    NOT NULL DEFAULT '',
    layout                 JSONB   NOT NULL DEFAULT '{}'::jsonb,
    auto_refresh_seconds   INT     NOT NULL DEFAULT 0,
    created_by             UUID,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS insights_dashboards_owner_idx
    ON insights_dashboards (tenant_id, created_by)
    WHERE created_by IS NOT NULL;

ALTER TABLE insights_dashboards ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON insights_dashboards;
CREATE POLICY tenant_isolation ON insights_dashboards
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON insights_dashboards TO kapp_app;

-- ---------------------------------------------------------------------------
-- Per-widget config. dashboard_id + query_id are not foreign keys so the
-- table tolerates a soft-deleted query without breaking the dashboard
-- read path; the runtime resolves missing queries to a "missing query"
-- placeholder. position + config are JSONB.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS insights_dashboard_widgets (
    tenant_id        UUID  NOT NULL REFERENCES tenants(id),
    id               UUID  NOT NULL,
    dashboard_id     UUID  NOT NULL,
    query_id         UUID,
    viz_type         TEXT  NOT NULL DEFAULT 'table',
    position         JSONB NOT NULL DEFAULT '{}'::jsonb,
    config           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS insights_dashboard_widgets_dashboard_idx
    ON insights_dashboard_widgets (tenant_id, dashboard_id);

ALTER TABLE insights_dashboard_widgets ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON insights_dashboard_widgets;
CREATE POLICY tenant_isolation ON insights_dashboard_widgets
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON insights_dashboard_widgets TO kapp_app;

-- ---------------------------------------------------------------------------
-- Materialized query results with TTL. Range-partitioned by tenant_id so
-- a hot tenant cannot bloat a shared tablespace; matches the partitioning
-- pattern used by krecords / events / audit_log. The default partition
-- catches every tenant until ranges are added by the operator.
--
-- query_hash + filter_hash together identify a result; the runner
-- computes them as SHA-256 over canonical_json(definition || filter_params)
-- before any DB lookup so collisions are not possible across queries.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS insights_query_cache (
    tenant_id     UUID  NOT NULL REFERENCES tenants(id),
    query_hash    TEXT  NOT NULL,
    filter_hash   TEXT  NOT NULL DEFAULT '',
    query_id      UUID,
    result        JSONB NOT NULL,
    row_count     INT   NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, query_hash, filter_hash)
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS insights_query_cache_default
    PARTITION OF insights_query_cache DEFAULT;

CREATE INDEX IF NOT EXISTS insights_query_cache_expiry_idx
    ON insights_query_cache (tenant_id, expires_at);

CREATE INDEX IF NOT EXISTS insights_query_cache_query_idx
    ON insights_query_cache (tenant_id, query_id)
    WHERE query_id IS NOT NULL;

ALTER TABLE insights_query_cache ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON insights_query_cache;
CREATE POLICY tenant_isolation ON insights_query_cache
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON insights_query_cache TO kapp_app;

-- ---------------------------------------------------------------------------
-- Per-resource sharing grants. resource_type is 'query' or 'dashboard';
-- grantee_type is 'user' or 'role' and grantee is the corresponding id /
-- role name; permission is 'view' or 'edit'.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS insights_shares (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    id               UUID NOT NULL,
    resource_type    TEXT NOT NULL CHECK (resource_type IN ('query', 'dashboard')),
    resource_id      UUID NOT NULL,
    grantee_type     TEXT NOT NULL CHECK (grantee_type IN ('user', 'role')),
    grantee          TEXT NOT NULL,
    permission       TEXT NOT NULL DEFAULT 'view' CHECK (permission IN ('view', 'edit')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, resource_type, resource_id, grantee_type, grantee)
);

CREATE INDEX IF NOT EXISTS insights_shares_resource_idx
    ON insights_shares (tenant_id, resource_type, resource_id);

ALTER TABLE insights_shares ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON insights_shares;
CREATE POLICY tenant_isolation ON insights_shares
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON insights_shares TO kapp_app;
