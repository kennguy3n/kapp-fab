# ADR-010: Leader election via PostgreSQL advisory locks

- Status:   Accepted
- Date:     2024-09-12
- Deciders: Platform leads
- Tags:     coordination, workers, reliability

## Context

The worker (`services/worker`) hosts several jobs that must run
**exactly once at a time** across all worker replicas in a cell:

- Outbox drainer (ADR-005) — to preserve event ordering per tenant.
- Scheduled-actions runner — to avoid double-firing.
- Retention sweeper — to avoid deleting the same partition twice.
- Cell autoscaler decisions — one decision per tick, not N.

Options for coordinating "one leader at a time":

1. **etcd / Consul / ZooKeeper** lease.
2. **Kubernetes leases** (the `coordination.k8s.io/Lease` resource
   used by controller-runtime).
3. **Redis `SETNX` + TTL**.
4. **PostgreSQL advisory locks** (`pg_advisory_lock` / `pg_try_advisory_lock`).

We already have PostgreSQL in the critical path; adding etcd or
ZooKeeper to deploy and monitor is a meaningful operational cost.
Redis is already deployed but adds a second consistency story.

## Decision

Use **PostgreSQL advisory locks** for leader election:

- One advisory lock per "named role" (outbox drainer,
  retention sweeper, …) keyed by a stable hash of the role name.
- The worker process holds the lock for the duration it acts as
  leader; if the process dies, the backend's session ends and the
  lock is released automatically — no fencing token needed.
- The election helper is in `internal/platform/leader.go`; it emits
  the `kapp_leader_active{namespace,identity}` gauge so dashboards
  show which replica holds which role.
- The helper periodically renews and probes with `pg_try_advisory_lock`
  so a deadlocked Go routine doesn't accidentally retain leadership.

## Alternatives considered

1. **etcd / Consul / ZooKeeper**. Rejected: another distributed
   coordination system to operate, monitor, back up, upgrade. Not
   worth it when the workload is "elect a worker once per minute".
2. **Kubernetes Leases**. Considered. Rejected for two reasons:
   (a) The same election story should work outside Kubernetes
   (compose, bare metal, future serverless), and
   (b) the API server has higher per-call latency than a Postgres
   advisory lock at the cell scale we operate.
3. **Redis SETNX**. Rejected: Redis is a cache in our architecture
   (no persistence guarantee); coupling leadership to it introduces
   a third consistency model.

## Consequences

- **Positive**:
  - Zero new components in the stack.
  - Lock release on backend termination is automatic — no orphaned
    leaders to clean up.
  - Atomic election + state read in the same transaction — the
    leader can verify a baseline before acting.
- **Negative**:
  - The DB primary now has an availability dependency at the cell
    level (already the case, but more pronounced because the worker
    needs the lock to do anything).
  - Cross-cell coordination is **not** supported; this is a feature,
    not a bug — cells are intentionally independent (ADR-006).
- **Operational**:
  - Alert: `KappLeaderLost` (see
    [ONCALL_PLAYBOOK.md §4.4](../ONCALL_PLAYBOOK.md#44-kappleaderlost)).
  - Stale-lock cleanup procedure in the on-call playbook.

## References

- `internal/platform/leader.go`
- [ONCALL_PLAYBOOK.md §4.4](../ONCALL_PLAYBOOK.md#44-kappleaderlost)
- ADR-005 (outbox)
- ADR-006 (cells)
