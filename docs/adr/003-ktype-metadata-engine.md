# ADR-003: KType metadata engine

- Status:   Accepted
- Date:     2024-04-02
- Deciders: Platform leads, product
- Tags:     metadata, extensibility, records

## Context

Kapp ships a fixed core of records (`crm.deal`, `inventory.item`, …)
but its value depends on tenants being able to declare their own
custom record types — without redeploying the platform binary.
Common needs:

- Add a new field (with type, validation, default) to a record type.
- Add a new record type entirely (e.g. `legal.contract`).
- Customize the workflow (states, transitions, guards) for an
  existing type.
- Tighten per-field permissions for an existing type.

The naive approach — code-generated structs and hand-rolled
migrations per tenant — would push every tenant change through the
release pipeline.

## Decision

Introduce a metadata layer called **KType**:

- A KType is a JSON-Schema-like declaration of a record type:
  fields, their types, constraints, examples, workflow, ACLs.
- KTypes are stored in the `ktypes` table (`migrations/000001_initial_schema.sql`),
  versioned (one row per `(name, version)` pair).
- Records (`KRecord`) live in the partitioned `krecords` table; the
  payload is JSONB validated against the KType schema at read/write.
- Built-in KTypes are registered in Go at boot (see
  `services/api/main.go`); tenant-defined KTypes are registered via
  `POST /api/v1/ktypes`.
- The registry walks `pg_stat_statements`-aware caches so common
  KType lookups don't hit the DB on every request
  (`internal/ktype/cache.go`).

Workflow, validation, and field-level permissions are part of the
KType declaration — the same engine drives all three.

## Alternatives considered

1. **Code-gen per tenant**. Rejected: each tenant change becomes a
   pull request, breaking the "tenant admin configures their own
   record type" UX completely.
2. **Pure JSONB without metadata**. Rejected: no validation, no
   schema discovery, no API documentation. Records degenerate into
   "any old object".
3. **Per-tenant SQL tables**. Rejected: every custom field becomes
   `ALTER TABLE`. Concurrent tenants with overlapping field names
   add cognitive load. Field-level ACL enforcement is also harder.

## Consequences

- **Positive**:
  - Tenants self-service their own record types without a deploy.
  - Built-ins and customs use the same engine — fewer code paths to
    audit.
  - OpenAPI is generated *from* the KType registry at runtime
    (`services/api/openapi.go`), so the API documentation always
    matches the live schema.
- **Negative**:
  - JSONB queries are slower than a column query at the same depth.
    Mitigation: GIN indexes on common fields; promote hot fields to
    generated columns when needed.
  - Schema evolution must be backward-compatible at the JSONB level
    (additive only). Mitigation: per-version stored copies; the
    record store reads the version that wrote the row.
- **Operational**:
  - KType authoring guide: [KTYPE_AUTHORING_GUIDE.md](../KTYPE_AUTHORING_GUIDE.md).
  - Migrations to add new built-in KTypes never `ALTER` data; they
    only register the new metadata.

## References

- [KTYPE_AUTHORING_GUIDE.md](../KTYPE_AUTHORING_GUIDE.md)
- `internal/ktype/`
- `internal/record/store.go`
- ADR-004 (JSONB rationale)
