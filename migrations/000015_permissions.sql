-- Phase H — granular permissions table.
--
-- Until now the authz evaluator read the tenant's role → permission
-- map from `roles.permissions` as a JSONB blob. That makes
-- object-scoped grants (e.g. "alice can approve *this* invoice but
-- not the rest") impossible to revoke atomically and impossible to
-- audit as discrete rows. This table normalises the map so each
-- (role, ktype, action) triple is an individually addressable row.
--
-- The evaluator joins this table by role name — user → role comes
-- from user_tenants as before, and roles.permissions remains the
-- baseline default until a tenant starts managing grants via this
-- table. RLS is the standard per-tenant isolation policy.

CREATE TABLE IF NOT EXISTS permissions (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    role_name       TEXT NOT NULL,
    ktype           TEXT NOT NULL,
    action          TEXT NOT NULL,
    conditions      JSONB NOT NULL DEFAULT '{}'::jsonb,
    granted_by      UUID,
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, role_name, ktype, action)
);

CREATE INDEX IF NOT EXISTS permissions_role_lookup_idx
    ON permissions (tenant_id, role_name)
    WHERE revoked_at IS NULL;

ALTER TABLE permissions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON permissions;
CREATE POLICY tenant_isolation ON permissions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
