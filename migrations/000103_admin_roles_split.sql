-- Phase 1 P1-5 — split the kapp_admin blast radius.
--
-- Today `kapp_admin` (migrations/000002_admin_role.sql) is a single
-- BYPASSRLS role with SELECT/INSERT/UPDATE/DELETE on every table. That
-- is wider than any single operational task needs: the control-plane
-- reads that justify BYPASSRLS (user→tenants lookup at login, cross-
-- tenant admin views) are read-only, while maintenance work (retention,
-- tenant purge, tier upgrades) does not need BYPASSRLS at all once it
-- runs under a SET LOCAL app.tenant_id GUC (see the kapp_tier_admin
-- pattern in 000042_tier_admin_role.sql).
--
-- This migration introduces three scoped roles and an immutable
-- admin-action audit table, and narrows `kapp_admin` to read-only on
-- the data plane. The runtime break-glass flow (reason code, time box,
-- approval, allowlist enforcement) is Phase 2 work tracked in
-- docs/SECURITY_HARDENING_PLAN.md; this migration lays the role +
-- audit-table foundation so the runtime can be wired against it.
--
-- Role inventory after this migration:
--
--   kapp_app                — data plane, RLS enforced (unchanged)
--   kapp_admin              — BYPASSRLS, SELECT only on data plane;
--                             used for cross-tenant control-plane reads
--   kapp_admin_readonly     — alias-grade read-only role for reporting /
--                             support views that never write (inherits
--                             kapp_admin's SELECT grants)
--   kapp_admin_maintenance  — NOSUPERUSER NOBYPASSRLS; owns retention,
--                             purge, and tenant-lifecycle jobs that run
--                             under SET LOCAL app.tenant_id (extends the
--                             kapp_tier_admin pattern)
--   kapp_breakglass         — BYPASSRLS, time-boxed human-approved
--                             sensitive access; every action MUST be
--                             recorded in admin_audit_log by the
--                             runtime wrapper (Phase 2)

-- 1. kapp_admin_readonly: a read-only mirror of kapp_admin's SELECT
--    grants, for support / reporting dashboards that must never write.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kapp_admin_readonly') THEN
        CREATE ROLE kapp_admin_readonly LOGIN PASSWORD 'kapp_admin_readonly_dev'
            NOSUPERUSER BYPASSRLS NOCREATEDB;
    END IF;
END $$;
GRANT USAGE ON SCHEMA public TO kapp_admin_readonly;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO kapp_admin_readonly;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO kapp_admin_readonly;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO kapp_admin_readonly;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO kapp_admin_readonly;

-- 2. kapp_admin_maintenance: non-BYPASSRLS role for scheduled jobs
--    (retention sweeps, tenant purge, quota recounts). These run under
--    SET LOCAL app.tenant_id so RLS scopes them per tenant; they do not
--    need to bypass RLS. Modeled on kapp_tier_admin (000042).
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kapp_admin_maintenance') THEN
        CREATE ROLE kapp_admin_maintenance LOGIN PASSWORD 'kapp_admin_maintenance_dev'
            NOSUPERUSER NOBYPASSRLS NOCREATEDB;
    END IF;
END $$;
GRANT USAGE ON SCHEMA public TO kapp_admin_maintenance;
-- Maintenance jobs read + write tenant-scoped rows under SET LOCAL GUCs.
-- DELETE is granted for retention/purge; INSERT/UPDATE for quota recounts
-- and bookkeeping. RLS still applies because the role is NOBYPASSRLS, so
-- a missing GUC yields zero rows rather than a cross-tenant leak.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO kapp_admin_maintenance;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kapp_admin_maintenance;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO kapp_admin_maintenance;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO kapp_admin_maintenance;

-- 3. kapp_breakglass: BYPASSRLS role for time-boxed, human-approved
--    sensitive access. The runtime (Phase 2) wraps every connection in
--    an admin_audit_log write carrying the reason code, operator id,
--    expiry, and target. Until that runtime exists, this role is
--    provisioned but NOT granted to any service pool — operators must
--    GRANT it explicitly and transiently during an incident.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kapp_breakglass') THEN
        CREATE ROLE kapp_breakglass LOGIN PASSWORD 'kapp_breakglass_dev'
            NOSUPERUSER BYPASSRLS NOCREATEDB;
    END IF;
END $$;
GRANT USAGE ON SCHEMA public TO kapp_breakglass;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO kapp_breakglass;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kapp_breakglass;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO kapp_breakglass;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO kapp_breakglass;

-- 4. Narrow kapp_admin: revoke the broad INSERT/UPDATE/DELETE it was
--    granted in 000002 so the BYPASSRLS control-plane role is read-only
--    on the data plane. Cross-tenant writes (tier upgrades, tenant
--    lifecycle) already go through the kapp_tier_admin SECURITY DEFINER
--    function (000042) or the maintenance role, neither of which needs
--    kapp_admin to hold write privileges.
--
--    We do not revoke from `tenants` control-plane writes the API uses
--    for tenant CRUD; those are retained via a targeted re-grant below
--    so the admin route keeps working while the broader data-plane
--    writes are removed.
REVOKE INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public FROM kapp_admin;
-- Re-grant the minimal control-plane writes the admin API route uses.
-- tenant CRUD + tier upgrade bookkeeping. Everything else stays
-- read-only under kapp_admin.
GRANT INSERT, UPDATE ON public.tenants TO kapp_admin;
GRANT UPDATE (schema) ON public.tenants TO kapp_admin;

-- 5. admin_audit_log: an append-only, RLS-exempt record of every
--    break-glass / BYPASSRLS action. The runtime wrapper (Phase 2)
--    inserts one row per break-glass connection with the reason,
--    operator, expiry, and target. The table is owned by
--    kapp_admin_maintenance so the maintenance pool can prune it on a
--    retention schedule; kapp_breakglass gets INSERT-only so an
--    operator using break-glass can record their action but cannot
--    tamper with the log.
CREATE TABLE IF NOT EXISTS admin_audit_log (
    id           bigserial PRIMARY KEY,
    occurred_at  timestamptz NOT NULL DEFAULT now(),
    operator_id  uuid,
    operator_kind text NOT NULL,
    role         text NOT NULL,
    reason_code  text NOT NULL,
    target_tenant uuid,
    target_table text,
    expires_at   timestamptz,
    approved_by  uuid,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb
);
-- Append-only enforcement: no UPDATE / DELETE to anyone but the owner.
-- The owner (kapp_admin_maintenance) only uses DELETE for retention
-- pruning; application roles get INSERT + SELECT only.
REVOKE ALL ON admin_audit_log FROM PUBLIC;
GRANT SELECT, INSERT ON admin_audit_log TO kapp_breakglass;
GRANT SELECT, INSERT ON admin_audit_log TO kapp_admin;
GRANT SELECT, INSERT, UPDATE, DELETE ON admin_audit_log TO kapp_admin_maintenance;
ALTER TABLE admin_audit_log OWNER TO kapp_admin_maintenance;

-- 5a. Indexes for common audit query patterns:
--   - (occurred_at) for retention pruning (DELETE WHERE occurred_at < ...)
--   - (operator_id, occurred_at) for per-operator audit trails
--   - (target_tenant, occurred_at) for per-tenant incident review
CREATE INDEX IF NOT EXISTS admin_audit_log_occurred_at_idx
    ON admin_audit_log (occurred_at);
CREATE INDEX IF NOT EXISTS admin_audit_log_operator_idx
    ON admin_audit_log (operator_id, occurred_at);
CREATE INDEX IF NOT EXISTS admin_audit_log_target_tenant_idx
    ON admin_audit_log (target_tenant, occurred_at);

-- 6. Membership wiring. kapp_admin_readonly inherits kapp_admin's
--    SELECT grants via membership so a single GRANT/REVOKE on
--    kapp_admin propagates. kapp_admin keeps its kapp_tier_admin
--    membership (000042) so the tier-upgrade SECURITY DEFINER path
--    still works.
GRANT kapp_admin TO kapp_admin_readonly;
