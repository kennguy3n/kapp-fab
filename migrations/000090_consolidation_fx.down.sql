-- Down migration for 000090_consolidation_fx.sql.
--
-- Drops the FX revaluation audit table and the consolidation CTA
-- account column. Reversible and idempotent.

DROP TABLE IF EXISTS fx_revaluation_runs;

ALTER TABLE consolidation_groups
    DROP COLUMN IF EXISTS cta_account_code;
