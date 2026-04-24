-- Phase I — multi-currency support.
--
-- Adds a per-tenant exchange_rates registry so the posting engine can
-- convert foreign-currency amounts into the tenant's functional
-- currency at post time and so reporting can re-value open balances
-- at the period close. The schema follows the ERPNext Currency
-- Exchange shape: one row per (from, to, date) with the rate and an
-- optional provider hint, keyed per tenant.
--
-- RLS + composite PK with tenant_id match the canonical multi-tenancy
-- pattern used across migrations/000001_initial_schema.sql et seq.

CREATE TABLE IF NOT EXISTS exchange_rates (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    from_currency   TEXT NOT NULL CHECK (char_length(from_currency) = 3),
    to_currency     TEXT NOT NULL CHECK (char_length(to_currency) = 3),
    rate_date       DATE NOT NULL,
    rate            NUMERIC(20, 10) NOT NULL CHECK (rate > 0),
    provider        TEXT,
    created_by      UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, from_currency, to_currency, rate_date)
);

CREATE INDEX IF NOT EXISTS exchange_rates_lookup_idx
    ON exchange_rates (tenant_id, from_currency, to_currency, rate_date DESC);

ALTER TABLE exchange_rates ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON exchange_rates;
CREATE POLICY tenant_isolation ON exchange_rates
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON exchange_rates TO kapp_app;
