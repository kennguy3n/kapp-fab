# Trust by Design: How Kapp Handles Privacy and Security

This is an engineering-facing series on how Kapp isolates, encrypts, audits, and
governs tenant data on a **shared multi-tenant platform**. The companion series
[*Kapp in Action*](../blog-series/index.md) shows the product; this one shows
the trust story underneath it.

Every query output in this series is real. We seeded a two-tenant demo
(**Acme Corp**, USD, US cell; **Globex GmbH**, EUR, EU cell) into a live
Postgres 16 instance running the actual Kapp schema, drove the real
`record.PGStore` + `tenant.KeyManager` + `audit.PGLogger` code paths to write
encrypted records and hash-chained audit entries, and then queried the
database back through the same `kapp_app` role the production API uses. No
output here is fabricated or hand-edited.

## The threat model in one paragraph

Kapp runs thousands of SME tenants on one Postgres cluster. The load-bearing
question is not "can a hacker break in from the outside" (that is the network
layer's job) but **"if a bug, a rogue query, or a compromised tenant lets one
tenant's code touch the database, can it read another tenant's data?"** Every
control in this series is ultimately an answer to that question, layered as
defence in depth: row-level security at the database, per-tenant encryption at
the field, redaction in the audit/event pipeline, scoped roles for operators,
and audited break-glass for the rare cross-tenant operation.

## The posts

1. [Tenant Isolation: Row-Level Security as the Load-Bearing Wall](./01-tenant-isolation.md) — why RLS, not application code, is the isolation boundary; the `app.tenant_id` GUC; the `kapp_app` role; default-deny.
2. [Authentication & Sessions: JWTs, Keyring Rotation, and Revocation](./02-authentication-sessions.md) — HS256/RS256 signing, the JWT keyring, session revocation, MFA via KChat SSO.
3. [Authorization: Roles, Permissions, and Fail-Closed Policy](./03-authorization-permissions.md) — RBAC, the normalised `permissions` table, object-scoped grants, field permissions, fail-closed enforcement.
4. [Encryption: Per-Tenant Keys from One Master Key](./04-encryption-at-rest.md) — HKDF-SHA256 derivation, AES-256-GCM field encryption, the `kapp:enc:v1:` ciphertext prefix, ZK Object Fabric, TLS.
5. [Audit & Integrity: An Append-Only, Hash-Chained, Redacted Trail](./05-audit-integrity.md) — the `audit_log` hash chain, redaction of sensitive fields, HMAC digests for change detection.
6. [Data Retention & Privacy: GDPR, Erasure, Residency, Crypto-Shredding](./06-data-retention-privacy.md) — DSAR, right to erasure, retention policies, data residency by cell, crypto-shredding.
7. [Operational Hardening: Rate Limits, Supply Chain, and Break-Glass](./07-operational-hardening.md) — rate limiting, idle eviction, dependency scanning, SBOM/SLSA, signed images, network policy, break-glass admin.
8. [Honest Gaps and the Roadmap to Stronger Confidentiality](./08-honest-gaps-roadmap.md) — what is verified, what is open, the privacy tiers, and the customer-facing promise we will and will not make.

## A note on honesty

This series is written to survive scrutiny from someone who already runs Xero,
ERPNext, or Odoo and will check our claims against the code. Where a control is
only partially implemented, where the default SME tier cannot guarantee
"no operator can ever read your data," or where a hardening item is still open,
the relevant post and the final post say so plainly. The last post lists the
verified gaps with the evidence that found them.
