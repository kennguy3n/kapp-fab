# ADR-007: Zero-Knowledge Object Fabric for files

- Status:   Accepted
- Date:     2024-06-04
- Deciders: Platform leads, security
- Tags:     storage, security, encryption

## Context

Tenants upload files: attachments, invoices, lesson materials,
exports. Files are larger than rows, often contain sensitive
information (financials, HR documents, customer PII), and have
different lifecycle and access patterns than relational data.

The naive "single shared S3 bucket with object prefixes" model works
operationally but doesn't satisfy a per-tenant
"the operator must not be able to read this without explicit
authorization" requirement that some enterprise customers ask for.

Options:

1. **Single shared bucket**. Easy. Operator can read.
2. **Bucket per tenant**, encrypted with KMS keys held by the
   operator. Easier per-tenant accounting. Operator can still read.
3. **Per-tenant key fabric** — keys derived from a tenant-specific
   secret that only the tenant (or a tenant-authorized operator
   action) can unlock. Operator cannot read at rest by default.

## Decision

Introduce the **Zero-Knowledge (ZK) Object Fabric** for tenant files:

- Each tenant gets a per-tenant bucket
  (`ZK_FABRIC_BUCKET_TEMPLATE` defines the naming pattern in
  `.env.example`).
- Per-tenant encryption keys are derived (HKDF) from a tenant-scoped
  secret managed by the ZK Fabric gateway, NOT from a single
  operator-held master key.
- The application accesses files through the gateway, which enforces
  tenant-scoped access tokens. The operator's runtime credentials
  cannot read the ciphertext from the raw bucket.
- A fallback **global MinIO** path exists for tenants who opt out
  (`internal/files/store.go::PerTenantS3Store`).

## Alternatives considered

1. **Shared bucket with object prefixes**. Rejected: operator can
   read at rest; per-tenant lifecycle policies (retention, deletion)
   are awkward to express at object granularity.
2. **Per-tenant S3 bucket with operator KMS keys**. Rejected:
   operationally clean but doesn't satisfy the "operator can't read"
   requirement.
3. **End-to-end encryption with client-held keys**. Rejected: would
   require the React client to hold and rotate decryption keys, and
   server-side processing (thumbnails, antivirus, OCR) becomes
   impossible.

## Consequences

- **Positive**:
  - Operator cannot read tenant files at rest by default.
  - Per-tenant key rotation is independent and doesn't ripple to
    other tenants.
  - Per-tenant lifecycle / retention rules are bucket-level.
- **Negative**:
  - More moving pieces (the ZK Fabric gateway) — more places to fail.
    Mitigation: gateway is stateless; HA via the same Kubernetes
    deployment patterns as the rest of the stack.
  - Per-tenant bucket count grows with tenant count. Mitigation:
    most clouds support 100+ buckets without paperwork; for larger
    scale, ZK Fabric multiplexes inside a smaller number of buckets
    by tenant prefix while retaining per-tenant keys.
- **Operational**:
  - Storage runbook: see [DR_RUNBOOK.md](../DR_RUNBOOK.md) §5
    for ZK Fabric backup / restore.
  - Key rotation playbook in [SECURITY_HARDENING.md §17.4](../SECURITY_HARDENING.md#174-secrets-rotation-schedule).

## References

- `internal/files/store.go`
- `.env.example` (ZK_FABRIC_* variables)
- [SECURITY_REVIEW.md](../SECURITY_REVIEW.md)
- [COMPLIANCE.md §10.4](../COMPLIANCE.md#104-encryption-at-rest-and-in-transit)
