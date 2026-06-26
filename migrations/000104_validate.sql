-- 000104_validate.sql
-- Validation script for migration 000104_blind_indexes.sql.
-- Run AFTER applying 000104 to verify the blind_indexes column exists.

\set ON_ERROR_STOP off
\echo '=== Validating migration 000104_blind_indexes.sql ==='

-- 1. blind_indexes column exists on krecords
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.columns
                WHERE table_schema = 'public'
                  AND table_name = 'krecords'
                  AND column_name = 'blind_indexes'
                  AND data_type = 'jsonb')
    THEN 'OK' ELSE 'MISSING'
END AS blind_indexes_column;

-- 2. blind_indexes is nullable (indexes are optional per record)
SELECT CASE
    WHEN EXISTS(SELECT 1 FROM information_schema.columns
                WHERE table_schema = 'public'
                  AND table_name = 'krecords'
                  AND column_name = 'blind_indexes'
                  AND is_nullable = 'YES')
    THEN 'OK' ELSE 'WRONG: column should be nullable'
END AS blind_indexes_nullable;

\echo '=== Validation complete ==='
