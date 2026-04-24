-- Phase I — helpdesk module.
--
-- The helpdesk.ticket and helpdesk.sla_policy KTypes live in the
-- generic KRecord store so they inherit RLS, audit, events, and the
-- agent-tool surface for free. Two typed tables back the module:
--
--   * sla_policies — per-tenant SLA response/resolution targets per
--     priority. Denormalised here so the SLA evaluator can resolve a
--     target without decoding a JSONB blob on every ticket create.
--
--   * ticket_sla_log — append-only SLA breach log. Every breach or
--     warning gets a row so the helpdesk page can surface the stream
--     without scanning every ticket's history.
--
-- Both tables follow the canonical multi-tenancy pattern: tenant_id
-- column, composite PK with tenant_id first, RLS enabled, tenant
-- isolation policy keyed off app.tenant_id, and GRANT to kapp_app.

CREATE TABLE IF NOT EXISTS sla_policies (
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    id                   UUID NOT NULL,
    name                 TEXT NOT NULL,
    priority             TEXT NOT NULL CHECK (priority IN ('low', 'medium', 'high', 'urgent')),
    response_minutes     INTEGER NOT NULL CHECK (response_minutes > 0),
    resolution_minutes   INTEGER NOT NULL CHECK (resolution_minutes > 0),
    active               BOOLEAN NOT NULL DEFAULT TRUE,
    created_by           UUID,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, priority, active) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX IF NOT EXISTS sla_policies_active_idx
    ON sla_policies (tenant_id, priority)
    WHERE active;

ALTER TABLE sla_policies ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON sla_policies;
CREATE POLICY tenant_isolation ON sla_policies
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON sla_policies TO kapp_app;

CREATE TABLE IF NOT EXISTS ticket_sla_log (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    id               BIGSERIAL,
    ticket_id        UUID NOT NULL,
    event_kind       TEXT NOT NULL CHECK (event_kind IN ('response_warning', 'response_breach', 'resolution_warning', 'resolution_breach')),
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    details          JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS ticket_sla_log_ticket_idx
    ON ticket_sla_log (tenant_id, ticket_id, occurred_at DESC);

ALTER TABLE ticket_sla_log ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON ticket_sla_log;
CREATE POLICY tenant_isolation ON ticket_sla_log
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON ticket_sla_log TO kapp_app;
