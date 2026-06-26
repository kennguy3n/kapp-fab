# Honest Gaps and the Roadmap to Stronger Confidentiality

**Tenant:** n/a — this post is about the platform's posture, not a demo tenant.

A security series that only lists what works is marketing. This post lists
what is verified, what is still open, and the customer-facing promise Kapp
will and will not make. The gaps below come straight from the living
confidentiality review (`docs/SECURITY_HARDENING_PLAN.md`) — they are tracked
there with the evidence that found them.

## What is verified today

The evidence in posts 1–7 is real query output against a live database running
the actual code paths. Summarising what that evidence proves:

- **RLS on every tenant-scoped table**, default-deny when no tenant GUC is set,
  enforced because the API connects as the non-superuser `kapp_app` role.
- **Per-tenant AES-256-GCM field encryption** with HKDF-derived keys and the
  `kapp:enc:v1:` ciphertext prefix, fail-closed in production on a missing
  master key.
- **Hash-chained, redacted audit log** — `prev_hash`/`row_hash` link every
  tenant's rows; sensitive fields are `<redacted>` with HMAC digests for
  change detection.
- **Redacted event outbox** — webhooks, SSE, and notifications receive only
  the redacted diff summary, never plaintext sensitive fields.
- **JWT auth with keyring rotation**, session revocation that beats token
  expiry, and the `X-Tenant-ID` header-injection hole closed.
- **Granular permissions** with object-scoped conditions and atomic revocation.
- **Per-tenant retention** with a 7-year audit floor and crypto-shredding for
  object storage.

## Verified gaps (with evidence)

These are open items the review flagged, not hidden problems:

| Gap | Evidence |
|---|---|
| Audit/event redaction is in place, but the **exporter** does not yet apply `FilterFields` | `internal/exporter/krecord_exporter.go` has no `field_permissions` references. |
| **Agent tools** receive full decrypted rows with no field filtering | `internal/agents/*_tools.go` return `existing.Data` to the model; no `FilterFields`/redaction in `internal/agents`. |
| **`kapp_admin` has `BYPASSRLS` + broad CRUD** | `migrations/000002_admin_role.sql`. A scoped `kapp_tier_admin` exists as a pattern in `000042`, but the split into readonly/maintenance/break-glass is not yet shipped. |
| **Object storage silently falls back to the global store** when a tenant is not ZK-provisioned | `internal/files/zk_fabric.go` `routeFor` returns `s.fallback`. The hardening plan adds a `KAPP_FILES_REQUIRE_ZK_FABRIC` fail-closed flag. |
| **Encryption key rotation** for the master key uses a dual-key window but the backfill sweep job is not yet automated | `SECURITY_REVIEW.md §8` open item. |
| **Internal service-to-service mTLS** via a service mesh is planned, not default | Current default is TLS without client certs. |
| **Auditor-facing evidence pack** (`kapp-cli compliance pack`) is planned for v0.2 | `docs/COMPLIANCE.md`. |

The Phase 1 hardening work (redact audit/events, sentinel leak CI test,
fail-closed master key across sidecars, disable object-storage fallback in
prod) is the slice that closes the plaintext-bleed surface; the near-term and
mid-term items (nested encryption, blind indexes, KMS-backed tenant KEKs,
agent redaction, export policy engine, privacy dashboard) are sequenced in the
hardening plan.

## The privacy tiers

The default posture — shared Postgres + RLS + classified field encryption +
redacted audit/events + ZK objects — is right for the ~5,000 SME tenants the
platform is built for. Higher-risk or higher-paying tenants get stronger
isolation as **upgrade tiers, not defaults**:

| Tier | Target | Storage | Confidentiality |
|---|---|---|---|
| **Standard SME** | default 5,000 tenants | shared Postgres cell | RLS, classified encryption, redacted audit/events, ZK objects |
| **Business Confidential** | finance/HR-heavy SMEs | shared Postgres or dedicated schema | KMS-backed tenant KEK, blind indexes, stricter admin access |
| **Regulated SME** | healthcare/legal/regional | dedicated DB or cell | isolated backups, custom retention, optional CMK |
| **Enterprise Private** | large customer | private deployment | customer-controlled infra and keys |

The deliberate choice is to **not** make "one database per SME tenant" the
default. That would sacrifice the cost and operational model that makes the
shared cell viable, and it is overkill for an SME whose threat model is "the
other tenants on the platform," not "a state actor with the database disk."

## The customer-facing promise we will make

> Kapp isolates tenant data at the database policy layer, encrypts classified
> customer fields with tenant-scoped keys, stores attachments in
> tenant-isolated encrypted object storage, redacts sensitive values from the
> audit/event pipeline, and gates operator access through scoped, audited
> admin roles.

## The promise we will *not* make

We will not claim "no one at Kapp can ever access your data." That is
inaccurate for the default SME tier: the application decrypts to serve
authorised users and agents, and the `kapp_admin` role can read across tenants
for control-plane operations. The honest version is that operator access is
**scoped and audited**, not impossible — and the hardening plan's break-glass
work makes even that access time-boxed and reason-coded. The tiers that
*approach* "the vendor cannot read your data" (customer-controlled keys,
private deployment) exist for buyers who need that property and are willing to
pay for the operational burden that comes with it.

## How to verify any of this

Every claim in this series is checkable against the repo:

- RLS policies: `migrations/000001_initial_schema.sql` and the per-table
  `CREATE POLICY tenant_isolation` in every later tenant-scoped migration.
- Encryption: `internal/tenant/encryption.go` (`DeriveKey`, `EncryptString`,
  the `kapp:enc:v1:` prefix).
- Audit hash chain: `migrations/000016_audit_hash_chain.sql` and
  `internal/audit/store.go` (`computeRowHash`, `lockTenantChain`).
- Redaction: `internal/record/redact.go` (`RedactData`, `DiffSummary`,
  `eventSummaryPayload`).
- Hardening plan and verified gaps: `docs/SECURITY_HARDENING_PLAN.md`,
  `docs/SECURITY_REVIEW.md`.

The demo seeding program that produced the query output in this series lives
at `cmd/seed-security-demo/main.go` and runs against the same docker-compose
Postgres stack any operator can bring up with `docker compose up -d postgres`.
Re-run it, re-run the queries, and check the output yourself. That is the
standard we want this series to be held to.
