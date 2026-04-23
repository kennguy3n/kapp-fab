-- Add the `schema` column used by scripts/upgrade_tier.sh.
--
-- The tier-upgrade script promotes a tenant from the shared `public`
-- schema into a dedicated `tenant_<uuid>` schema and then rewrites
-- the routing record on `tenants` so the API gateway knows where to
-- look. Without this column the final UPDATE inside upgrade_tier.sh
-- fails with `column "schema" of relation "tenants" does not exist`
-- and the upgrade rolls back, leaving the tenant wedged between the
-- two physical layouts.
--
-- Default is `public` so every existing tenant keeps its current
-- routing without a backfill step.
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS schema TEXT NOT NULL DEFAULT 'public';
