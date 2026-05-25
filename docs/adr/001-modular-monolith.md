# ADR-001: Modular monolith architecture

- Status:   Accepted
- Date:     2024-03-15
- Deciders: Platform leads
- Tags:     architecture, deployment

## Context

A multi-tenant ERP touches finance, CRM, inventory, HR, helpdesk,
manufacturing, agents, and integrations. Each domain has independent
data models and workflows but tight transactional requirements where
they intersect (e.g. a sale must write CRM, inventory, and finance in
one ACID transaction).

Two extremes were on the table:

1. **Pure microservices** — one service per domain, async
   coordination via events. Maximum independence; transactional
   coordination becomes a saga.
2. **Pure monolith** — one binary, one database, one process. Maximum
   transactional consistency; rebuilds and deploys block on every
   change.

We have a small team and an early-stage product. Operational
overhead is the dominant cost.

## Decision

Adopt a **modular monolith** with a small fixed set of long-running
binaries:

- `services/api` — HTTP + gRPC, all customer traffic
- `services/worker` — background work, outbox drain, scheduled actions
- `services/kchat-bridge` — KChat I/O isolation
- `services/importer` — bulk import workers
- `services/agent-tools` — agent capability runtime
- `services/kapp-backup` — per-tenant extract/restore CLI

All binaries share the same Go module (`github.com/kennguy3n/kapp-fab`),
the same internal packages, and (within a cell) the same PostgreSQL
cluster. Each domain (finance, CRM, …) is a Go package under
`internal/<domain>/` with a tight import boundary enforced by
`scripts/check_module_boundaries.sh` (planned).

## Alternatives considered

1. **Per-domain microservices**. Rejected: at our team size, the
   operational overhead (one set of deployment pipelines per service,
   one observability story per service, sagas instead of transactions)
   would slow product velocity without delivering proportionate
   benefit.
2. **Single binary**. Rejected: long-running workers, file imports,
   and KChat I/O have different scaling profiles and failure-blast
   radii than the API. Splitting them out is cheap (same Go module,
   different `main.go`) and pays back on day one.

## Consequences

- **Positive**:
  - Transactions across domains stay ACID; no saga code-paths to
    write/test.
  - Single Go module → instant cross-domain refactors with the type
    checker behind them.
  - Single DB cluster → uniform observability and backup story.
  - Small deploy surface (5 binaries instead of 50).
- **Negative**:
  - Cannot scale individual domains independently; the API is one
    horizontal-scale unit. Mitigation: per-route timeouts, per-tenant
    rate limits, partition pruning at the DB layer.
  - Any new domain that adopts the platform inherits the module
    boundary discipline. Mitigation: linter + ADR review process.
- **Operational**:
  - HPA targets are at the binary level (api, worker, bridge);
    sizing rules in [CAPACITY_PLANNING.md](../CAPACITY_PLANNING.md).

## References

- [ARCHITECTURE.md](../../ARCHITECTURE.md)
- [PHASES.md](../../PHASES.md)
- [SCALING_RUNBOOK.md](../SCALING_RUNBOOK.md)
