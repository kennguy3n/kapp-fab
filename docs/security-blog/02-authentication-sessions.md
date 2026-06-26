# Authentication & Sessions: JWTs, Keyring Rotation, and Revocation

**Tenant:** Acme Corp
**Persona:** Alex, the tenant admin, and the platform operator who must be able to force Alex's session off the system instantly.

Tenant isolation (post 1) only means something if the database can trust
*which tenant* a request claims to be. That trust is established by
authentication: every tenant-scoped request carries a signed JWT that the API
verifies, and the JWT's `tenant_id` claim becomes the `app.tenant_id` GUC.

## JWT signing: HS256 by default, RS256 optional

Kapp signs access tokens with HMAC-SHA256 (HS256) by default. The signing
secret lives in `KAPP_JWT_SECRET`, which must be at least 32 bytes — the
`auth.NewSigner` constructor rejects anything shorter. In production the secret
is sourced through a pluggable secret provider (`KAPP_SECRET_PROVIDER`), with
backends for env, on-disk files (Kubernetes Secret mounts), AWS Secrets
Manager, and HashiCorp Vault. RS256 is selectable for deployments that prefer
asymmetric keys.

A defence-in-depth guard refuses to construct a signer keyed on the literal
dev placeholder from `.env.example` unless `KAPP_ALLOW_DEV_JWT_SECRET=1` is
also set. A deployment that copies `.env.example` without rotating the secret
**fails to start** rather than silently signing tokens with a well-known key.

## The JWT keyring: rotate without restarting

JWT verification is the most security-sensitive hot path in the API, so key
rotation cannot require downtime. The signer reads its keys through a *keyring*
abstraction: one `KAPP_JWT_PRIMARY_REF` (the active signing key) plus an
ordered list of `KAPP_JWT_VERIFY_REFS` (previously-active keys kept around
during a rotation window). A background refresher polls the secret provider
every `KAPP_JWT_KEYRING_REFRESH_INTERVAL` and atomically swaps the primary key
when the upstream version changes — no restart, no dropped verifications.

The rotation procedure (from the hardening guide): set `KAPP_JWT_PRIMARY_REF`
to the new key, keep the old key in `KAPP_JWT_VERIFY_REFS` for one access-token
TTL window, then remove it. In-flight tokens keep verifying throughout.

## Token lifetimes and the header-injection hole that is closed

Access tokens are short-lived (`KAPP_JWT_ACCESS_TTL`, default 15 minutes);
refresh tokens live up to `KAPP_JWT_REFRESH_TTL` (default 24 hours, 7 days in
production). Every tenant-scoped route group sits behind a `tenantChain`
middleware that requires a configured signer. Leaving `KAPP_JWT_SECRET` empty
no longer falls back to the legacy `X-Tenant-ID` header path — instead every
tenant-scoped request returns `503` with a message explaining the misconfig.
This closes the header-impersonation vector: a request cannot simply *assert*
a tenant ID; it must present a token the server signed.

## Sessions: revocation that beats token expiry

A signed JWT is stateless — once issued it is valid until it expires, even if
the user is fired or the tenant is suspended. That is too slow for an ERP.
Kapp pairs every refresh token with a row in the `sessions` table:

```sql
CREATE TABLE sessions (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    user_id         UUID NOT NULL REFERENCES users(id),
    refresh_jti     TEXT NOT NULL,
    issued_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent      TEXT NOT NULL DEFAULT '',
    ip_address      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);
```

Session revocation is how the platform forces logout: setting `revoked_at`
stops the next access-token refresh from succeeding even though the JWT itself
has not expired. The table is tenant-scoped and RLS-protected, so suspending a
tenant can revoke every session in a single `UPDATE` inside that tenant's
context — `POST /api/v1/admin/tenants/{id}/suspend` does exactly this.

The demo seeds two Acme sessions — one active, one revoked — to show the
state the operator sees:

```
kapp_app=> SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000001';
kapp_app=> SELECT user_id, refresh_jti, (revoked_at IS NULL) AS active,
                  ip_address, user_agent
            FROM sessions ORDER BY issued_at;
              user_id                |    refresh_jti    | active |  ip_address   |     user_agent
--------------------------------------+-------------------+--------+---------------+---------------------
 00000000-0000-4000-8000-000000000012 | jti-priya-revoked | f      | 198.51.100.22 | Mozilla/5.0 Firefox
 00000000-0000-4000-8000-000000000011 | jti-alex-active   | t      | 203.0.113.10  | Mozilla/5.0 Chrome
```

Priya's session was revoked (`active = f`); her next refresh attempt will fail
regardless of the JWT's `exp` claim. The `ip_address` and `user_agent` columns
also feed the runtime password-spray detector (post 7): a spike in
`auth.failure` audit entries for one account from one IP auto-locks the user.

## MFA via KChat SSO

Kapp does not roll its own MFA. Authentication is delegated to KChat SSO
(or email credentials as a fallback), so second-factor enforcement, device
trust, and adaptive auth inherit whatever the identity provider configures.
This is deliberate: MFA is a moving target, and a bespoke implementation is
a liability. The trade-off is that MFA strength is the operator's
responsibility, not Kapp's — documented as such in the compliance mapping.

## The sign-in surface

The login page is intentionally minimal: it accepts KChat SSO or email
credentials, sets the tenant context in the app shell, and lands on the
dashboard. The customer portal has its own isolated sign-in so external users
never receive an internal tenant session.

![Kapp login](../screenshots/00-login.png)

![Customer portal login](../screenshots/14-portal-login.png)

The next post covers what happens *after* authentication: what the verified
user is allowed to do.
