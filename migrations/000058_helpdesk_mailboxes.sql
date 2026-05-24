-- Phase L (PR-7 Surface F) — per-tenant helpdesk mailbox config.
--
-- The pre-PR-7 inbound-email pipeline relied on `tenant_support_domains`
-- alone: an inbound `To: support@<domain>` was mapped to a tenant via
-- the recipient host. That covers the webhook-receiver flow (Mailgun /
-- Postmark / SES → relay → API webhook) but says nothing about the
-- IMAP-poller flow (Surface B). For a tenant to attach an inbox via
-- IMAP we need a row that carries:
--
--   - which IMAP server to talk to
--   - which credentials to use (referenced by *name*, never stored in
--     the DB — see `secret_ref` below)
--   - which folder to poll
--   - the poll cadence + backoff cap
--   - whether the mailbox is enabled (operator can pause without
--     deleting the row)
--
-- This is intentionally a NEW table rather than an extension of
-- `tenant_support_domains`. The recipient-host → tenant lookup
-- (used by the webhook path) and the mailbox-config lookup (used by
-- the IMAP path) have different cardinalities (one tenant has many
-- hosts; one tenant has zero or more mailboxes), different lifecycles
-- (hosts rarely change; mailboxes get rotated when credentials roll),
-- and different access patterns (hosts are read on every inbound
-- request; mailboxes are read once at worker start). Keeping them
-- separate also leaves `tenant_support_domains` untouched, so the
-- webhook-receiver path is unaffected by this migration.
--
-- Secrets are referenced by string key (e.g. `vault://kapp/helpdesk/
-- <tenant-uuid>/imap-password` or `env:KAPP_HELPDESK_PASSWORD_<TAG>`)
-- — never stored as plaintext. The worker resolves the ref via the
-- SecretProvider abstraction (PR-6) just before opening the IMAP
-- session. This means a DB dump never carries credential material,
-- and rotating an IMAP password is "update the secret store +
-- restart the worker" without any SQL change.

CREATE TABLE IF NOT EXISTS helpdesk_mailboxes (
    tenant_id              UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    mailbox_id             UUID NOT NULL,
    name                   TEXT NOT NULL,
    imap_host              TEXT NOT NULL,
    imap_port              INTEGER NOT NULL DEFAULT 993,
    imap_username          TEXT NOT NULL,
    -- Logical key resolved by SecretProvider. Never the plaintext
    -- password. Empty string is rejected by the application layer
    -- so an operator can't accidentally leave the field blank and
    -- have the worker fail open.
    imap_password_ref      TEXT NOT NULL,
    imap_use_tls           BOOLEAN NOT NULL DEFAULT TRUE,
    folder                 TEXT NOT NULL DEFAULT 'INBOX',
    -- Poll cadence in seconds. NULL → worker default (60s).
    poll_interval_seconds  INTEGER,
    -- Max backoff cap in seconds. NULL → worker default (900s).
    max_backoff_seconds    INTEGER,
    -- Per-FETCH batch size cap. NULL → worker default (100).
    fetch_batch_size       INTEGER,
    enabled                BOOLEAN NOT NULL DEFAULT TRUE,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, mailbox_id)
);

-- The worker enumerates enabled mailboxes across all tenants at
-- startup + on convergence ticks. The composite index on (enabled,
-- tenant_id) keeps the supervisor's "list all enabled" scan from
-- becoming a sequential read once we have hundreds of tenants. The
-- partial index on enabled=TRUE shrinks the index further.
CREATE INDEX IF NOT EXISTS helpdesk_mailboxes_enabled_idx
    ON helpdesk_mailboxes (tenant_id)
    WHERE enabled;

-- Used by the admin UI to render the mailbox list per tenant. The
-- (tenant_id, name) composite avoids loading the whole tenant
-- partition into memory for the rendering step.
CREATE INDEX IF NOT EXISTS helpdesk_mailboxes_tenant_name_idx
    ON helpdesk_mailboxes (tenant_id, lower(name));

ALTER TABLE helpdesk_mailboxes ENABLE ROW LEVEL SECURITY;

-- Per-tenant RLS: operators within a tenant only see / mutate their
-- own mailboxes. The admin-pool bypass (next policy) handles the
-- worker's supervisor lookup which precedes any tenant context.
DROP POLICY IF EXISTS helpdesk_mailboxes_isolation ON helpdesk_mailboxes;
CREATE POLICY helpdesk_mailboxes_isolation ON helpdesk_mailboxes
    USING (tenant_id::text = current_setting('app.tenant_id', true));

-- Admin-pool bypass for the supervisor's enumerate-all-mailboxes
-- query at worker boot / convergence. Same shape as the
-- tenant_support_domains_admin_bypass policy (000031). The all-zero
-- UUID is the conventional "no tenant context set" sentinel.
DROP POLICY IF EXISTS helpdesk_mailboxes_admin_bypass ON helpdesk_mailboxes;
CREATE POLICY helpdesk_mailboxes_admin_bypass ON helpdesk_mailboxes
    FOR SELECT
    USING (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-000000000000');

COMMENT ON TABLE helpdesk_mailboxes IS
    'Per-tenant IMAP mailbox configuration consumed by the helpdesk worker. One row per (tenant, mailbox) — a tenant may attach multiple inboxes (e.g. support@ and billing@) by adding multiple rows. Credentials are referenced by SecretProvider key in imap_password_ref; the plaintext password never lands in this table.';
