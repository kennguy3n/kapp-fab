-- Phase J — full-text search across KRecords.
--
-- Reference: frappe/frappe global search + frappe/erpnext Awesome Bar.
--
-- We add a stored generated tsvector column to krecords that extracts
-- the union of commonly-searched top-level JSONB fields (name, title,
-- description, subject, sku, code, email) so the platform can answer
-- `GET /api/v1/search?q=...` without a per-query `to_tsvector(...)`
-- evaluation. A GIN index over the column gives constant-time
-- prefix/term lookups at tenant scale.
--
-- RLS on krecords already isolates tenants via the existing
-- `tenant_isolation` policy defined in 000001_initial_schema.sql, so
-- no new policy is required — the search handler just runs inside
-- `dbutil.WithTenantTx` like every other tenant-scoped query.
--
-- The generated column uses `simple` instead of `english` so searches
-- across non-English tenant data (product SKUs, customer codes) do
-- not lose stems. Upgrading to a language-aware configuration is a
-- future refinement that only needs a re-stamp of the generated
-- expression.

ALTER TABLE krecords
    ADD COLUMN IF NOT EXISTS search_vector tsvector
    GENERATED ALWAYS AS (
        to_tsvector(
            'simple',
            coalesce(data->>'name', '')        || ' ' ||
            coalesce(data->>'title', '')       || ' ' ||
            coalesce(data->>'description', '') || ' ' ||
            coalesce(data->>'subject', '')     || ' ' ||
            coalesce(data->>'sku', '')         || ' ' ||
            coalesce(data->>'code', '')        || ' ' ||
            coalesce(data->>'email', '')       || ' ' ||
            coalesce(data->>'phone', '')       || ' ' ||
            coalesce(data->>'company', '')     || ' ' ||
            coalesce(data->>'reference', '')
        )
    ) STORED;

CREATE INDEX IF NOT EXISTS krecords_search_idx
    ON krecords USING gin (search_vector);
