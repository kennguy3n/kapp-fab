-- Phase L deferred — Insights dashboard embedding.
--
-- An embed is a long-lived bearer token a tenant operator can hand
-- out to anonymous viewers. The token grants read access to one
-- specific dashboard with optional locked-in filters (e.g. "this
-- region only"); it cannot be elevated to access other dashboards
-- or to run arbitrary queries.
--
-- The token is a 256-bit secret (base64url, 43 chars) stored as
-- the SHA-256 digest of the secret. Comparison is done in constant
-- time and the secret itself is only ever returned once (at create
-- time) — anyone who later wants the secret has to revoke and
-- reissue. Mirrors how the platform stores webhook signing secrets.
--
-- Anonymous fetches don't have a tenant_id in the request URL;
-- the lookup path uses the admin pool to read the row by digest,
-- then sets `app.tenant_id` to the row's owning tenant before
-- running the dashboard. Rate-limiting then bills the *owning
-- tenant's* bucket, not the caller IP, so a viral embed can't
-- starve other tenants.
--
-- Reference: frappe/insights dashboard share tokens.

CREATE TABLE IF NOT EXISTS insights_embeds (
    tenant_id        UUID    NOT NULL REFERENCES tenants(id),
    id               UUID    NOT NULL,
    dashboard_id     UUID    NOT NULL,
    token_digest     TEXT    NOT NULL,
    scoped_filters   JSONB   NOT NULL DEFAULT '{}'::jsonb,
    max_views        INT     NOT NULL DEFAULT 0,
    view_count       INT     NOT NULL DEFAULT 0,
    expires_at       TIMESTAMPTZ,
    revoked_at       TIMESTAMPTZ,
    created_by       UUID,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (token_digest)
);

CREATE INDEX IF NOT EXISTS insights_embeds_dashboard_idx
    ON insights_embeds (tenant_id, dashboard_id);

ALTER TABLE insights_embeds ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON insights_embeds;
CREATE POLICY tenant_isolation ON insights_embeds
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Token-digest lookups happen on the unauth public-facing endpoint
-- which goes through the admin pool to bypass RLS just for the
-- lookup row (after which the request switches to the owning
-- tenant context). Granting kapp_admin SELECT is enough; kapp_app
-- gets full access for the auth'd CRUD path.
GRANT SELECT, INSERT, UPDATE, DELETE ON insights_embeds TO kapp_app;
GRANT SELECT, UPDATE ON insights_embeds TO kapp_admin;
