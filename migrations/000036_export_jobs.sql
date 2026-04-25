-- Phase K — Per-tenant data export jobs.
--
-- Tracks user-initiated exports: a single KType (or `*` for the
-- whole tenant) produced as CSV or JSON. The worker
-- (services/worker/export_worker.go) walks the queue with FOR UPDATE
-- SKIP LOCKED, runs the export, persists the resulting payload, and
-- flips status to `completed`. Download is served from the API by
-- streaming `payload` back to the user.
--
-- Reference: frappe/frappe Data Export — per-site export with
-- DocType selection and file format.
--
-- Follows the canonical multi-tenancy pattern: tenant_id column,
-- composite PK, RLS, tenant_isolation policy, GRANT to kapp_app.

CREATE TABLE IF NOT EXISTS export_jobs (
    tenant_id      UUID        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    id             UUID        NOT NULL DEFAULT gen_random_uuid(),
    ktype          TEXT        NOT NULL,
    format         TEXT        NOT NULL CHECK (format IN ('csv', 'json')),
    status         TEXT        NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    progress_pct   SMALLINT    NOT NULL DEFAULT 0
                   CHECK (progress_pct BETWEEN 0 AND 100),
    row_count      BIGINT      NOT NULL DEFAULT 0,
    payload        BYTEA,
    error          TEXT,
    file_name      TEXT        NOT NULL,
    content_type   TEXT        NOT NULL DEFAULT 'application/octet-stream',
    created_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS export_jobs_status_idx
    ON export_jobs (tenant_id, status, created_at);

CREATE INDEX IF NOT EXISTS export_jobs_pending_idx
    ON export_jobs (created_at)
    WHERE status = 'pending';

ALTER TABLE export_jobs ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON export_jobs;
CREATE POLICY tenant_isolation ON export_jobs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON export_jobs TO kapp_app;

COMMENT ON TABLE export_jobs IS
    'Per-tenant async export queue: status pending|running|completed|failed, output stored in payload BYTEA.';
COMMENT ON COLUMN export_jobs.ktype IS
    'Single KType to export, e.g. "crm.lead". Use the literal value "*" to request a tenant-wide dump.';
