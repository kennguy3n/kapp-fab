-- Phase M2 (i18n) follow-up — widen the tenants.locale CHECK to
-- accept multi-subtag IETF BCP 47 tags.
--
-- Migration 000059 added a single-subtag CHECK
--   `^[a-z]{2,3}(-[A-Za-z0-9]{2,4})?$`
-- which only allowed one optional region or script subtag. That was
-- enough for the initial pack (en, de, fr, zh-Hans, ar) but rejects
-- forms the country-derived defaults emit when a future catalogue
-- ships (zh-Hant-TW, sr-Latn-RS, de-CH-1996, es-419) and any
-- operator-supplied tag with both script AND region. The Go-side
-- regex in internal/tenant.localeRe was widened in the same PR; this
-- migration keeps the DB defence-in-depth gate in lock-step so the
-- column doesn't reject a value the wizard generates.
--
-- New regex allows the primary language (2-3 lowercase letters) plus
-- up to three trailing alphanumeric subtags of 2-8 characters each.
-- The upper-bound subtag count (3) covers every shape BCP 47
-- production rules emit in practice (language-script-region-variant)
-- and the 8-character cap follows the spec's longest variant subtag.
-- Bare ASCII case ranges, no locale-specific collation surprises.
--
-- No data migration needed: every row that passed the stricter
-- constraint also passes the looser one (the new pattern is a
-- superset). The constraint name stays the same so any monitoring /
-- alerting on `tenants_locale_format_chk` continues to work.

ALTER TABLE tenants
    DROP CONSTRAINT IF EXISTS tenants_locale_format_chk;

ALTER TABLE tenants
    ADD CONSTRAINT tenants_locale_format_chk
    CHECK (locale ~ '^[a-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$');
