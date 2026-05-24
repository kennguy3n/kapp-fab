-- Phase M2 (i18n) — per-tenant locale.
--
-- Adds a `locale` column to `tenants` so the API can resolve the
-- right UI bundle and the right Intl formatters (currency, dates,
-- numbers) for every authenticated request, and so the
-- `Accept-Language` middleware has a concrete fallback when the
-- caller sends nothing.
--
-- The column is `TEXT NOT NULL DEFAULT 'en'` rather than NULL-able
-- so the i18n bundle resolver never has to handle NULL — every
-- legacy tenant comes through as English without any back-fill
-- step. Tenant operators flip this from the setup wizard or the
-- admin surface once they pick a country.
--
-- The format is IETF BCP 47 (lowercase ISO 639-1/639-3 base with
-- an optional region/script subtag, e.g. `en`, `de`, `fr`,
-- `zh-Hans`, `ar-SA`). PGStore.SetLocale validates against an
-- in-process whitelist of supported bundles so a malformed value
-- never round-trips; the DB-level CHECK is a defence-in-depth
-- format gate that rejects obviously broken values regardless of
-- which Go service writes the row.
--
-- No RLS / GRANT changes — `tenants` is the operator-scoped
-- control-plane table and is intentionally outside the per-tenant
-- RLS envelope. Same shape as migration 000046 (tenants.country)
-- and 000047 (tenants.timezone).

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS locale TEXT NOT NULL DEFAULT 'en';

COMMENT ON COLUMN tenants.locale IS
    'IETF BCP 47 language tag for the tenant UI bundle and Intl '
    'formatters. Resolved by the API''s Accept-Language middleware '
    'and persisted by SetLocale. Defaults to ''en''.';

-- Defence-in-depth format gate. The full bundle whitelist is
-- enforced in Go (internal/i18n + tenant.PGStore.SetLocale) so the
-- DB constraint only rejects clearly malformed values that would
-- never resolve to a real bundle. The base is 2-3 lowercase
-- letters (ISO 639-1 or 639-3); the optional subtag is 2-4
-- alphanumeric (region or script). Bare ASCII case ranges, no
-- locale-specific collation surprises in CHECK.
ALTER TABLE tenants
    ADD CONSTRAINT tenants_locale_format_chk
    CHECK (locale ~ '^[a-z]{2,3}(-[A-Za-z0-9]{2,4})?$');
