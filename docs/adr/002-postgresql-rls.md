# ADR-002: PostgreSQL with Row-Level Security

- Status:   Accepted
- Date:     2024-03-22
- Deciders: Platform leads, security
- Tags:     database, security, isolation

## Context

A multi-tenant SaaS platform must isolate tenants such that a bug in
the application cannot leak one tenant's rows into another tenant's
query results. Three popular isolation models exist:

1. **Database per tenant** — strongest isolation, hardest operations.
   1000 tenants ≈ 1000 databases to back up, vacuum, monitor.
2. **Schema per tenant** — separate `tenant_<id>` schema per tenant.
   Easier connection model than per-DB but still N pg_dump streams,
   N analyze schedules.
3. **Shared schema with a discriminator column + RLS** — one set of
   tables; PostgreSQL enforces `tenant_id` matches on every query.

We need the operational simplicity of (3) without sacrificing the
isolation guarantee of (1).

## Decision

Use **shared schema with `tenant_id`-keyed Row-Level Security**:

- Every tenant-scoped table has a `tenant_id UUID NOT NULL` column.
- Every tenant-scoped table has `ALTER TABLE … ENABLE ROW LEVEL
  SECURITY` and a `tenant_isolation` policy of the form
  `USING (tenant_id = current_setting('app.tenant_id')::uuid)`.
- The application connects as `kapp_app`, which does **NOT** have
  `BYPASSRLS`. A separate `kapp_admin` role with `BYPASSRLS` exists
  for control-plane and forensic queries; its DSN is never wired
  into a customer-facing endpoint.
- Every request handler invokes
  `db.WithTenantTx(ctx, tenantID, func(tx) { … })` which `SET LOCAL
  app.tenant_id = $1` inside the transaction. Forgetting this is a
  compile-time error (the helper consumes the tenant from the request
  context).

`scale-out` scenarios that outgrow a single Postgres cluster are
handled by **cells** (see ADR-006), not by switching the isolation
model.

Tenants requiring schema isolation (regulatory or contractual) can
opt in to a dedicated schema via the tier-upgrade path
(`scripts/upgrade_tier.sh`) which uses a SECURITY DEFINER function
(`migrations/000042_tier_admin_role.sql`).

## Alternatives considered

1. **Database per tenant.** Rejected: 1000 backup streams, 1000
   connection pools, 1000 vacuum schedules. Operational cost grows
   linearly with tenant count.
2. **Schema per tenant.** Rejected for the same reason at scale; the
   migration story (`ALTER TABLE`s across N schemas) is also fragile.
3. **Application-only filtering**. Rejected: relies on every query
   author remembering to add `WHERE tenant_id = $1`. We had this
   model in an earlier prototype; missed predicates leaked rows in
   the test suite. RLS is the kernel-level defense-in-depth.

## Consequences

- **Positive**:
  - One backup pipeline, one vacuum schedule, one migration pass.
  - A missing `tenant_id` predicate at the application layer becomes
    a cross-table partition-pruning miss (slow query) instead of a
    cross-tenant leak (a SEV-1).
  - Per-tenant resource attribution is straightforward via the
    `tenant_id` column.
- **Negative**:
  - Cross-tenant aggregation queries require the admin role or
    explicit policy holes. Mitigation: keep these in `internal/admin/`
    behind audited routes.
  - Indexes must lead with `tenant_id` to benefit from partition
    pruning; this is a discipline issue.
- **Operational**:
  - CI gate `migration-rls-check.yml` enforces that every new
    tenant-scoped table ships with an RLS policy.
  - On-call check in [SECURITY_HARDENING.md §17.1](../SECURITY_HARDENING.md#171-pre-deployment-checklist).

## References

- [`ARCHITECTURE.md`](https://github.com/kennguy3n/kapp-fab/blob/main/ARCHITECTURE.md)
- [`migrations/000001_initial_schema.sql`](https://github.com/kennguy3n/kapp-fab/blob/main/migrations/000001_initial_schema.sql)
- [`migrations/000002_admin_role.sql`](https://github.com/kennguy3n/kapp-fab/blob/main/migrations/000002_admin_role.sql)
- ADR-006 (cell-based scale-out)
