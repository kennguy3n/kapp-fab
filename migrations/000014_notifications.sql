-- Phase H — notifications persistence.
--
-- The worker's notification router (services/worker/notifications.go)
-- fans every outbox event carrying a `notification` envelope out to
-- the requested external channel: KChat DM, webhook, or email. Those
-- deliveries are best-effort — if the bridge is down, the webhook
-- endpoint rejects, or SMTP isn't configured, the user still needs to
-- see the notice. This table is the durable in-app inbox that backs
-- the bell / inbox component in the web UI and survives even when
-- every external transport fails.
--
-- Rows are always tenant-scoped; RLS matches every other tenant table
-- so a user inside tenant A never sees tenant B's notices. The
-- `read` flag is what the inbox UI toggles on mark-read, and the
-- `created_at` index supports the common "latest N" query.

CREATE TABLE IF NOT EXISTS notifications (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    user_id         UUID REFERENCES users(id),
    type            TEXT NOT NULL,
    title           TEXT NOT NULL DEFAULT '',
    body            TEXT NOT NULL DEFAULT '',
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    read            BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at         TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS notifications_tenant_user_created_idx
    ON notifications (tenant_id, user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS notifications_unread_idx
    ON notifications (tenant_id, user_id)
    WHERE read = FALSE;

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON notifications;
CREATE POLICY tenant_isolation ON notifications
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
