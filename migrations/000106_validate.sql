-- 000106_validate.sql
-- Validation script for migration 000106_export_user_roles.sql.
-- Run AFTER applying 000106 to verify the user_roles column exists.

\set ON_ERROR_STOP off
\echo '=== Validating migration 000106_export_user_roles.sql ==='

-- 1. user_roles column exists on export_jobs
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.columns
                WHERE table_schema = 'public'
                  AND table_name = 'export_jobs'
                  AND column_name = 'user_roles'
                  AND data_type = 'jsonb')
    THEN 'OK' ELSE 'MISSING'
END AS user_roles_column;

-- 2. user_roles is nullable (system-initiated exports have no roles)
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.columns
                WHERE table_schema = 'public'
                  AND table_name = 'export_jobs'
                  AND column_name = 'user_roles'
                  AND is_nullable = 'YES')
    THEN 'OK' ELSE 'WRONG: column should be nullable'
END AS user_roles_nullable;

\echo '=== Validation complete ==='
