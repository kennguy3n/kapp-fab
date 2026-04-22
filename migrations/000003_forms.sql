-- Migration 000003: public-facing Forms KApp.
--
-- A Form is a tenant-scoped record-creation surface that does not require
-- the submitter to hold a tenant session. Anonymous or loosely-auth'd
-- users POST to /api/v1/forms/{id}/submit; the API derives the tenant
-- from the form config and creates the target KRecord under that tenant's
-- context. Public GET of the form + its KType schema is allowed so the
-- renderer can be fully client-side.
--
-- Forms live in a tenant-scoped table with RLS enforced the same way as
-- other tenant-scoped objects. Form IDs are UUIDs so a leaked link cannot
-- be guessed from any sequential counter.

CREATE TABLE IF NOT EXISTS forms (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    ktype        TEXT NOT NULL,
    config       JSONB NOT NULL DEFAULT '{}'::jsonb,
    status       TEXT NOT NULL DEFAULT 'active',
    created_by   UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_forms_tenant ON forms(tenant_id);
CREATE INDEX IF NOT EXISTS idx_forms_ktype ON forms(tenant_id, ktype);

ALTER TABLE forms ENABLE ROW LEVEL SECURITY;

-- The tenant_current() / app.tenant_id convention is used elsewhere in
-- the schema (see migration 000001). Forms follow the same rule: a row
-- is visible only when the session has SET LOCAL app.tenant_id to a
-- matching UUID, except for the public-submit path which uses the
-- BYPASSRLS admin role by design.
DROP POLICY IF EXISTS tenant_isolation ON forms;
CREATE POLICY tenant_isolation ON forms
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON forms TO kapp_app;

-- Admin reads of forms happen during public submission — the admin role
-- reads the form row to determine the target tenant, then re-enters the
-- app role with the correct tenant context. This keeps the public
-- submission path single-query on the admin pool and avoids a
-- tenant-guessing round-trip.
GRANT SELECT ON forms TO kapp_admin;
