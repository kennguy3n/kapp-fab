-- iam-core (OAuth2/OIDC Authorization Server) integration — identity
-- mapping columns.
--
-- When the integration is enabled (IAM_CORE_ISSUER set), kapp-fab
-- provisions a matching tenant and (optionally) users in iam-core and
-- needs to remember the iam-core-side identifiers so it can:
--
--   * scope Management API calls to the right iam-core tenant
--     (X-Tenant-ID header) during user sync and session revocation;
--   * correlate an inbound iam-core token back to the Kapp tenant/user
--     during audit, without a Management API round-trip on the hot
--     path (the token itself carries kapp_tenant_id / kapp_user_id,
--     so these columns are for the REVERSE lookup and for idempotent
--     re-provisioning).
--
-- Both columns are nullable so the rollout is gradual: existing tenants
-- and users predate the integration and carry NULL until they are
-- (re)provisioned, and a deployment that never enables iam-core simply
-- leaves them NULL forever. Nothing in the legacy KChat path reads
-- them, so this migration is inert for such deployments.
--
--   * tenants.iam_tenant_id — the iam-core tenant id (opaque string,
--                             not a UUID in iam-core's contract).
--   * users.iam_user_id     — the iam-core user id (opaque string).
--
-- The partial UNIQUE indexes enforce a 1:1 mapping (a given iam-core
-- id maps to at most one Kapp row) while permitting many NULLs — the
-- WHERE ... IS NOT NULL clause keeps unprovisioned rows out of the
-- uniqueness constraint. The plain lookup is served by the same index.
--
-- Both `tenants` and `users` are CONTROL-PLANE tables: they have no
-- per-customer scoping column and therefore no row-level security
-- (consistent with the RLS note in migrations/000041_cell_capacity.sql
-- and 000081_cell_region_metadata.sql). The migration-rls-check
-- workflow only requires ENABLE ROW LEVEL SECURITY for tables whose
-- body declares a customer-scoping column, which these ALTERs do not.
--
-- All statements are idempotent (IF NOT EXISTS) so re-applying against
-- a partially-migrated database is safe.

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS iam_tenant_id TEXT;
ALTER TABLE users   ADD COLUMN IF NOT EXISTS iam_user_id   TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS tenants_iam_tenant_id_key
    ON tenants (iam_tenant_id)
    WHERE iam_tenant_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS users_iam_user_id_key
    ON users (iam_user_id)
    WHERE iam_user_id IS NOT NULL;
