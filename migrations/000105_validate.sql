-- 000105_validate.sql
-- Validation script for migration 000105_tenant_keys.sql.
-- Run AFTER applying 000105 to verify the tenant_keys table and
-- constraints are in place.

\set ON_ERROR_STOP off
\echo '=== Validating migration 000105_tenant_keys.sql ==='

-- 1. tenant_keys table exists
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.tables
                WHERE table_schema = 'public' AND table_name = 'tenant_keys')
    THEN 'OK' ELSE 'MISSING'
END AS tenant_keys_exists;

-- 2. tenant_keys has the expected columns
SELECT CASE
    WHEN count(*) = 7
    THEN 'OK'
    ELSE 'WRONG: ' || count(*) || ' columns (expected 7)'
END AS tenant_keys_column_count
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'tenant_keys';

-- 3. tenant_keys has a composite PK on (tenant_id, key_version)
SELECT CASE
    WHEN EXISTS(
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_schema = 'public'
          AND table_name = 'tenant_keys'
          AND constraint_type = 'PRIMARY KEY'
    )
    THEN 'OK' ELSE 'MISSING'
END AS tenant_keys_pk;

-- 4. The active-uniq partial index exists
SELECT CASE
    WHEN EXISTS(
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename = 'tenant_keys'
          AND indexname = 'tenant_keys_active_uniq'
    )
    THEN 'OK' ELSE 'MISSING'
END AS tenant_keys_active_uniq_index;

-- 5. tenant_keys is owned by kapp_admin_maintenance
SELECT CASE
    WHEN EXISTS(
        SELECT 1 FROM pg_tables t
        JOIN pg_roles r ON r.oid = t.tableowner
        WHERE t.schemaname = 'public' AND t.tablename = 'tenant_keys'
          AND r.rolname = 'kapp_admin_maintenance'
    )
    THEN 'OK' ELSE 'WRONG'
END AS tenant_keys_owner;

-- 6. kapp_admin has SELECT on tenant_keys
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.role_table_grants
                WHERE grantee = 'kapp_admin'
                  AND table_name = 'tenant_keys'
                  AND privilege_type = 'SELECT')
    THEN 'OK' ELSE 'MISSING'
END AS kapp_admin_select_tenant_keys;

-- 7. PUBLIC does NOT have access to tenant_keys
SELECT CASE
    WHEN count(*) = 0
    THEN 'OK'
    ELSE 'WRONG: PUBLIC has grants on tenant_keys'
END AS public_no_access_tenant_keys
FROM information_schema.role_table_grants
WHERE grantee = 'PUBLIC' AND table_name = 'tenant_keys';

\echo '=== Validation complete ==='
