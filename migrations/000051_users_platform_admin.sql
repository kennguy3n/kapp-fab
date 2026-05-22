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
--   3. Second SSO (refresh OR fresh exchange): the auth bootstrap
--      reads the env var, sees the user matches, persists
--      is_platform_admin = TRUE, and stamps the claim on the JWT.
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

CREATE INDEX IF NOT EXISTS users_is_platform_admin_idx
    ON users (id)
 WHERE is_platform_admin = TRUE;
