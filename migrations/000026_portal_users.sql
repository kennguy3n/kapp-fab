-- Phase J — helpdesk customer portal.
--
-- Reference: frappe/helpdesk customer portal + frappe/lms public pages.
--
-- The portal gives external customers a low-privilege surface for
-- submitting and tracking helpdesk tickets. Authentication is via
-- magic-link: the customer submits their email, the platform mints
-- a one-shot token stamped with token_expires_at, emails it, and
-- swaps it for a portal-scoped JWT on the verify call.
--
-- portal_users is per-tenant. RLS is enforced — the magic-link
-- flows run under dbutil.WithTenantTx after resolving the tenant
-- from the slug supplied in the request body so a forged tenant
-- cannot harvest email existence from another tenant's list.

CREATE TABLE IF NOT EXISTS portal_users (
    tenant_id          UUID NOT NULL REFERENCES tenants(id),
    id                 UUID NOT NULL,
    email              TEXT NOT NULL,
    display_name       TEXT NOT NULL DEFAULT '',
    email_verified     BOOLEAN NOT NULL DEFAULT FALSE,
    magic_link_token   TEXT,
    token_expires_at   TIMESTAMPTZ,
    last_login_at      TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- One row per (tenant, email). Needed so the request-magic-link
-- flow can upsert by email without racing.
CREATE UNIQUE INDEX IF NOT EXISTS portal_users_tenant_email_idx
    ON portal_users (tenant_id, lower(email));

-- Token lookup index. We do SELECT ... WHERE token_hash = $1 on
-- every verify call; hashing the token before storage keeps a
-- database dump from being usable as a login vector.
CREATE INDEX IF NOT EXISTS portal_users_token_idx
    ON portal_users (magic_link_token)
    WHERE magic_link_token IS NOT NULL;

ALTER TABLE portal_users ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON portal_users;
CREATE POLICY tenant_isolation ON portal_users
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON portal_users TO kapp_app;
