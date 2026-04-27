-- Phase M Task 2: country tax packs.
--
-- Adds a `country` column to `tenants` so the payroll engine can
-- resolve a per-country tax pack at slip generation time. The wizard
-- already collects `cfg.Country` (used by DerivePlacementPolicy) but
-- never persisted it to a column the engine could read; this
-- migration closes that gap.
--
-- The column is `TEXT NOT NULL DEFAULT ''` rather than `NULL`-able
-- so the engine can fall back to "no statutory deductions" without
-- needing to handle NULL on every read. ISO 3166-1 alpha-2 codes
-- (`US`, `AU`, `GB`, …) are the canonical form, matching the
-- wizard payload.
--
-- No RLS / GRANT changes — `tenants` is the operator-scoped control-
-- plane table and is intentionally outside the per-tenant RLS
-- envelope.

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS country TEXT NOT NULL DEFAULT '';

-- Index for the (cell, country) bulk-lookup the dashboard / billing
-- views run when grouping tenants by jurisdiction. Partial so it
-- doesn't bloat with the back-fill default.
CREATE INDEX IF NOT EXISTS tenants_country_idx
    ON tenants (country)
    WHERE country <> '';
