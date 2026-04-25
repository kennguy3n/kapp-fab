-- Phase K — Report scheduling.
--
-- Persists the configuration of a periodic report run: which saved
-- report to execute, what cadence, what output format, and which
-- recipients to email. Per-row execution is owned by the worker's
-- ReportScheduleHandler (services/worker/report_scheduler.go) which
-- is triggered by a single tenant-scoped scheduled_actions row of
-- type "report_schedule". The worker iterates `report_schedules`
-- under that tenant context, so a misconfigured cron only re-runs
-- the existing handler — it does not change the row footprint here.
--
-- Reference: frappe/frappe Auto Email Report.
--
-- Follows the canonical multi-tenancy pattern: tenant_id column,
-- composite PK with tenant_id, RLS, tenant_isolation policy, GRANT
-- to kapp_app.

CREATE TABLE IF NOT EXISTS report_schedules (
    tenant_id        UUID         NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    id               UUID         NOT NULL DEFAULT gen_random_uuid(),
    report_id        UUID         NOT NULL,
    name             TEXT         NOT NULL,
    cron_expression  TEXT         NOT NULL,
    format           TEXT         NOT NULL CHECK (format IN ('csv', 'pdf')),
    recipients       JSONB        NOT NULL DEFAULT '[]'::jsonb,
    enabled          BOOLEAN      NOT NULL DEFAULT TRUE,
    last_run_at      TIMESTAMPTZ,
    last_status      TEXT,
    last_error       TEXT,
    created_by       UUID,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, report_id)
        REFERENCES saved_reports (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS report_schedules_enabled_idx
    ON report_schedules (tenant_id, enabled)
    WHERE enabled;

ALTER TABLE report_schedules ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON report_schedules;
CREATE POLICY tenant_isolation ON report_schedules
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON report_schedules TO kapp_app;

COMMENT ON TABLE report_schedules IS
    'Per-tenant report scheduling: cron-driven runs of saved_reports with CSV/PDF email delivery.';
