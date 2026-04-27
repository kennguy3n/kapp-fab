-- Phase G — scoped tier-upgrade role.
--
-- The /api/v1/admin/tenants/{id}/upgrade-tier endpoint and the
-- scripts/upgrade_tier.sh runbook both need to:
--
--   1. CREATE SCHEMA tenant_<uuid>
--   2. CREATE TABLE … (LIKE public.<table> INCLUDING ALL) on every
--      tenant-scoped table
--   3. INSERT INTO tenant_<uuid>.<table> SELECT … FROM public.<table>
--      WHERE tenant_id = $1
--   4. UPDATE public.tenants SET schema = $1
--
-- Up to now the admin path executed every statement as `kapp_admin`,
-- the BYPASSRLS service role used for cross-tenant reads. Granting a
-- BYPASSRLS role to every operator who runs the upgrade is wider
-- than needed: kapp_admin can also DROP arbitrary tables, see all
-- tenants' rows, etc. SECURITY_REVIEW.md §8 item 5 tracks this gap.
--
-- This migration introduces `kapp_tier_admin`, a non-superuser role
-- that owns CREATE on the public schema and only the privileges
-- required for the upgrade. The admin pool wraps the actual upgrade
-- in a SECURITY DEFINER function `promote_tenant_to_schema(...)`
-- that runs as kapp_tier_admin regardless of which role is
-- connected, so an operator only needs EXECUTE on the function — no
-- direct table ownership and no BYPASSRLS.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kapp_tier_admin') THEN
        -- NOSUPERUSER NOBYPASSRLS NOCREATEDB are the defaults but
        -- documented explicitly so a future ALTER ROLE that flips
        -- one of them surfaces in code review.
        CREATE ROLE kapp_tier_admin LOGIN PASSWORD 'kapp_tier_admin_dev'
            NOSUPERUSER NOBYPASSRLS NOCREATEDB;
    END IF;
END $$;

-- The upgrade walks information_schema.columns to enumerate
-- non-generated columns; that requires CONNECT + TEMP on the
-- current database plus USAGE/CREATE on public. We resolve the
-- current database name dynamically so the migration works across
-- environments (kapp, kapp_test, etc.).
DO $$
DECLARE
    db TEXT := current_database();
BEGIN
    EXECUTE format('GRANT CONNECT, TEMPORARY, CREATE ON DATABASE %I TO kapp_tier_admin', db);
END $$;
GRANT USAGE, CREATE ON SCHEMA public TO kapp_tier_admin;

-- The function copies every tenant-scoped row into the dedicated
-- schema, so it needs SELECT on every public table. Read-only on the
-- source side; the writes happen against the freshly-created
-- dedicated schema, which the role owns by virtue of having created
-- it.
GRANT SELECT ON ALL TABLES IN SCHEMA public TO kapp_tier_admin;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO kapp_tier_admin;

-- Step 4 of the upgrade flips public.tenants.schema. UPDATE without
-- DELETE ensures the role cannot remove tenant rows even if it had
-- access to the underlying connection.
GRANT UPDATE (schema) ON public.tenants TO kapp_tier_admin;

-- Now the SECURITY DEFINER wrapper. It is owned by kapp_tier_admin
-- and runs with the role's privileges, so an EXECUTE caller (kapp_admin
-- or a future scoped operator role) does NOT need direct DDL rights.
--
-- The function takes the tenant id, the target schema name, and a
-- text[] of tenant-scoped tables to copy. The Go side
-- (internal/tenant/tier.go) stays the source of truth for that list
-- so we don't double-maintain it in SQL.

DROP FUNCTION IF EXISTS public.promote_tenant_to_schema(UUID, TEXT, TEXT[]);
CREATE OR REPLACE FUNCTION public.promote_tenant_to_schema(
    p_tenant_id   UUID,
    p_schema_name TEXT,
    p_tables      TEXT[]
)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    t            TEXT;
    col_list     TEXT;
    create_stmt  TEXT;
    copy_stmt    TEXT;
BEGIN
    -- Identifier safety: refuse anything outside [a-zA-Z0-9_].
    IF p_schema_name !~ '^[a-zA-Z_][a-zA-Z0-9_]{0,62}$' THEN
        RAISE EXCEPTION 'promote_tenant_to_schema: refusing unsafe schema name %', p_schema_name;
    END IF;

    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I', p_schema_name);

    -- Set the RLS context so the SELECT below sees the target
    -- tenant's rows. kapp_tier_admin is deliberately NOBYPASSRLS,
    -- so without this GUC the SELECT returns zero rows on every
    -- tenant-scoped table and the dedicated schema would land
    -- empty. SET LOCAL is rolled back at function exit so we don't
    -- pollute the caller's session state.
    EXECUTE format('SET LOCAL app.tenant_id = %L', p_tenant_id::text);

    FOREACH t IN ARRAY p_tables LOOP
        IF t !~ '^[a-zA-Z_][a-zA-Z0-9_]{0,62}$' THEN
            RAISE EXCEPTION 'promote_tenant_to_schema: refusing unsafe table name %', t;
        END IF;

        create_stmt := format(
            'CREATE TABLE IF NOT EXISTS %I.%I (LIKE public.%I INCLUDING ALL)',
            p_schema_name, t, t
        );
        EXECUTE create_stmt;

        SELECT string_agg(format('%I', column_name), ', ')
          INTO col_list
          FROM information_schema.columns
         WHERE table_schema = 'public'
           AND table_name   = t
           AND COALESCE(is_generated, 'NEVER') = 'NEVER';

        IF col_list IS NULL THEN
            CONTINUE;
        END IF;

        copy_stmt := format(
            'INSERT INTO %I.%I (%s) SELECT %s FROM public.%I WHERE tenant_id = $1 ON CONFLICT DO NOTHING',
            p_schema_name, t, col_list, col_list, t
        );
        EXECUTE copy_stmt USING p_tenant_id;
    END LOOP;

    UPDATE public.tenants SET schema = p_schema_name WHERE id = p_tenant_id;
END;
$$;

ALTER FUNCTION public.promote_tenant_to_schema(UUID, TEXT, TEXT[]) OWNER TO kapp_tier_admin;

-- Lock down EXECUTE so only roles with explicit GRANT can call the
-- function. PUBLIC is revoked; kapp_admin (the pool the API uses)
-- gets EXECUTE so the existing code path keeps working with no
-- behaviour change.
REVOKE ALL ON FUNCTION public.promote_tenant_to_schema(UUID, TEXT, TEXT[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.promote_tenant_to_schema(UUID, TEXT, TEXT[]) TO kapp_admin;

-- kapp_admin inherits kapp_tier_admin's privileges so it can manage
-- (drop, alter, regrant) the schemas the function creates. This is
-- the path the API gateway uses post-upgrade, the test cleanup uses
-- when tearing down a fixture, and the operator uses if a botched
-- upgrade needs reversing. PostgreSQL membership is INHERIT by
-- default for a CREATE ROLE that does not say NOINHERIT.
GRANT kapp_tier_admin TO kapp_admin;
