-- Phase J/K — Multi-currency journal posting + tenant base currency.
--
-- Two columns join the schema:
--
--   * `tenants.base_currency` — the tenant's functional currency.
--     Defaults to USD so existing rows behave identically; the
--     setup wizard now persists `cfg.CurrencyCode` here when
--     non-empty so subsequent journal posts can detect foreign
--     currency lines and convert them.
--
--   * `journal_lines.base_amount` (signed decimal) — the line's
--     net amount converted to the tenant's base currency at the
--     posting-date rate. PostJournalEntry computes this via
--     `ExchangeRateStore.Convert`. Same-currency lines store the
--     net debit/credit; foreign lines preserve `currency` +
--     `debit/credit` AND attach `base_amount` so reports can sum
--     in either currency without re-doing the conversion.
--
-- Rationale: ERPNext stores both `debit_in_account_currency` and
-- `debit` (account / functional currency) on the Journal Entry
-- account row. We mirror that contract here so finance reports can
-- aggregate in base currency without recomputing rates and the
-- audit trail keeps the original line as posted.

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS base_currency CHAR(3) NOT NULL DEFAULT 'USD';

COMMENT ON COLUMN tenants.base_currency IS
    'ISO-4217 code of the tenant''s functional / base currency. PostJournalEntry converts foreign-currency lines to this on insert.';

ALTER TABLE journal_lines
    ADD COLUMN IF NOT EXISTS base_amount NUMERIC(20, 4) DEFAULT NULL;

COMMENT ON COLUMN journal_lines.base_amount IS
    'Net signed amount in the tenant''s base currency (debit positive, credit negative). NULL for legacy rows posted before 000029.';

-- No new RLS policy needed — both columns hang off existing
-- tenant-scoped tables that already enforce RLS via tenant_id.
