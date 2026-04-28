-- Phase RBAC — multi-role membership.
--
-- The original `user_tenants` table carries a single `role TEXT` column
-- per (tenant_id, user_id) pair (migrations/000001_initial_schema.sql).
-- That makes it impossible for a user to hold, say, both `crm.rep` and
-- `helpdesk.agent` in the same tenant; they are forced to a superset
-- role. This migration introduces a junction table so a member can
-- accumulate roles independently and the authz evaluator can union
-- their permissions across the whole set.
--
-- `user_tenants.role` is intentionally retained: it remains the
-- canonical "primary" role that legacy code paths still read, and the
-- backfill below copies each active membership's role into the new
-- table so existing tenants keep working without code changes. New
-- inserts go to BOTH tables; the wizard and role-management API write
-- the junction first and mirror the primary role into `user_tenants`
-- for backwards compatibility.

CREATE TABLE IF NOT EXISTS user_tenant_roles (
    user_id     UUID NOT NULL REFERENCES users(id),
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    role_name   TEXT NOT NULL,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    granted_by  UUID,
    PRIMARY KEY (tenant_id, user_id, role_name)
);

CREATE INDEX IF NOT EXISTS user_tenant_roles_lookup_idx
    ON user_tenant_roles (tenant_id, user_id);

ALTER TABLE user_tenant_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_tenant_roles FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON user_tenant_roles;
CREATE POLICY tenant_isolation ON user_tenant_roles
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Default privileges from migration 000001 already grant kapp_app on new
-- tables, but be explicit so a fresh database that runs this migration
-- against an existing role gets the grant deterministically.
GRANT SELECT, INSERT, UPDATE, DELETE ON user_tenant_roles TO kapp_app;

-- Backfill from the legacy single-role column. ON CONFLICT DO NOTHING
-- so the migration is idempotent if it is re-applied or if the
-- application has already started writing to the new table during a
-- staged rollout.
INSERT INTO user_tenant_roles (user_id, tenant_id, role_name)
SELECT user_id, tenant_id, role
  FROM user_tenants
 WHERE status = 'active'
   AND role IS NOT NULL
   AND role <> ''
ON CONFLICT DO NOTHING;
