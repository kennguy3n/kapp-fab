-- Workstream 4 — Reporting → warehouse/BI bridge (export direction).
--
-- The platform already reads FROM external warehouses (the insights
-- package: external Postgres datasources, pool manager, SQL
-- validation, embeddable dashboards). This migration adds the export
-- direction: a tenant can register a "warehouse sync" that mirrors a
-- selected set of its sources (krecords KTypes and/or the allowed
-- ledger.* relations) INTO an external Postgres warehouse on a cron
-- schedule, full or incremental. Customers then point their own BI
-- tool (Tableau / Power BI / Metabase / dbt) at the warehouse copy
-- instead of querying kapp directly.
--
-- The destination is NOT a new connection type: it reuses the
-- insights_data_sources row (encrypted connection string, per-tenant
-- pool cache) the tenant already manages for inbound external
-- queries. warehouse_sync_configs.destination_datasource_id FKs that
-- table so a sync can only target a connection the tenant owns, and a
-- datasource cannot be deleted while a sync still references it
-- (ON DELETE RESTRICT).
--
--   * warehouse_sync_configs — one row per registered sync: the
--     destination datasource + schema, the ordered set of sources to
--     export, the cron expression, full-vs-incremental mode, and the
--     per-source incremental watermark state. last_run_* denormalize
--     the most recent run outcome so a listing needs no aggregate
--     join. The worker (warehouse.ActionTypeWarehouseSync) iterates
--     enabled configs whose cron is due, mirroring the report
--     scheduler.
--
--   * warehouse_sync_runs — the auditable run history: one row per
--     executed (or in-flight) sync with status, row/table counts,
--     started/finished timestamps, an error message on failure, and a
--     per-source detail envelope. Composite FK to warehouse_sync_configs
--     cascades a config delete to its run history.
--
-- Both tables follow the canonical tenant-scoped pattern: composite
-- (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, and GRANT to
-- kapp_app. The migration-rls-check CI gate enforces RLS. Reserved
-- migration number for this work is 000094.

-- ---------------------------------------------------------------------------
-- Warehouse sync configs
-- ---------------------------------------------------------------------------
-- destination_datasource_id FKs insights_data_sources (tenant_id, id):
-- the export reuses the tenant's existing external-datasource
-- connection model rather than inventing a second credential store.
-- ON DELETE RESTRICT keeps a sync from dangling when its destination
-- is removed. destination_schema is the target schema in the
-- warehouse the export writes its mirror tables into; it is validated
-- as a SQL identifier by the Go layer before any DDL is emitted.
-- sources is the ordered JSONB array of source keys ("ktype:<name>"
-- or "ledger.<table>") the export walks. watermarks is the per-source
-- incremental cursor state ({"<source>": {...}}), advanced only after
-- a source's rows have landed in the destination, so a failed run
-- re-reads from the last durable cursor instead of skipping rows.
CREATE TABLE IF NOT EXISTS warehouse_sync_configs (
    tenant_id                 UUID    NOT NULL REFERENCES tenants(id),
    id                        UUID    NOT NULL,
    name                      TEXT    NOT NULL,
    destination_datasource_id UUID    NOT NULL,
    destination_schema        TEXT    NOT NULL DEFAULT 'kapp',
    sources                   JSONB   NOT NULL DEFAULT '[]'::jsonb,
    cron_expression           TEXT    NOT NULL,
    mode                      TEXT    NOT NULL DEFAULT 'incremental'
                              CHECK (mode IN ('full', 'incremental')),
    enabled                   BOOLEAN NOT NULL DEFAULT TRUE,
    watermarks                JSONB   NOT NULL DEFAULT '{}'::jsonb,
    last_run_at               TIMESTAMPTZ,
    last_status               TEXT,
    last_error                TEXT,
    created_by                UUID,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id, destination_datasource_id)
        REFERENCES insights_data_sources (tenant_id, id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS warehouse_sync_configs_enabled_idx
    ON warehouse_sync_configs (tenant_id, enabled)
    WHERE enabled = TRUE;

ALTER TABLE warehouse_sync_configs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON warehouse_sync_configs;
CREATE POLICY tenant_isolation ON warehouse_sync_configs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON warehouse_sync_configs TO kapp_app;

-- ---------------------------------------------------------------------------
-- Warehouse sync runs
-- ---------------------------------------------------------------------------
-- The auditable run history. A run row is inserted as 'running' when
-- the export starts and updated to 'success' / 'error' on completion,
-- so an in-flight or crashed run is observable. rows_exported /
-- tables_exported denormalize the totals for a listing; details is a
-- per-source {"<source>": rows} envelope for drill-down. trigger
-- distinguishes a cron-driven run from an operator "run now". Composite
-- FK to the config cascades a config delete to its history.
CREATE TABLE IF NOT EXISTS warehouse_sync_runs (
    tenant_id       UUID    NOT NULL REFERENCES tenants(id),
    id              UUID    NOT NULL,
    config_id       UUID    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running', 'success', 'error')),
    mode            TEXT    NOT NULL
                    CHECK (mode IN ('full', 'incremental')),
    trigger         TEXT    NOT NULL DEFAULT 'schedule'
                    CHECK (trigger IN ('schedule', 'manual')),
    rows_exported   BIGINT  NOT NULL DEFAULT 0 CHECK (rows_exported >= 0),
    tables_exported INTEGER NOT NULL DEFAULT 0 CHECK (tables_exported >= 0),
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    error           TEXT,
    details         JSONB   NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, config_id)
        REFERENCES warehouse_sync_configs (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS warehouse_sync_runs_config_idx
    ON warehouse_sync_runs (tenant_id, config_id, started_at DESC);

ALTER TABLE warehouse_sync_runs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON warehouse_sync_runs;
CREATE POLICY tenant_isolation ON warehouse_sync_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON warehouse_sync_runs TO kapp_app;
