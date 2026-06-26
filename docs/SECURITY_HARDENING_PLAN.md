# Security Hardening Plan — Confidentiality Posture

Status: **adopted**. Tracks the work to close the plaintext-propagation gap
identified in the multi-tenant confidentiality review while keeping the
shared-Postgres + RLS + per-tenant field-encryption default for the 5,000 SME
tenants.

Target posture:

> Shared Postgres + strict RLS + classified field encryption + redacted
> audit/events + per-tenant object encryption + break-glass admin controls +
> paid isolation tiers.

Do **not** make "one database per SME tenant" the default. Reserve dedicated
schema / DB / cell / private deployment for higher-risk or higher-paying
tenants (the repo already anticipates those upgrade tiers in
`migrations/000042_tier_admin_role.sql` and `internal/tenant/tier.go`).

## Verified gaps (evidence)

| Gap | Evidence |
| --- | --- |
| Plaintext propagates into audit/events | `internal/record/store.go` `Create` swaps `created.Data = plaintext` then `emit()` puts `r.Data` into the event payload and `audit.Entry.After = plaintext` (lines ~484-495, 1237-1249). `Update` does the same for diffs. |
| `audit_log` stores `before/after/context` as JSONB | `internal/audit/store.go` LogTx inserts the raw JSON (lines 92-109). |
| `events.payload` is JSONB | `internal/events/store.go` EmitTx (lines 46-49). |
| Encryption opt-in, top-level strings only | `internal/ktype/validator.go` `FieldSpec.Encrypted bool`; `encryptFields` walks top-level fields only. |
| `field_permissions` opt-in; unlisted fields unrestricted | `FilterFields` returns `data` unchanged when no rules. |
| `kapp_admin` has `BYPASSRLS` + broad CRUD | `migrations/000002_admin_role.sql`. (A scoped `kapp_tier_admin` already exists as a pattern in 000042.) |
| Object storage silently falls back to global store | `internal/files/zk_fabric.go` `routeFor` returns `s.fallback` when a tenant is not provisioned. |
| Agent tools get full decrypted rows, no field filtering | `internal/agents/*_tools.go` call `records.Get`/`Update` and return `existing.Data` to the model; no `FilterFields`/redaction anywhere in `internal/agents`. |
| Exporter does not apply field permissions | `internal/exporter/krecord_exporter.go` has no `FilterFields`/`field_permissions` references. |
| Worker persists full payload into notifications/emails | `services/worker/notifications.go` `"payload": json.RawMessage(e.Payload)` and `body = string(e.Payload)`. |

## Phase 1 — Immediate (stop the plaintext bleed)

### P0-1. Redact encrypted fields from audit + event payloads
- New helper `internal/record/redact.go`:
  - `RedactData(schema, data) json.RawMessage` — replaces every
    `encryptedFieldNames(schema)` value with `"<redacted>"` and records the
    field in a `"_redacted": [...]` array.
  - `DiffSummary(schema, before, after)` — returns `changed_fields`,
    `redacted_fields`, and per-redacted-field `*_hash_before/after` HMAC
    digests.
  - `tenantHMAC(tenantID, value)` — HMAC-SHA256 over the value using a
    tenant key derived with `infoLabel = "kapp.audit.hmac.v1"` (reuses
    `internal/tenant/encryption.go` HKDF).
- `internal/record/store.go`:
  - `Create`: keep the plaintext swap **only** for the in-memory `KRecord`
    returned to the API caller; pass `RedactData(...)` to `audit.Entry.After`
    and to the event payload.
  - `Update`: pass redacted `Before`/`After` and a `DiffSummary` to the
    audit `Context`; event payload carries the summary, not full `data`.
  - `emit()`: replace `"data": r.Data` with `id, ktype, version, status,
    changed_fields, redacted_fields, actor, snapshot`.

### P0-2. Sentinel leak CI test
- New `internal/record/leak_test.go` (+ `services/api` integration variant).
- Sentinel: `DO_NOT_LEAK_9f3c2a`. Create a KType with an `encrypted` field
  set to the sentinel; run `Create`/`Update`; assert the sentinel never
  appears in `krecords.data`, `audit_log.before/after/context`,
  `events.payload`, notification rows, SSE response bodies, worker log
  capture.
- CI gate (template: `internal/platform/security_fail_closed_test.go`).

### P0-3. Disable object-storage fallback in production
- `internal/files/zk_fabric.go` `routeFor()`: when
  `KAPP_FILES_REQUIRE_ZK_FABRIC` is true (default true in prod) and
  `resolver.ResolveZKCredentials` returns `!ok`, return a hard error instead
  of `s.fallback`.
- Wire the flag in `internal/platform/config.go` alongside the existing
  prod fail-closed master-key check.

### P0-4. Verify fail-closed `KAPP_MASTER_KEY` across all sidecars
- API already fails closed. Audit `services/worker/main.go`,
  `services/kchat-bridge/main.go`, `services/importer`,
  `services/agent-tools/main.go`. Add a shared boot assertion in
  `internal/platform` reused by every `main.go`.

### P1-5. Split admin DB roles
- New migration `migrations/000XXX_admin_roles_split.sql`:
  - `kapp_admin_readonly` — `SELECT` on control-plane tables, `BYPASSRLS`,
    used by `tenant.UserStore.GetUserTenants` and cross-tenant reads.
  - `kapp_admin_maintenance` — `NOSUPERUSER NOBYPASSRLS`, owns
    retention/purge jobs (extends the `kapp_tier_admin` pattern).
  - `kapp_breakglass` — `BYPASSRLS`, time-boxed, requires reason code;
    writes to a new immutable `admin_audit_log` table.
  - Narrow `kapp_admin` itself: revoke broad `INSERT/UPDATE/DELETE` where
    readonly suffices.
- Break-glass flow: reason + expiry in `admin_audit_log`; alert on
  cross-tenant reads outside an allowlist.

### P1-6. Centralize data classification
- Add `Classification string` to `ktype.FieldSpec` with values
  `public|internal|confidential|secret`. Make `encrypted: true` a derived
  consequence of `classification in {confidential, secret}`.
- Validator rule: KTypes declaring sensitive domain fields (salary, ssn,
  iban, bank creds, tokens) **must** carry a classification; reject
  registration otherwise.

### P1-7. Redact request bodies, errors, worker logs
- Extend the sentinel test (P0-2) to assert no plaintext in `slog` output
  from `services/worker` and error responses. Add a logging sanitizer that
  strips known ciphertext-prefix values and sentinel patterns.

## Phase 2 — Near term (2-6 weeks)

- Nested JSON encryption (dotted paths like `employee.bank.iban`).
- Blind indexes (`HMAC(tenant_search_key, canonical(value))`) for
  email/phone/tax_id; update `internal/record/search.go` `ListByField`.
- `tenant_keys` table + envelope encryption (KMS-backed for
  business/regulated tiers, HKDF for standard SME).
- Export policy engine: `internal/exporter/krecord_exporter.go` calls
  `record.FilterFields(data, schema, userRoles)` and drops `secret`-class
  fields.
- Agent tool redaction: shared wrapper applies `FilterFields` to every
  record returned by a tool using the invoking user's roles; tenant setting
  `ai.confidential_fields` (default off); audit entry listing which
  record/fields were surfaced to the agent.
- Tenant privacy dashboard + break-glass UI.

## Phase 3 — Mid term (6-12 weeks, tier productization)

- KMS/Vault-backed tenant KEKs, CMK for high-ACV, dedicated schema/DB/cell/
  private deployment tiers. Productize as upgrade tiers, not defaults.

## Privacy tiers

| Tier | Target | Storage | Confidentiality | Cost |
| --- | --- | --- | --- | --- |
| Standard SME | default 5,000 tenants | shared Postgres cell | RLS, classified encryption, redacted audit/events, ZK objects | lowest |
| Business Confidential | finance/HR-heavy SMEs | shared Postgres or dedicated schema | KMS-backed tenant KEK, blind indexes, stricter admin access | moderate |
| Regulated SME | healthcare/legal/regional | dedicated DB or cell | isolated backups, custom retention, optional CMK | high |
| Enterprise Private | large customer | private deployment | customer-controlled infra and keys | highest |

## Customer-facing promise

> "KChat isolates tenant data at the database policy layer, encrypts
> classified customer fields with tenant-scoped keys, stores attachments in
> tenant-isolated encrypted object storage, redacts sensitive values from
> audit/event pipelines, and gates operator access through audited
> break-glass controls."

Avoid claiming "no one at KChat can ever access your data" — inaccurate for
the default SME tier where the app decrypts to serve users and agents.
