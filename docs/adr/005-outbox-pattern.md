# ADR-005: Transactional outbox for events

- Status:   Accepted
- Date:     2024-04-23
- Deciders: Platform leads
- Tags:     events, consistency, reliability

## Context

Kapp emits events on every meaningful record / workflow / audit
mutation. Downstream consumers include:

- Webhook deliveries to tenants.
- Notification fan-out (KChat, email).
- Insights cache invalidation.
- Cross-domain workflows (e.g. invoice created → inventory move).

Two patterns compete:

1. **Direct publish** to NATS in the request handler. Easy to write;
   fails the dual-write problem — a crash between commit and
   publish silently drops events.
2. **Transactional outbox** — write the event row in the same DB
   transaction as the record change; a background drainer publishes
   from the outbox to NATS and marks the row delivered.

Kapp's audit-log story (ADR-009 — append-only ledgers) makes the
dual-write problem unacceptable: an event must exist if and only if
the underlying state change committed.

## Decision

Use a **transactional outbox**:

- Table: `events` (`migrations/000001_initial_schema.sql`).
  Partitioned by `tenant_id` like the other hot tables.
- Producer: each handler writes to `events` *inside the same
  transaction* as the state change. No NATS call in the handler.
- Drainer: `services/worker` runs an outbox loop
  (`services/worker/main.go`) that:
  1. Picks up undelivered events (`WHERE delivered_at IS NULL`).
  2. Publishes them to NATS (and per-event sinks: webhooks,
     notifications, etc.).
  3. Sets `delivered_at = now()` on success.
- Idempotency: every event has a stable UUID; consumers dedupe on it.
- Leader election (ADR-010) ensures one drainer per cell at a time
  to preserve order.

Metrics emitted by the drainer
(`services/worker/main.go`):

- `kapp_outbox_drain_duration_seconds{result}` — drain batch latency.
- `kapp_outbox_events_total{result}` — counter of drained events.

## Alternatives considered

1. **Direct NATS publish**. Rejected: dual-write problem; can lose
   events.
2. **Change Data Capture from the WAL** (Debezium / wal2json).
   Rejected: extra moving piece, more operational surface, and we
   need richer per-event metadata (subscribers, headers) than a raw
   WAL stream provides.
3. **Synchronous event publishing inside the transaction**
   (transactional NATS via 2PC). Rejected: NATS doesn't speak XA,
   and inventing a homegrown 2PC introduces more failure modes than
   it removes.

## Consequences

- **Positive**:
  - "Exactly-once-effects" downstream as long as consumers dedupe on
    event ID. Outbox commits with the state change; drainer never
    fails to publish an event that committed.
  - One uniform mechanism for every event type.
  - Recovery is trivial: a downed cell resumes drain on restart,
    nothing else to do.
- **Negative**:
  - Drain lag is now a first-class metric (`kapp_outbox_drain_duration_seconds`).
    Mitigation: alert + runbook in
    [ONCALL_PLAYBOOK.md §4.3](../ONCALL_PLAYBOOK.md#43-kappoutboxlag).
  - Outbox table grows; retention is bounded by a 90-day window
    (see [COMPLIANCE.md §10.3](../COMPLIANCE.md#103-data-retention)).
- **Operational**:
  - Alert: `KappOutboxLag` when `kapp_outbox_drain_duration_seconds > 30`.
  - Backup includes the outbox table — replays after restore.

## References

- `services/worker/main.go`
- [ONCALL_PLAYBOOK.md §4.3](../ONCALL_PLAYBOOK.md#43-kappoutboxlag)
- ADR-010 (leader election)
