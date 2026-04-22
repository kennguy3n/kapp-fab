-- Phase A step 4 — provision an administrative role for control-plane reads.
--
-- `kapp_app` is the normal application role and is subject to RLS. A handful
-- of legitimate reads span tenants and therefore cannot supply a tenant GUC,
-- notably `tenant.UserStore.GetUserTenants` which lists every membership for
-- a given user (login / tenant-picker UI, onboarding). Under `kapp_app` those
-- queries return zero rows because the RLS policy compares `tenant_id` with
-- `NULL` when no GUC is set.
--
-- Rather than widen every cross-tenant policy, this migration adds a
-- dedicated `kapp_admin` role with BYPASSRLS. The application opens a second,
-- small pool as `kapp_admin` only for control-plane reads; regular tenant
-- traffic keeps using `kapp_app` so data-plane isolation remains mandatory.
--
-- Credentials are dev-only (docker-compose). Production rotates them via the
-- secret manager.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kapp_admin') THEN
        CREATE ROLE kapp_admin LOGIN PASSWORD 'kapp_admin_dev' BYPASSRLS;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO kapp_admin;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO kapp_admin;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kapp_admin;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO kapp_admin;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO kapp_admin;
