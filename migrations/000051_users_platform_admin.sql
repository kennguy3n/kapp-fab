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
-- Bootstrap (two-step, by design): a fresh install has zero
-- platform admins AND zero rows in `users` — the candidate user
-- has not authenticated yet, so the DB-generated UUID does not
-- exist for the operator to enumerate in advance. The bootstrap
-- therefore takes two SSO logins:
--   1. First SSO: the user authenticates, the upsert in sso.go
--      creates the `users` row, and the operator can now look up
--      the DB-generated UUID (e.g., `SELECT id FROM users WHERE
--      email = 'admin@example.com'`).
--   2. Operator sets KAPP_PLATFORM_ADMIN_USERS to that UUID
--      (comma-separated for multiple admins) and restarts the API.
--   3. Second SSO (fresh exchange only, NOT refresh): the auth
--      bootstrap reads the env var, sees the user matches, persists
--      is_platform_admin = TRUE, and stamps the claim on the JWT.
--      A session refresh would re-query `users.is_platform_admin`
--      (which is still FALSE at this point) and mint a non-admin
--      token — the bootstrap promotion runs inside `upsertUser`,
--      which is only reached from `Exchange`, not from `Refresh`.
--      In practice this means the candidate user MUST log out and
--      log back in (or have their existing session revoked) so
--      their next request goes through Exchange.
-- Once at least one row has is_platform_admin = TRUE the env var
-- should be unset and further promotions happen via the
-- /api/v1/admin/users/{id}/promote endpoint (added in a subsequent
-- PR). Operators with shell access to the admin DB can also flip
-- the flag directly during the initial install to skip step 3 —
-- the env var is the documented happy path because it does not
-- require psql access.
--
-- The flag lives on the `users` table (which is BYPASSRLS in
-- migrations/000001) rather than on `user_tenants` because the
-- concept is install-wide, not tenant-scoped. The RLS-bypassing
-- admin pool is the only entrypoint that reads it.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS is_platform_admin BOOLEAN NOT NULL DEFAULT FALSE;

-- Partial index intentionally forward-looking: the per-user lookup
-- in internal/auth/sso.go::lookupPlatformAdmin reads the column
-- through the users PK and does NOT need this index. It exists for
-- the admin-management endpoints landing alongside
-- /api/v1/admin/users/{id}/promote in the next PR — specifically the
-- "list all platform admins" query, which scans the table for the
-- ≤0.1% of rows where the flag is TRUE. Building it now avoids an
-- ALTER on a populated table later; the maintenance cost on a
-- column that flips ≈once per admin promotion is negligible.
CREATE INDEX IF NOT EXISTS users_is_platform_admin_idx
    ON users (id)
 WHERE is_platform_admin = TRUE;
