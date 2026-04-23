-- Phase H — auth sessions table.
--
-- Each row is one active authentication context: a user inside a
-- tenant with a refresh-token family. Session revocation is how the
-- platform forces logout: deleting (or setting revoked_at on) the row
-- stops the next access-token verify from succeeding even if the JWT
-- itself has not expired yet.
--
-- The table is tenant-scoped and carries RLS — a tenant that is
-- suspended by the control plane can have every row revoked in a
-- single UPDATE inside that tenant's context.

CREATE TABLE IF NOT EXISTS sessions (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    user_id         UUID NOT NULL REFERENCES users(id),
    refresh_jti     TEXT NOT NULL,
    issued_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent      TEXT NOT NULL DEFAULT '',
    ip_address      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS sessions_tenant_user_active_idx
    ON sessions (tenant_id, user_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS sessions_expires_idx
    ON sessions (expires_at)
    WHERE revoked_at IS NULL;

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON sessions;
CREATE POLICY tenant_isolation ON sessions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
