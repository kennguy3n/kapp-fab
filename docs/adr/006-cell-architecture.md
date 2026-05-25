# ADR-006: Cell-based horizontal scaling

- Status:   Accepted
- Date:     2024-05-07
- Deciders: Platform leads, SRE
- Tags:     architecture, scaling, multi-region

## Context

ADR-002 keeps every tenant inside one PostgreSQL cluster with RLS for
isolation. That cluster has a ceiling — vertical scaling buys
linear performance up to a point, then the planner cache, connection
overhead, and replication bandwidth all push back.

We also need data residency: an EU tenant cannot have their rows in
a US-region database.

Options:

1. **Database-per-tenant**. Already rejected in ADR-002.
2. **Sharding by `tenant_id`** inside one logical cluster (Citus,
   Vitess, custom application-level sharding). Adds operational
   complexity (resharding, distributed transactions).
3. **Cells** — replicate the entire stack (Postgres + API + Worker +
   NATS + …) per group of tenants. Each cell is an independent unit;
   tenants are placed onto a cell at onboarding.

## Decision

Adopt **cells**:

- One cell = one independent stack: Postgres primary + replicas,
  Kubernetes namespace, NATS cluster, Redis, object storage.
- A cell holds up to ~5,000 baseline tenants (sized to the
  [CAPACITY_PLANNING.md §1.1](../CAPACITY_PLANNING.md#1-resource-sizing-matrix) baseline).
- The control plane keeps `cells` and `tenants.cell_id`
  (`migrations/000041_cell_capacity.sql`); the L7 router consults
  this map to send each tenant's traffic to its cell.
- A `default` cell is seeded by the same migration; new tenants land
  there until placement routes them elsewhere.
- Cross-cell operations are explicit and rare (platform admin,
  consolidated reporting).

## Alternatives considered

1. **Citus / multi-shard PG inside one cluster**. Rejected: still one
   logical cluster, so any platform-wide incident is platform-wide.
   Cells give us blast-radius limitation in addition to capacity.
2. **Per-tenant sharding at the connection-pool layer**. Rejected:
   resharding stays a hard problem; cells defer it as "rebalance by
   moving tenants between cells".
3. **One big monolithic cluster forever**. Rejected: vertical
   scaling alone tops out below the platform's scale ambition.

## Consequences

- **Positive**:
  - Blast radius is bounded to one cell: a Postgres outage in cell-a
    leaves cell-b alone.
  - Data residency is a simple cell-placement decision.
  - The platform stays multi-tenant *within* a cell (the cheap path)
    and multi-cell *across* tenants (the expensive path); we pay
    cross-cell costs only when needed.
- **Negative**:
  - Cross-cell analytics requires a federated query path
    (Prometheus federation, central log aggregator, separate insights
    view).
  - Tenant migration between cells is non-trivial (suspend → extract
    → restore → reactivate, see
    [SCALING_RUNBOOK.md §2.5](../SCALING_RUNBOOK.md#25-tenant-migration-between-cells)).
- **Operational**:
  - Procedures: [MULTI_CELL_OPERATIONS.md](../MULTI_CELL_OPERATIONS.md)
    and [CAPACITY_PLANNING.md §1.3](../CAPACITY_PLANNING.md#13-cell-boundary-guidelines).
  - Autoscaling decisions logged to `platform_scale_events`
    (`migrations/000041_cell_capacity.sql`).

## References

- [MULTI_CELL_OPERATIONS.md](../MULTI_CELL_OPERATIONS.md)
- [CAPACITY_PLANNING.md](../CAPACITY_PLANNING.md)
- ADR-002 (RLS within a cell)
