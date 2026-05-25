# Architecture Decision Records

This directory holds the Architecture Decision Records (ADRs) for
Kapp. An ADR captures a single significant decision, the context that
forced it, the alternatives considered, the choice, and the
consequences (positive and negative).

ADRs are written when the decision is made; they are amended only by
**superseding** them with a new ADR that links back. The history is
the value — we don't rewrite past decisions.

---

## Process

1. Open a PR with the proposed ADR, status **Proposed**.
2. Discuss in the PR. The maintainers' review is the decision body.
3. Once consensus is reached:
   - Approve: merge with status **Accepted** and date stamp.
   - Reject: close the PR; keep the discussion linked from a follow-up.
4. To overturn a decision later, write a new ADR that begins with
   "Supersedes ADR-NNN" and update the superseded ADR's status to
   **Superseded by ADR-MMM**.

Number ADRs contiguously (001, 002, ...). Numbers are never reused
even if an ADR is rejected. Keep one decision per file.

---

## Template

```markdown
# ADR-NNN: <short title>

- Status:   <Proposed | Accepted | Superseded by ADR-MMM | Deprecated>
- Date:     YYYY-MM-DD
- Deciders: <names>
- Tags:     <tags>

## Context
<What is the problem? What forces (technical, organizational, regulatory)
are in play?>

## Decision
<What was decided? Be specific. A reader should not have to infer the
choice from the rationale.>

## Alternatives considered
1. <Option A> — pros / cons.
2. <Option B> — pros / cons.

## Consequences
- Positive: <enabled capabilities, simplifications>
- Negative: <costs, trade-offs, limits>
- Operational: <runbooks affected, monitoring needed>

## References
- <Links to related ADRs, docs, code, RFCs>
```

---

## Index

| ADR  | Title                                                  | Status   |
| ---- | ------------------------------------------------------ | -------- |
| 001  | [Modular monolith architecture](./001-modular-monolith.md)             | Accepted |
| 002  | [PostgreSQL with Row-Level Security](./002-postgresql-rls.md)          | Accepted |
| 003  | [KType metadata engine](./003-ktype-metadata-engine.md)                | Accepted |
| 004  | [JSONB vs. relational record storage](./004-jsonb-vs-relational.md)     | Accepted |
| 005  | [Transactional outbox for events](./005-outbox-pattern.md)              | Accepted |
| 006  | [Cell-based horizontal scaling](./006-cell-architecture.md)             | Accepted |
| 007  | [Zero-knowledge object fabric for files](./007-zk-object-fabric.md)     | Accepted |
| 008  | [gRPC plugin SDK over WASM / native plugins](./008-grpc-plugin-sdk.md)  | Accepted |
| 009  | [Append-only ledgers for finance / inventory / HR](./009-append-only-ledgers.md) | Accepted |
| 010  | [Leader election via PostgreSQL advisory locks](./010-leader-election.md) | Accepted |
