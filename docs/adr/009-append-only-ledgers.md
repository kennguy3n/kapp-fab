# ADR-009: Append-only ledgers for finance / inventory / HR

- Status:   Accepted
- Date:     2024-08-09
- Deciders: Platform leads, finance, security
- Tags:     finance, ledger, audit, immutability

## Context

Financial postings, inventory movements, and HR leave / payroll
ledgers must be **auditable for years**: every change must be
reconstructable, no entry must be silently rewritten, and the
running total must always equal the sum of every committed entry.

ERPs that allow "edit an old journal entry" lose this property and
become non-auditable. Regulators (e.g. GoBD in Germany, ESEF in the
EU, SOX in the US) treat that as a non-starter.

## Decision

The following tables are **append-only typed ledgers** — INSERT-only,
with the running total computed from the row history:

- `journal_lines` — double-entry finance lines
  (`migrations/000003_finance.sql`).
- `inventory_moves` — quantity / cost movements
  (`migrations/000005_inventory.sql`).
- `leave_ledger` — HR leave balance changes
  (`migrations/000006_hr.sql`).
- `audit_log` — every meaningful action, with a SHA-256 hash chain
  added in `migrations/000016_audit_hash_chain.sql`.

Conventions:

- No UPDATE / DELETE on these tables — enforced by Postgres triggers
  (`pg_trigger` rejects non-INSERT operations for `kapp_app`).
- Corrections happen by inserting **compensating entries**, not by
  editing the original.
- A periodic verification job recomputes hash chains
  (`kapp-cli audit verify`) and posts `KappAuditChainBroken` on any
  mismatch
  ([ONCALL_PLAYBOOK.md §4.6](../ONCALL_PLAYBOOK.md#46-additional-alerts-to-document)).

## Alternatives considered

1. **Mutable ledger with audit log**. Rejected: audit log can be
   tampered if not hash-chained; tracing the "current" balance back
   through the audit log is fragile.
2. **Event-sourcing only** (no materialized ledger). Considered. The
   ledger is *materialised* on disk anyway (so balances are fast to
   read); the append-only convention is what gives it the audit
   property without the cost of replaying every event for every
   balance query.

## Consequences

- **Positive**:
  - Strong audit story for regulators (and for engineers debugging
    a tenant's accounts).
  - Hash chain makes tampering detectable cheaply.
  - "What did the balance look like on 2024-12-31?" is a simple
    `WHERE created_at <= '...'` sum.
- **Negative**:
  - The ledger tables grow without bound (in proportion to activity).
    Mitigation: partition by `tenant_id`; archive partitions to cold
    storage after the regulatory retention window.
  - Engineers must internalise the "compensating entry" model.
    Mitigation: API enforces it; UI surfaces "reverse" rather than
    "edit" for ledger rows.
- **Operational**:
  - Verification cadence in [COMPLIANCE.md §10.5](../COMPLIANCE.md#105-audit-trail-integrity)
    and [COMPLIANCE.md §10.7](../COMPLIANCE.md#107-periodic-compliance-tasks).
  - Backup includes all ledger tables — restore parity check in
    [DATABASE_MAINTENANCE.md §5.5](../DATABASE_MAINTENANCE.md#55-backup-verification).

## References

- `migrations/000003_finance.sql`
- `migrations/000005_inventory.sql`
- `migrations/000006_hr.sql`
- `migrations/000016_audit_hash_chain.sql`
- [COMPLIANCE.md §10.5](../COMPLIANCE.md#105-audit-trail-integrity)
