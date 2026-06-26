# Tenant Isolation: Row-Level Security as the Load-Bearing Wall

**Tenant:** Acme Corp (USD, US cell) + Globex GmbH (EUR, EU cell)
**Persona:** an engineer evaluating whether one tenant can ever read another tenant's rows.

The single most important security property of a multi-tenant ERP is: **tenant
A cannot read tenant B's data, even if tenant A finds a bug in the API.** Kapp
enforces this at the database, not in application code, because application
code is exactly where bugs live.

## Why isolation belongs in the database

An application-level `WHERE tenant_id = ?` filter is only as strong as every
query author remembering to add it. One missed join, one raw SQL builder, one
new background worker, and a tenant sees another tenant's invoices. Postgres
Row-Level Security (RLS) moves the invariant into the database itself: the
policy is attached to the table, so *no query can return a row whose
`tenant_id` does not match the session's tenant context*, regardless of who
wrote the SQL. The application cannot forget the filter because the database
will not let it.

## The `app.tenant_id` GUC and default-deny

Every tenant-scoped table carries the same policy, generated once in the
initial schema and re-applied per table by every later migration that adds a
tenant-scoped table:

```sql
CREATE POLICY tenant_isolation ON krecords
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
```

The policy reads the tenant from the `app.tenant_id` custom GUC (Grand Unified
Configuration setting). Application code sets it inside every transaction via
`SET LOCAL app.tenant_id = $1` (wrapped by `dbutil.WithTenantTx`). The
`NULLIF(..., '')` is the subtle part: when the GUC is unset,
`current_setting('app.tenant_id', true)` returns `NULL`, the comparison
evaluates to `NULL`, and Postgres treats that as **reject**. That gives
default-deny behaviour for any connection that forgets to establish tenant
context — a direct DB session, a misconfigured worker, or a migration script.

We can prove the default-deny on the live database by connecting as `kapp_app`
(the role the API uses) and querying with no tenant context set:

```
kapp_app=> SELECT count(*) AS rows_visible_without_tenant_guc FROM krecords;
 rows_visible_without_tenant_guc
---------------------------------
                               0
```

Zero rows. Not "all rows," not "an error" — *zero*. The policy silently drops
everything when no tenant is established.

## The `kapp_app` role: non-superuser, no BYPASSRLS

RLS only protects you if the application connects as a role it actually applies
to. Postgres exempts superusers and the table owner from RLS by default. Kapp
therefore provisions a dedicated application role in the very first migration:

```sql
CREATE ROLE kapp_app LOGIN PASSWORD 'kapp_app_dev';
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO kapp_app;
```

`kapp_app` is **not** a superuser and has **no** `BYPASSRLS` privilege, so RLS
policies are enforced against it. The API, worker, and sidecars all connect as
`kapp_app`. The superuser (`kapp`) is reserved for schema migrations only. A
separate `kapp_admin` role (covered in post 7) has `BYPASSRLS` but is used
exclusively for cross-tenant control-plane reads, never for tenant data-plane
traffic.

The role configuration on the live database:

```
 rolname     | rolbypassrls | rolsuper
-------------+--------------+----------
 kapp        | t            | t        -- migrations only
 kapp_admin  | t            | f        -- control-plane reads only
 kapp_app    | f            | f        -- the API's role; RLS enforced
 kapp_tier_admin | f        | f        -- tenant-tier promotion only
```

## Evidence: one tenant cannot see the other

The demo seeds two tenants. Acme owns two `hr.employee` records; Globex owns
one. Here is what `kapp_app` sees under Acme's tenant context:

```
kapp_app=> BEGIN; SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000001';
kapp_app=> SELECT count(*) AS acme_rows, count(*) FILTER (WHERE ktype='hr.employee') AS acme_employees FROM krecords;
 acme_rows | acme_employees
-----------+----------------
         2 |              2
```

And the same connection, still under Acme's context, explicitly asking for
Globex's rows:

```
kapp_app=> SELECT count(*) AS globex_rows_visible_from_acme
            FROM krecords WHERE tenant_id = '00000000-0000-4000-8000-000000000002';
 globex_rows_visible_from_acme
-------------------------------
                             0
```

The `WHERE tenant_id = globex` clause is irrelevant — the RLS policy already
restricted the scan to Acme rows before the `WHERE` was evaluated. Switching
the GUC to Globex flips the view completely:

```
kapp_app=> SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000002';
kapp_app=> SELECT count(*) FROM krecords;
 count
-------
     1
```

This is the property the whole platform rests on: **the tenant context is the
only knob, and it is enforced below the application.**

## What RLS does not do

RLS prevents cross-tenant reads through the application role. It does **not**
encrypt data at rest (a database backup or disk snapshot still exposes
plaintext) and it does **not** protect against the superuser/owner. Those gaps
are closed by per-tenant field encryption (post 4) and the scoped admin roles
(post 7). RLS is the first wall, not the only one.

## The admin surface

The tenant management screen is where an operator sees the control plane. RLS
does not apply to the `tenants` table itself (it is control-plane, shared
metadata), so isolation there is enforced by routing those reads through the
`kapp_admin` role and the admin endpoints — not by a row policy.

![Tenant management](../screenshots/13-admin-tenants.png)

The next post covers how a request proves *which* tenant it belongs to: JWT
authentication and session revocation.
