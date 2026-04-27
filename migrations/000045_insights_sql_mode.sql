-- Phase M — Insights SQL editor mode.
--
-- The Phase L visual query builder produces a structured
-- reporting.Definition (sources, filters, aggregations, joins) that
-- the runner translates into SQL with the tenant_id GUC enforced via
-- RLS. Power users on enterprise plans want to escape the visual
-- builder when their question doesn't fit the grammar — e.g. window
-- functions, CTEs, EXPLAIN, ad hoc joins to a system view. This
-- migration extends `insights_queries` with two additive columns so
-- the existing CRUD surface can persist either flavour:
--
--   - mode      enum-like text, 'visual' (default) or 'sql'
--   - raw_sql   text body of the SQL statement when mode = 'sql';
--               '' when mode = 'visual' so the column is NOT NULL +
--               default '' rather than NULLABLE (keeps scans simple
--               and prevents NULL-vs-empty drift).
--
-- The raw SQL is executed inside the same per-tenant transaction
-- pattern the visual runner uses: dbutil.WithTenantTx sets
-- `app.tenant_id`, RLS pins every read to the tenant's own rows, and
-- a SET LOCAL statement_timeout fence kills runaway scans. Parameter
-- binding flows through pgx.Query so the SQL is parameterised, not
-- string-interpolated. This is gated by the `insights_sql_editor`
-- feature flag (off for free / starter / business; on for enterprise)
-- so a stolen tenant header on a non-enterprise plan can't reach the
-- raw-SQL surface even with a valid `insights` flag.
--
-- The table already has tenant_id, the ENABLE ROW LEVEL SECURITY +
-- tenant_isolation policy from migrations/000038_insights.sql, and
-- GRANT to kapp_app — additive ALTER does not need to redeclare any
-- of those.
--
-- Reference: frappe/insights raw-SQL query mode.

ALTER TABLE insights_queries
    ADD COLUMN IF NOT EXISTS mode    TEXT NOT NULL DEFAULT 'visual',
    ADD COLUMN IF NOT EXISTS raw_sql TEXT NOT NULL DEFAULT '';

ALTER TABLE insights_queries
    DROP CONSTRAINT IF EXISTS insights_queries_mode_check;
ALTER TABLE insights_queries
    ADD CONSTRAINT insights_queries_mode_check
    CHECK (mode IN ('visual', 'sql'));

-- A query in 'sql' mode must carry a non-empty raw_sql body. A query
-- in 'visual' mode must leave raw_sql as the empty default. The
-- check is symmetric so neither column can drift into a half-state
-- where the runner silently picks the wrong execution path.
ALTER TABLE insights_queries
    DROP CONSTRAINT IF EXISTS insights_queries_mode_body_check;
ALTER TABLE insights_queries
    ADD CONSTRAINT insights_queries_mode_body_check
    CHECK (
        (mode = 'visual' AND raw_sql = '')
        OR
        (mode = 'sql' AND length(raw_sql) > 0)
    );

CREATE INDEX IF NOT EXISTS insights_queries_mode_idx
    ON insights_queries (tenant_id, mode);

-- Backfill tenant_features for existing tenants so the
-- insights_sql_editor flag is explicit. FeatureStore.IsEnabled
-- defaults missing rows to true (so a newly added flag doesn't
-- require a backfill *for new flags meant to be on*), so without
-- this INSERT every pre-existing business / starter / free tenant
-- with insights=true would silently inherit SQL editor access on
-- the first deploy of this migration. Enterprise tenants get
-- `enabled=true`; everyone else gets `false`. ON CONFLICT DO
-- NOTHING preserves any operator override (e.g. a beta tester
-- granted SQL access on a business plan).
INSERT INTO tenant_features (tenant_id, feature_key, enabled)
SELECT id, 'insights_sql_editor', (plan = 'enterprise')
FROM tenants
ON CONFLICT (tenant_id, feature_key) DO NOTHING;
