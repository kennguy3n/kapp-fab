# Authorization: Roles, Permissions, and Fail-Closed Policy

**Tenant:** Acme Corp
**Persona:** Priya the HR manager (can read and approve employees up to a limit) vs. a sales rep who must not see HR data.

Authentication answers "who are you?" Authorization answers "what are you
allowed to do?" Kapp's authz layer runs *after* RLS — RLS guarantees you only
see your own tenant's rows; authz decides which of those rows you may read,
write, or approve, and which fields you may see.

## RBAC: the role → permission baseline

Every user-tenant membership carries a role on `user_tenants.role`. The
baseline permission map lives in `roles.permissions` as a JSONB array. The demo
seeds Acme with three roles:

```
kapp_app=> SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000001';
kapp_app=> SELECT name, permissions FROM roles ORDER BY name;
     name      |                  permissions
---------------+-----------------------------------------------
 hr_manager    | ["hr.employee.read","hr.employee.write","hr.employee.approve"]
 sales_rep     | ["crm.lead.read","crm.deal.write"]
 tenant_admin  | ["*"]
```

A `tenant_admin` gets the wildcard `"*"`; everyone else gets an explicit
allow-list. The evaluator resolves the user's role from `user_tenants`, reads
the matching `roles` row, and checks the requested `(ktype, action)` against
the list.

## The normalised `permissions` table: object-scoped grants

A JSONB blob is fine for coarse role-level grants, but it makes one thing
impossible: **object-scoped grants that can be revoked atomically and audited
as discrete rows.** "Priya can approve *this* invoice but not the rest" cannot
be expressed as a JSON array entry without losing revocation granularity.
Migration `000015` adds a normalised table:

```sql
CREATE TABLE permissions (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    role_name       TEXT NOT NULL,
    ktype           TEXT NOT NULL,
    action          TEXT NOT NULL,
    conditions      JSONB NOT NULL DEFAULT '{}'::jsonb,
    granted_by      UUID,
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, role_name, ktype, action)
);
```

The `conditions` JSONB carries scoped predicates. The demo seeds an HR-manager
approval grant capped at a salary amount, and a sales-rep deal-write grant:

```
kapp_app=> SELECT role_name, ktype, action, conditions
            FROM permissions WHERE revoked_at IS NULL ORDER BY role_name;
 role_name  |    ktype    | action  |      conditions
------------+-------------+---------+-----------------------
 hr_manager | hr.employee | approve | {"amount_max": 50000}
 sales_rep  | crm.deal    | write   | {}
```

The evaluator joins this table by role name; `roles.permissions` remains the
default until a tenant starts managing grants here. Revoking a grant is a
single `UPDATE permissions SET revoked_at = now()` — atomic, audited, and
immediate. The table is RLS-protected like every other tenant-scoped table.

## Role hierarchy

Migration `000050` introduces a role hierarchy so a senior role can inherit a
junior role's permissions without the JSON array being copied. The evaluator
expands a role into its full ancestor set before checking the allow-list, so
"manager inherits rep" is one row, not a duplicated permission list.

## Field permissions: column-level redaction

Beyond "can you read this record" there is "can you see this *field*." The
`field_permissions` mechanism lets a KType declare which roles may see which
fields. The record store's `FilterFields` applies it on read, so a sales rep
viewing an employee record sees the name and department but not the salary.
This composes with encryption (post 4): a confidential field is encrypted at
rest *and* filtered out of the response for roles that lack the grant.

## Fail-closed enforcement

The authz engine has an environment-gated posture: `KAPP_AUTHZ_ENFORCE=true`
(the default) means a request is **denied** when the policy engine cannot
decide — a missing role row, an unresolved KType, a transient lookup error.
The engine never fails open. The hardening checklist requires
`KAPP_AUTHZ_ENFORCE=true` in production and treats disabling it as a WARN-only
dev convenience that must never ship to staging.

## What this does not cover

Authz governs the API's view of a tenant's own data. It does not govern the
operator's cross-tenant access — that is the break-glass admin story in post 7.
And it does not protect the data once it leaves the database as plaintext to
serve a request — that is the encryption and audit-redaction story in posts 4
and 5.

![Feature flags & admin](../screenshots/13-admin-features.png)

The next post is the one most buyers ask about first: how data is encrypted at
rest.
