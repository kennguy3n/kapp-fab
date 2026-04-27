-- Phase G/L — Insights performance tuning indexes.
--
-- Audit pass over internal/insights/{store,cache,runner}.go found three
-- access paths that scan more rows than the existing 000038 indexes
-- support. Each new index leads with tenant_id so RLS-bound queries
-- (every read in the package wraps SELECT in dbutil.WithTenantTx and
-- the planner sees `tenant_id = $1` in the predicate) can use the
-- index for both isolation and ordering. Reference:
-- docs/PERFORMANCE_TUNING.md §Insights.
--
-- Index choices intentionally avoid duplicating the indexes that
-- 000038_insights.sql already created (insights_queries UNIQUE
-- (tenant_id, name), insights_dashboards UNIQUE (tenant_id, name),
-- insights_query_cache_expiry_idx, insights_query_cache_query_idx,
-- insights_shares_resource_idx).

-- 1. Cache sweep ordering. CacheStore.SweepExpired walks rows where
--    expires_at <= now() and the existing expiry_idx already supports
--    that. The new (tenant_id, query_id, created_at DESC) layout
--    accelerates the dashboard digest path (services/kchat-bridge
--    /dashboard-digest) which fetches the *latest* cache entry per
--    saved query without paying the index → table re-sort cost.
CREATE INDEX IF NOT EXISTS insights_query_cache_query_recent_idx
    ON insights_query_cache (tenant_id, query_id, created_at DESC)
    WHERE query_id IS NOT NULL;

-- 2. Widget → query reverse lookup. When a saved query is deleted the
--    QueryStore must list every widget that referenced it so the UI
--    can render the "missing query" placeholder without a sequential
--    scan of insights_dashboard_widgets per delete.
CREATE INDEX IF NOT EXISTS insights_dashboard_widgets_query_idx
    ON insights_dashboard_widgets (tenant_id, query_id)
    WHERE query_id IS NOT NULL;

-- 3. Case-insensitive name lookup. /insight + /dashboard-digest slash
--    commands accept a query / dashboard *name* and resolve it via
--    LOWER(name) so KChat users don't have to remember the exact case
--    a teammate used at create time. Without this expression index
--    the resolver falls back to a sequential scan inside the tenant
--    partition.
CREATE INDEX IF NOT EXISTS insights_queries_name_lower_idx
    ON insights_queries (tenant_id, LOWER(name));

CREATE INDEX IF NOT EXISTS insights_dashboards_name_lower_idx
    ON insights_dashboards (tenant_id, LOWER(name));

-- 4. Share lookup by grantee. The insights authz check ("does user
--    X have view on dashboard Y?") joins from a known grantee back
--    onto insights_shares. The existing insights_shares_resource_idx
--    supports the resource→grantee direction; the new index supports
--    the grantee→resource direction so the per-request authorise
--    fan-out completes in one index seek.
CREATE INDEX IF NOT EXISTS insights_shares_grantee_idx
    ON insights_shares (tenant_id, grantee_type, grantee, resource_type);
