-- Phase F — Importer pipeline.
--
-- The importer drives onboarding of customer data from existing systems
-- (CSV/JSON, Frappe REST APIs, generic REST exports) through a
-- Discover → Export → Normalize → Map → Validate → Stage → Reconcile →
-- Accept → Cutover pipeline. The pipeline state lives in `import_jobs`
-- and staged records land in `import_staging` until the operator
-- accepts them, at which point the accept stage promotes them into the
-- live `krecords` table (or downstream typed ledgers).
--
-- Both tables are tenant-scoped with RLS; the policy body mirrors the
-- Phase A pattern (see migrations/000001_initial_schema.sql) so an
-- import run can't escape the invoking tenant's context. Import jobs
-- count against the per-tenant mutation quota indirectly — each stage
-- transition writes an audit row and emits an event through the shared
-- outbox.

CREATE TABLE IF NOT EXISTS import_jobs (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    source_type     TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN (
                        'pending',
                        'discovering',
                        'exporting',
                        'normalizing',
                        'mapping',
                        'validating',
                        'staging',
                        'reconciling',
                        'accepting',
                        'cutting_over',
                        'completed',
                        'failed'
                    )),
    config          JSONB NOT NULL DEFAULT '{}'::jsonb,
    mapping         JSONB NOT NULL DEFAULT '{}'::jsonb,
    progress        JSONB NOT NULL DEFAULT '{}'::jsonb,
    errors          JSONB NOT NULL DEFAULT '[]'::jsonb,
    reconciliation  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS import_jobs_tenant_status_idx
    ON import_jobs (tenant_id, status, updated_at DESC);

ALTER TABLE import_jobs ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON import_jobs;
CREATE POLICY tenant_isolation ON import_jobs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON import_jobs TO kapp_app;

-- Staging rows — one per source record per import run. Partitioned by
-- tenant_id range so that large imports can be split out onto dedicated
-- partitions and a tenant's accept / cutover vacuum work doesn't scan
-- other tenants.
CREATE TABLE IF NOT EXISTS import_staging (
    id                  BIGSERIAL,
    tenant_id           UUID NOT NULL,
    job_id              UUID NOT NULL,
    source_type         TEXT NOT NULL,
    source_id           TEXT,
    target_ktype        TEXT NOT NULL,
    data                JSONB NOT NULL,
    validation_errors   JSONB NOT NULL DEFAULT '[]'::jsonb,
    status              TEXT NOT NULL CHECK (status IN ('pending','valid','invalid','imported')),
    imported_record_id  UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS import_staging_default PARTITION OF import_staging DEFAULT;

CREATE INDEX IF NOT EXISTS import_staging_tenant_job_idx
    ON import_staging (tenant_id, job_id, status);

CREATE INDEX IF NOT EXISTS import_staging_tenant_source_idx
    ON import_staging (tenant_id, job_id, source_type, source_id);

ALTER TABLE import_staging ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON import_staging;
CREATE POLICY tenant_isolation ON import_staging
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON import_staging TO kapp_app;
GRANT USAGE, SELECT ON SEQUENCE import_staging_id_seq TO kapp_app;
