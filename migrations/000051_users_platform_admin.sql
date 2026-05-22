-- Phase 1 security hotfix — install-scoped "platform admin" flag.
--
-- Control-plane routes (/api/v1/tenants/*, /api/v1/admin/*, KType
-- registration POST) are gated behind a JWT claim that is set only
-- when this column is TRUE for the authenticating user. A tenant
-- role (even "owner" or "tenant.admin") does NOT grant platform-admin
-- access; that is intentional — those roles are tenant-scoped, and
-- elevating one tenant's owner to control-plane access would let
-- them suspend or delete other tenants.
--
-- Bootstrap: a fresh install has zero platform admins. Operators
-- promote the first user via the KAPP_PLATFORM_ADMIN_USERS env var
-- (comma-separated UUIDs), which the auth bootstrap reads at SSO
-- issuance time. Once at least one user has is_platform_admin =
-- TRUE the env var should be unset and further promotions happen
-- via the /api/v1/admin/users/{id}/promote endpoint (added in a
-- subsequent PR).
--
-- The flag lives on the `users` table (which is BYPASSRLS in
-- migrations/000001) rather than on `user_tenants` because the
-- concept is install-wide, not tenant-scoped. The RLS-bypassing
-- admin pool is the only entrypoint that reads it.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS is_platform_admin BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS users_is_platform_admin_idx
    ON users (id)
 WHERE is_platform_admin = TRUE;
