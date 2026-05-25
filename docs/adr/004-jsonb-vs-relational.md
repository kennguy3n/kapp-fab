# ADR-004: JSONB vs. relational record storage

- Status:   Accepted
- Date:     2024-04-09
- Deciders: Platform leads, performance
- Tags:     database, records, performance

## Context

ADR-003 chose a metadata-driven KType engine. The next question:
**where do the actual record bytes live?**

1. **One column per field** — generate columns per KType, requiring
   a schema migration when fields are added.
2. **EAV (Entity-Attribute-Value)** — a `(record_id, field, value)`
   row per attribute.
3. **JSONB blob per record** with the schema enforced at the
   application layer and indexed via GIN / generated columns.

Performance, ergonomics, and the "tenants can add fields without a
deploy" requirement from ADR-003 dominate the choice.

## Decision

Store `KRecord` payloads as a single **JSONB `data` column** in the
`krecords` table:

```sql
CREATE TABLE krecords (
    tenant_id UUID NOT NULL,
    id        UUID NOT NULL,
    ktype     TEXT NOT NULL,
    ktype_version INT NOT NULL,
    data      JSONB NOT NULL,
    ...
) PARTITION BY RANGE (tenant_id);
```

- Validation lives at the application boundary
  (`internal/ktype/validate.go`).
- Frequent queries get **GIN** indexes (`USING gin(data jsonb_path_ops)`)
  or **expression indexes** on hot fields (`((data->>'stage'))`).
- Hot fields can be **promoted to generated columns** when query
  patterns demand:
  ```sql
  ALTER TABLE krecords
    ADD COLUMN stage TEXT
    GENERATED ALWAYS AS (data->>'stage') STORED;
  CREATE INDEX ON krecords (tenant_id, ktype, stage);
  ```

## Alternatives considered

1. **One column per field**. Rejected: every tenant field change
   becomes `ALTER TABLE`. Concurrent ALTERs on a partitioned table
   are operationally painful. Tenant-defined field names also
   inflate the column count without bound.
2. **EAV**. Rejected: queries become joins-per-attribute, killing
   the planner. JSONB is essentially "EAV with the storage engine
   on our side".

## Consequences

- **Positive**:
  - Zero schema changes when a tenant adds a field — only a KType
    registration.
  - One row per record means range scans behave predictably.
  - JSONB is binary-encoded — no string parse per access at the
    storage layer.
- **Negative**:
  - JSONB cell access is slower than a typed column at the same
    depth. Profiling shows ~10–20 ns per `->>` extraction; this is
    fine for non-hot paths but motivates the generated-column
    escape hatch for hot ones.
  - Schema typing is enforced in application code, not by the
    database. Mitigation: shared KType registry + CI tests.
- **Operational**:
  - Query tuning playbook in [PERFORMANCE_TUNING.md](../PERFORMANCE_TUNING.md).
  - Maintenance procedures in [DATABASE_MAINTENANCE.md §5.6](../DATABASE_MAINTENANCE.md#56-query-performance-monitoring).

## References

- ADR-003 (KType engine)
- [PERFORMANCE_TUNING.md](../PERFORMANCE_TUNING.md)
- `internal/record/store.go`
