-- Phase I — scheduled actions engine.
--
-- Durable store of per-tenant background jobs that the worker polls
-- and executes. Each row carries one of two cadence specifiers:
--
--   * cron_expr      — standard 5-field cron expression; the scheduler
--                      uses it to compute the next fire time after a
--                      successful run.
--   * interval_seconds — fixed-delay cadence; preferred for "every N
--                      seconds" polling loops (e.g. SLA breach sweeps)
--                      where a cron expression would be more indirection
--                      than it's worth.
--
-- Handlers register against `action_type` in Go; the scheduler fetches
-- rows whose enabled=true and next_run_at<=now(), dispatches them to
-- the matching handler, then advances next_run_at. The worker tolerates
-- handler errors per-row so a broken action does not starve the others.
--
-- Follows the canonical multi-tenancy pattern: tenant_id column,
-- composite PK with tenant_id first, RLS + tenant_isolation policy,
-- GRANT to kapp_app. The (enabled, next_run_at) index matches the poll
-- query so the worker does not scan the full table every tick.

CREATE TABLE IF NOT EXISTS scheduled_actions (
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    id                UUID NOT NULL DEFAULT gen_random_uuid(),
    action_type       TEXT NOT NULL,
    cron_expr         TEXT,
    interval_seconds  INTEGER,
    next_run_at       TIMESTAMPTZ NOT NULL,
    last_run_at       TIMESTAMPTZ,
    payload           JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    created_by        UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    CHECK (cron_expr IS NOT NULL OR interval_seconds IS NOT NULL),
    CHECK (interval_seconds IS NULL OR interval_seconds > 0)
);

-- The poll query (action_type-agnostic) is:
--   SELECT ... WHERE enabled AND next_run_at <= now()
--     FOR UPDATE SKIP LOCKED LIMIT $1
-- so the worker fans work out across multiple replicas without
-- duplicating runs. Indexing (enabled, next_run_at) keeps the sweep
-- cheap even when most rows are enabled but not yet due.
CREATE INDEX IF NOT EXISTS scheduled_actions_due_idx
    ON scheduled_actions (enabled, next_run_at)
    WHERE enabled;

CREATE INDEX IF NOT EXISTS scheduled_actions_tenant_type_idx
    ON scheduled_actions (tenant_id, action_type);

ALTER TABLE scheduled_actions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON scheduled_actions;
CREATE POLICY tenant_isolation ON scheduled_actions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON scheduled_actions TO kapp_app;
