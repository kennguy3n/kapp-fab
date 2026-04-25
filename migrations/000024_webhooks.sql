-- Phase J — tenant-managed webhooks with delivery log.
--
-- Reference: frappe/frappe Webhook DocType.
--
-- `webhooks` stores per-tenant outbound HTTP subscriptions. Each
-- subscription specifies the target URL, an HMAC-SHA256 signing
-- secret, and an event filter (JSONB array of event-type prefixes —
-- empty matches every event). The worker's notificationRouter scans
-- this table on each drain tick so tenants can add/remove endpoints
-- without restarting the worker.
--
-- `webhook_deliveries` is the append-only delivery log. One row is
-- written per attempt; retries bump the `attempt` column and carry
-- forward the same `event_id` so the UI can render a grouped
-- "attempts for event X" timeline. next_retry_at schedules the next
-- attempt when the previous one failed.
--
-- Both tables follow the canonical multi-tenancy pattern: tenant_id
-- FK + RLS + tenant_isolation policy + GRANT to kapp_app.

CREATE TABLE IF NOT EXISTS webhooks (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    id              UUID NOT NULL,
    url             TEXT NOT NULL,
    secret          TEXT NOT NULL,
    event_filters   JSONB NOT NULL DEFAULT '[]'::jsonb,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS webhooks_tenant_active_idx
    ON webhooks (tenant_id, active);

ALTER TABLE webhooks ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON webhooks;
CREATE POLICY tenant_isolation ON webhooks
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON webhooks TO kapp_app;

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    id              UUID NOT NULL,
    webhook_id      UUID NOT NULL,
    event_id        UUID NOT NULL,
    event_type      TEXT NOT NULL,
    status_code     INT,
    response_body   TEXT,
    attempt         INT NOT NULL DEFAULT 1,
    delivered       BOOLEAN NOT NULL DEFAULT FALSE,
    error           TEXT,
    next_retry_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS webhook_deliveries_webhook_idx
    ON webhook_deliveries (tenant_id, webhook_id, created_at DESC);

CREATE INDEX IF NOT EXISTS webhook_deliveries_retry_idx
    ON webhook_deliveries (next_retry_at)
    WHERE delivered = FALSE AND next_retry_at IS NOT NULL;

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON webhook_deliveries;
CREATE POLICY tenant_isolation ON webhook_deliveries
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_deliveries TO kapp_app;
