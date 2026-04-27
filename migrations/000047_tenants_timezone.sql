-- Phase M Task 3: tenant timezone for shift / attendance arithmetic.
--
-- Adds a `timezone` column to `tenants` so the kchat-bridge presence
-- handler can interpret `hr.shift_type.start_time` (which is wall-clock
-- HH:MM, not UTC) in the tenant's local time before comparing against
-- the UTC check-in timestamp. Without this column, a NY tenant's 09:05
-- local check-in for a 09:00 shift would be reported as 305 tardy
-- minutes rather than 5 because the shift start would be parsed as UTC
-- 09:00.
--
-- Default `'UTC'` keeps existing tenants on the previous behaviour —
-- they continue to compare wall-clock starts as UTC until an operator
-- explicitly sets a region. IANA timezone identifiers (`America/New_York`,
-- `Australia/Sydney`, …) are the canonical form, matching what
-- `time.LoadLocation` accepts directly.
--
-- No RLS / GRANT changes — `tenants` is the operator-scoped control-
-- plane table and is intentionally outside the per-tenant RLS envelope.

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC';
