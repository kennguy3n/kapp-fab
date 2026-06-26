-- 000103_validate.sql
-- Validation script for migration 000103_admin_roles_split.sql.
-- Run this AFTER applying 000103 to verify all expected changes
-- landed. Every query should return exactly one row with the listed
-- expected value. Run with:
--
--   psql "$ADMIN_DB_URL" -f migrations/000103_validate.sql
--
-- Expected: all rows show "OK". Any "MISSING" or "WRONG" result
-- indicates the migration did not apply cleanly.

\set ON_ERROR_STOP off
\echo '=== Validating migration 000103_admin_roles_split.sql ==='

-- 1. kapp_admin_readonly role exists with BYPASSRLS
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM pg_roles WHERE rolname = 'kapp_admin_readonly'
                AND rolbypassrls = true)
    THEN 'OK' ELSE 'MISSING'
END AS kapp_admin_readonly_bypassrls;

-- 2. kapp_admin_maintenance role exists with NO BYPASSRLS
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM pg_roles WHERE rolname = 'kapp_admin_maintenance'
                AND rolbypassrls = false)
    THEN 'OK' ELSE 'MISSING'
END AS kapp_admin_maintenance_no_bypassrls;

-- 3. kapp_breakglass role exists with BYPASSRLS
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM pg_roles WHERE rolname = 'kapp_breakglass'
                AND rolbypassrls = true)
    THEN 'OK' ELSE 'MISSING'
END AS kapp_breakglass_bypassrls;

-- 4. kapp_admin no longer has INSERT on data-plane tables (except tenants)
SELECT CASE
    WHEN count(*) = 0
    THEN 'OK'
    ELSE 'WRONG: ' || count(*) || ' tables still have INSERT granted to kapp_admin'
END AS kapp_admin_no_data_insert
FROM (
    SELECT table_name
    FROM information_schema.role_table_grants
    WHERE grantee = 'kapp_admin'
      AND privilege_type = 'INSERT'
      AND table_schema = 'public'
      AND table_name != 'tenants'
      AND table_name != 'admin_audit_log'
) t;

-- 5. admin_audit_log table exists
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.tables
                WHERE table_schema = 'public' AND table_name = 'admin_audit_log')
    THEN 'OK' ELSE 'MISSING'
END AS admin_audit_log_exists;

-- 6. admin_audit_log is owned by kapp_admin_maintenance
SELECT CASE
    WHEN EXISTS(
        SELECT 1 FROM pg_tables t
        JOIN pg_roles r ON r.oid = t.tableowner
        WHERE t.schemaname = 'public' AND t.tablename = 'admin_audit_log'
          AND r.rolname = 'kapp_admin_maintenance'
    )
    THEN 'OK' ELSE 'WRONG'
END AS admin_audit_log_owner;

-- 7. kapp_breakglass has INSERT on admin_audit_log (but not UPDATE/DELETE)
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.role_table_grants
                WHERE grantee = 'kapp_breakglass'
                  AND table_name = 'admin_audit_log'
                  AND privilege_type = 'INSERT')
     AND NOT EXISTS(SELECT 1 FROM information_schema.role_table_grants
                WHERE grantee = 'kapp_breakglass'
                  AND table_name = 'admin_audit_log'
                  AND privilege_type IN ('UPDATE', 'DELETE'))
    THEN 'OK' ELSE 'WRONG'
END AS breakglass_insert_only_on_audit_log;

-- 8. kapp_admin_readonly is a member of kapp_admin
SELECT CASE
    WHEN EXISTS(
        SELECT 1 FROM pg_auth_members m
        JOIN pg_roles r ON r.oid = m.roleid
        JOIN pg_roles m_ ON m_.oid = m.member
        WHERE r.rolname = 'kapp_admin' AND m_.rolname = 'kapp_admin_readonly'
    )
    THEN 'OK' ELSE 'MISSING'
END AS readonly_member_of_admin;

-- 9. admin_audit_log columns are correct
SELECT CASE
    WHEN count(*) = 9
    THEN 'OK'
    ELSE 'WRONG: ' || count(*) || ' columns (expected 9)'
END AS admin_audit_log_column_count
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'admin_audit_log';

-- 10. admin_audit_log has the expected columns by name
SELECT CASE
    WHEN count(*) = 9
    THEN 'OK'
    ELSE 'WRONG: missing columns'
END AS admin_audit_log_expected_columns
FROM (
    SELECT column_name FROM information_schema.columns
    WHERE table_schema = 'public' AND table_name = 'admin_audit_log'
      AND column_name IN ('id', 'occurred_at', 'operator_id', 'operator_kind',
                          'role', 'reason_code', 'target_tenant', 'target_table',
                          'expires_at', 'approved_by', 'metadata')
) t;

\echo '=== Validation complete ==='
