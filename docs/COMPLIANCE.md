# Compliance and Data Governance

This document maps Kapp's technical controls to specific compliance
obligations. It is operator-facing — for auditor-facing evidence, see
the audit-pack output of `kapp-cli compliance pack` (planned in v0.2).

Authoritative cross-references:

- Encryption design: [SECURITY_REVIEW.md](./SECURITY_REVIEW.md)
- Hardening checklist: [SECURITY_HARDENING.md](./SECURITY_HARDENING.md)
- Tenant data lifecycle: [DR_RUNBOOK.md](./DR_RUNBOOK.md)
- Audit-log schema: `migrations/000016_audit_hash_chain.sql`

---

## 10.1 GDPR Compliance

### 10.1.1 Data Subject Access Request (DSAR)

A tenant admin (acting as data controller) submits a DSAR for one of
their users. The operator extracts via `kapp-backup`:

```bash
go run ./services/kapp-backup extract \
  --db        "$KAPP_ADMIN_DB_URL" \
  --tenant    "$TENANT_ID" \
  --user      "$USER_ID" \
  --format    json \
  --out       /tmp/dsar_${USER_ID}.json
```

The `--user` filter is applied at extract time (planned in v0.2 — the
current binary extracts the entire tenant; until shipped, post-filter
with `jq '[.[] | select(.actor_id == "<USER_ID>" or .created_by ==
"<USER_ID>")]'`).

SLA: respond within 30 days of the request (GDPR Art. 12(3)).

### 10.1.2 Right to Erasure

Per-user erasure within a tenant:

```sql
-- Step 1: identify all rows owned/authored by the user
SET LOCAL app.tenant_id = '$TENANT_ID';
SELECT count(*) FROM krecords      WHERE created_by = $1;
SELECT count(*) FROM audit_log     WHERE actor_id   = $1;
SELECT count(*) FROM sessions      WHERE user_id    = $1;
SELECT count(*) FROM user_tenants  WHERE user_id    = $1;

-- Step 2: anonymise audit log entries (DO NOT DELETE — regulatory
-- retention applies). Replace identifiers with the constant
-- "ANONYMIZED" while preserving timestamps and event types.
UPDATE audit_log
SET actor_id = NULL,
    payload  = jsonb_set(payload, '{actor_email}', '"ANONYMIZED"')
WHERE actor_id = $1 AND tenant_id = $2;

-- Step 3: delete sessions
DELETE FROM sessions WHERE user_id = $1;

-- Step 4: delete user-tenant membership
DELETE FROM user_tenants WHERE user_id = $1 AND tenant_id = $2;

-- Step 5: delete the user row if it has no remaining memberships
DELETE FROM users
WHERE id = $1 AND NOT EXISTS (
  SELECT 1 FROM user_tenants WHERE user_id = $1
);
```

Erasure of the tenant itself (e.g. service termination) is the
`/api/v1/admin/tenants/{id}/destroy` flow — see
[DR_RUNBOOK.md](./DR_RUNBOOK.md) for the canonical procedure.

### 10.1.3 Data Processing Agreements

The repo ships a DPA template at `docs/legal/DPA-template.md` (planned).
It maps:

- The operator (`kapp-fab` deployer) as the **data processor**.
- The tenant admin as the **data controller**.
- Sub-processors enumerated: cloud provider, object-storage provider,
  email transactional provider.

### 10.1.4 Data Residency

Each cell carries a `region` column (`migrations/000041_cell_capacity.sql`):

```sql
SELECT id, region, max_tenants FROM cells;
```

Tenants requiring EU residency are placed in EU-region cells via the
placement policy ([MULTI_CELL_OPERATIONS.md §6.6](./MULTI_CELL_OPERATIONS.md#66-placement-policy)).
The wizard surfaces region selection during tenant onboarding.

---

## 10.2 SOC 2 Type II Controls Mapping

| Control                              | Kapp implementation                                                                                                                                |
| ------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| CC6.1 — Logical access               | JWT auth (`internal/auth/jwt.go`) + RBAC (`internal/authz`) + Postgres RLS policies on every tenant-scoped table.                                  |
| CC6.2 — Auth mechanisms              | HMAC-SHA256 (HS256) JWTs (rotation via `internal/auth/signer_provider.go`); session management in `migrations/000013_auth_sessions.sql`; MFA via KChat SSO. |
| CC6.3 — Access removal               | `POST /api/v1/admin/tenants/{id}/suspend` revokes all sessions immediately (`UPDATE sessions SET revoked_at = now()`); platform admin can de-provision. |
| CC7.1 — Change management            | CI/CD pipeline (`.github/workflows/ci.yml`); schema-migration safety checklist ([UPGRADE_RUNBOOK.md §7.5](./UPGRADE_RUNBOOK.md#75-database-migration-safety-checklist)). |
| CC7.2 — System monitoring            | Prometheus metrics, structured slog logs, OTLP traces. SLO + alert routing in [OBSERVABILITY_GUIDE.md](./OBSERVABILITY_GUIDE.md).                       |
| CC8.1 — Incident management          | [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md); on-call rotation in PagerDuty / Opsgenie; post-mortem within 5 business days for SEV-1/2.            |
| A1.2 — Backup and recovery           | Hourly per-cell backups; daily per-tenant extracts; tested DR restore in [DR_RUNBOOK.md](./DR_RUNBOOK.md) + [DATABASE_MAINTENANCE.md §5.5](./DATABASE_MAINTENANCE.md#55-backup-verification). |
| A1.3 — DR testing                    | Monthly chaos drill — see DR_RUNBOOK §6.                                                                                                            |
| PI1.1 — Processing integrity         | Append-only typed ledgers (`journal_lines`, `inventory_moves`); hash-chained `audit_log` (`migrations/000016_audit_hash_chain.sql`).                |
| C1.1 — Confidentiality               | Per-tenant HKDF-derived AES-256 field encryption (`internal/tenant/encryption.go`); ZK Fabric AES-GCM for files; TLS 1.3 in transit.                 |

---

## 10.3 Data Retention

`data_retention_policies` (`migrations/000032_data_retention.sql`)
stores per-tenant overrides. The scheduler enforces defaults:

| Class                  | Default retention | Override range            | Notes                                          |
| ---------------------- | ----------------- | ------------------------- | ---------------------------------------------- |
| `audit_log`            | **7 years**       | 7 y → 10 y                | Regulatory floor; never below 7 y.              |
| `events`               | 90 days           | 30 d → 1 y                | Outbox / delivery state, not business records.  |
| `notifications`        | 30 days           | 7 d → 90 d                | Read receipts deleted with the row.             |
| `sessions`             | 24 h post-expiry  | not configurable          | Hard floor for forensics.                       |
| `webhook_deliveries`   | 30 days           | 7 d → 90 d                | Failed deliveries retained for replay.          |
| `import_staging`       | 7 days post-completion | fixed                | Drops after the import is committed.            |
| Files (object storage) | Per tenant policy | 30 d → 10 y               | Soft-delete then crypto-shred after retention.  |

The retention sweep runs daily at 03:00 UTC via the worker scheduler
and emits an audit trail entry for every deletion:

```sql
SELECT created_at, payload
FROM audit_log
WHERE action = 'retention.sweep.delete'
ORDER BY created_at DESC LIMIT 50;
```

---

## 10.4 Encryption at Rest and in Transit

**At rest:**

- **Database fields**: per-tenant AES-256 (HKDF-derived from
  `KAPP_MASTER_KEY` and `tenant_id` — see
  `internal/tenant/encryption.go`). Master key lives in KMS, NOT in
  the env file in production.
- **Files (ZK Fabric)**: AES-GCM, per-tenant HMAC-derived keys,
  managed by the ZK Object Fabric gateway.
- **Database disk**: cloud-provider native encryption (RDS / Cloud SQL
  managed disks). Verify with
  `aws rds describe-db-instances --query 'DBInstances[*].StorageEncrypted'`.

**In transit:**

- External: TLS 1.3 minimum (cert-manager + ACME).
- Internal service-to-service: mTLS via service mesh (Istio /
  Linkerd) **(planned)**; current default is TLS without client certs.
- Database: `sslmode=verify-full` with CA cert validation (the
  Makefile DSN defaults to `sslmode=disable` for the dev compose
  stack — never use this in production).
- NATS: TLS with client certificates for authentication.

**Key management:**

| Key                            | Storage             | Rotation                                    |
| ------------------------------ | ------------------- | ------------------------------------------- |
| `KAPP_MASTER_KEY`              | KMS (AWS/GCP/Vault) | Annual; see [DR_RUNBOOK.md §4](./DR_RUNBOOK.md) for the rotation procedure. |
| `KAPP_JWT_*` (signing keys)    | Secret provider     | 90 days, via JWT keyring rotation (`KAPP_JWT_PRIMARY_REF` + `KAPP_JWT_VERIFY_REFS`). |
| Per-tenant ZK fabric HMAC keys | ZK Fabric console   | On suspected compromise; per-tenant.        |
| TLS certificates               | cert-manager       | Auto-renewed; alert at < 7 days remaining (`KappCertExpiringSoon`). |

---

## 10.5 Audit Trail Integrity

The audit log is hash-chained — each row contains the SHA-256 of the
previous row in the same tenant. Definition in
`migrations/000016_audit_hash_chain.sql`.

Verification:

```bash
# Walk all chains in all tenants (slow — only for monthly drill):
kapp-cli audit verify --all

# Verify a single tenant:
kapp-cli audit verify --tenant "$TENANT_ID"
```

The CLI exits non-zero on any tampered row. A non-zero exit triggers
`KappAuditChainBroken` (see [ONCALL_PLAYBOOK.md §4.6](./ONCALL_PLAYBOOK.md#46-additional-alerts-to-document))
— SEV-1 security incident.

Direct SQL fallback (when the CLI is unavailable):

```sql
SELECT id, tenant_id, created_at,
       encode(prev_hash, 'hex') AS stored_prev,
       encode(row_hash,  'hex') AS stored_row,
       encode(
         digest(
           coalesce(prev_hash, '\x'::bytea) ||
           id::text::bytea ||
           created_at::text::bytea ||
           coalesce(payload::text, '')::bytea,
           'sha256'
         ),
         'hex'
       )                       AS recomputed
FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at;
-- A row where stored_row != recomputed is tampered with → SEV-1.
```

Tamper evidence is **detective**, not preventive — RLS + role
separation (`kapp_app` cannot bypass RLS; only `kapp_admin` has
BYPASSRLS) is the preventive layer.

---

## 10.6 Sub-Processors and Third-Party Disclosure

Every operator deployment must publish (at minimum) the list of
sub-processors that process tenant data, and notify tenants 30 days
before adding a new one. A starter list ships in
`docs/legal/sub-processors.md` (planned):

| Sub-processor          | Service                                      | Data accessed                  |
| ---------------------- | -------------------------------------------- | ------------------------------ |
| Cloud provider         | Compute, managed Postgres, KMS               | All tenant data at rest        |
| Object storage         | File uploads (ZK Fabric backend)             | Encrypted file ciphertext only |
| Email transactional    | Outbound notifications                       | Recipient address + message body for the specific email |
| Observability (Tempo)  | Distributed traces                           | Request metadata; no PII       |
| Observability (Loki)   | Structured logs                              | Request metadata + truncated payloads |

---

## 10.7 Periodic Compliance Tasks

| Cadence | Task                                                                                  | Evidence                                       |
| ------- | ------------------------------------------------------------------------------------- | ---------------------------------------------- |
| Daily   | Backup verification (`aws s3 ls`)                                                     | S3 inventory                                   |
| Daily   | Retention sweep audit trail walk                                                      | `audit_log WHERE action LIKE 'retention.%'`    |
| Weekly  | Backup-restore parity (`pg_dump` → scratch instance)                                  | `prod_counts.csv` vs `restore_counts.csv` diff |
| Weekly  | Admin API access review                                                               | `audit_log WHERE action LIKE 'admin.%'`        |
| Monthly | Full DR restore drill                                                                 | RUNBOOK chaos-drill log                        |
| Monthly | Audit-chain integrity verification (`kapp-cli audit verify --all`)                    | CLI exit code + log                            |
| Monthly | Vulnerability scan (`govulncheck`, `npm audit`, container image scan)                 | CI summary report                              |
| Quarterly | Access certification — every tenant admin re-attests their user list                 | KChat workflow output                          |
| Annual  | Master encryption key rotation                                                        | DR_RUNBOOK §4 log                              |
| Annual  | SOC 2 audit fieldwork                                                                 | Auditor letter                                 |
