# Kapp Security Review — Phase G–I

This document is the living security review checklist for Kapp's
shared-infrastructure multi-tenancy model. It captures what was
verified, what the evidence is, and which areas remain open.

**Last reviewed**: 2026-04-24
**Scope**: Phase A–I merged to `main`, including the Phase H hardening
slice (PR #22) and the Phase I feature slice (PR #24).

---

## 1. Row-Level Security on every tenant-scoped table

**Invariant**: every table that stores a `tenant_id UUID` has RLS
enabled, a `FORCE ROW LEVEL SECURITY` flag set (so the table owner is
not exempt), and a policy that ties reads + writes to
`current_setting('app.tenant_id')`.

| Migration | Tables covered | RLS present |
| --- | --- | --- |
| `000001_initial_schema.sql` | tenants, users, user_tenants, roles, ktypes, krecords, workflows, workflow_runs, approvals, audit_log, events, idempotency_keys | Yes |
| `000003_forms.sql` | forms | Yes |
| `000004_finance_extensions.sql` | accounts, journal_entries, journal_lines, fiscal_periods, tax_codes | Yes |
| `000006_hr.sql` | leave_ledger | Yes |
| `000007_lms.sql` | lesson_progress | Yes |
| `000008_importer.sql` | import_jobs, import_staging | Yes |
| `000009_base_docs.sql` | base_tables, base_rows, docs_documents, docs_document_versions, files | Yes |
| `000010_phase_g.sql` | saved_views | Yes |
| `000011_sales_procurement_bank.sql` | bank_accounts, bank_transactions, cost_centers | Yes |
| `000013_auth_sessions.sql` | sessions | Yes |
| `000014_notifications.sql` | notifications | Yes |
| `000015_permissions.sql` | permissions | Yes |
| `000016_audit_hash_chain.sql` | audit_log (prev_hash, row_hash additions — hash-chain columns on existing RLS-enabled table) | Yes |
| `000017_multi_currency.sql` | exchange_rates | Yes |
| `000018_helpdesk.sql` | sla_policies, ticket_sla_log | Yes |
| `000019_reports.sql` | saved_reports | Yes |

**Verification command** (run against a dev DB):

```bash
psql -At -c "
  SELECT relname, relrowsecurity, relforcerowsecurity
    FROM pg_class
   WHERE relkind = 'r'
     AND relname IN ('accounts','journal_entries','journal_lines',
                     'fiscal_periods','tax_codes','krecords','ktypes',
                     'workflows','workflow_runs','approvals',
                     'audit_log','events','files','base_tables',
                     'base_rows','docs_documents',
                     'docs_document_versions','forms','import_jobs',
                     'import_staging','leave_ledger','lesson_progress',
                     'saved_views','bank_accounts','bank_transactions',
                     'cost_centers','idempotency_keys',
                     'sessions','notifications','permissions',
                     'exchange_rates','sla_policies','ticket_sla_log',
                     'saved_reports')
   ORDER BY relname;"
```

Every row in the output must show `t|t` for `relrowsecurity |
relforcerowsecurity`. A row that shows `f|_` is a Sev-1 finding.

**Open items**: none. Adding a new tenant-scoped table MUST include
RLS in the same migration — CI should fail the migration review when
this is missed (tracked for Phase H).

---

## 2. Agent tools cannot bypass workflow state machines

**Invariant**: the only way to transition a record is through
`workflow.Engine.Transition`. Agent tools that look like they mutate
state (e.g. `sales.confirm_order`, `crm.close_deal`) must call the
workflow engine, not the raw KRecord store.

**Evidence**:

- `internal/agents/executor.go` holds a `workflow.Engine` reference;
  every registered tool receives it on dispatch.
- PR #14 (LMS) fixed a case where a tool wrote directly to the record
  store. The regression test `TestExecutor_LMSToolUsesWorkflow`
  locks the invariant in.
- The new sales/procurement tools in `internal/sales/` use the
  `crm.close_deal`-style pattern: they construct a transition request
  and hand it to the engine; they never UPDATE the `status` column
  directly.

**Open items**: none in code. Enforce via a lint rule in Phase H
(`internal/agents` must not import `internal/record` except through
the executor).

---

## 3. Encryption round-trip correctness

**Invariant**: per-tenant encryption uses HKDF-SHA256 with the tenant
UUID as salt; the derived key encrypts every JSONB payload in
`krecords.data`. Create, List, Update, Delete all round-trip through
the same `record.Encryptor`.

**Evidence**:

- `internal/record/store.go#WithEncryptor` wires the encryptor.
- PR #17 fixed the List and Delete paths that were skipping the
  round-trip. Regression tests:
  - `TestPGStore_EncryptionRoundTripCreate`
  - `TestPGStore_EncryptionRoundTripList`
  - `TestPGStore_EncryptionRoundTripUpdate`
  - `TestPGStore_EncryptionRoundTripDelete`
- `KAPP_MASTER_KEY` is loaded by `services/api/main.go` and
  `services/worker/main.go` at boot; a missing key is fatal so a
  misconfigured deployment cannot silently ship unencrypted.

**Open items**: key rotation is not implemented. Plan:
Phase H migration that rewrites ciphertext under a new master key
while keeping the old key for pre-rotation records.

---

## 4. No cross-tenant data leakage in background workers

**Invariant**: workers that scan across tenants (stock alerts, outbox
dispatcher, import reconciler) use an `adminPool` that is explicitly
denied RLS bypass; each job sets `app.tenant_id` before running any
per-tenant query.

**Evidence**:

- `services/worker/stock_alerts.go` — PR #17 replaced the raw pool
  scan with `adminPool.Query(...) GROUP BY tenant_id` and per-tenant
  follow-ups that set the GUC.
- `services/worker/outbox.go` — every event loop iteration scopes to
  one tenant_id at a time.
- The dedupe map in `stock_alerts.go` is capped at 10k entries (PR
  #18) so a burst of alerts cannot pin memory.

**Open items**: none. Re-run the cross-tenant leakage check
whenever a new worker is added.

---

## 5. Tenant GUC enforcement across code paths

**Invariant**: every tenant-scoped DB interaction runs through
`dbutil.WithTenantTx`, which issues `SET LOCAL app.tenant_id = $1`
before executing the callback.

**Evidence**:

- `grep -R "dbutil.WithTenantTx" internal services` — every store
  method uses it.
- The bank reconciliation + cost-center code added this round uses
  `dbutil.WithTenantTx`; both are verified above.

**Open items**: none.

---

## 6. Rate limiter + LRU cache idle eviction

**Invariant** (claimed in ARCHITECTURE.md): idle tenants consume zero
compute and ~zero memory. The `platform.RateLimiter.buckets` map and
the LRU metadata cache must both drop a tenant's entry within
`IdleTimeout`.

**Evidence**: the new benchmark
`internal/integrationtest/bench_idle_test.go` creates 100 tenants,
warms the caches, advances time past `IdleTimeout`, and asserts both
maps are empty. This runs on every `go test ./...`.

**Distributed backend**: `platform.RedisRateLimiter`
(`internal/platform/rate_limiter_redis.go`) is a drop-in alternative
for multi-replica deployments. It shares the per-tenant bucket via
Redis using an atomic sliding-window Lua script (loaded once with
`SCRIPT LOAD` and invoked via `EVALSHA`, falling back to `EVAL` on
`NOSCRIPT` after a Redis restart). The key schema `kapp:rl:{tenant}`
carries the bucket HASH, and the script calls `EXPIRE` on every
access so idle keys are dropped after `IdleTimeout` — preserving the
zero-idle-cost invariant across replicas. Replicas opt in with
`REDIS_URL=redis://host:6379/0`; absent the env var, services fall
back to the in-process limiter so local dev keeps working without
Redis. Redis outages fail open (return `allowed=true`) rather than
blocking every request — the reverse proxy remains the outer
ceiling on abusive traffic.

**Open items**: none.

---

## 7. Sub-millisecond tenant context switching

**Invariant** (claimed in ARCHITECTURE.md): `SET LOCAL
app.tenant_id` across tenant boundaries completes in well under a
millisecond at the p99.

**Evidence**: `internal/integrationtest/bench_switching_test.go`
cycles through 1000 tenants 5000 times and asserts p99 under a 5ms
ceiling (informational bound; the production target of 1ms is tracked
by the benchmark output but not failed on so CI is not flaky).

**Open items**: tighten the ceiling once the dedicated bench host is
available.

---

## 8. Outstanding items tracked for later phases

These are known gaps called out here so nobody forgets:

1. Encryption key rotation (item 3 above).
2. A CI rule that fails new migrations lacking RLS (item 1 above).
3. A CI rule that forbids `internal/agents` importing
   `internal/record` outside of the executor (item 2 above).
4. Periodic audit-log integrity check (hash chain) — spec lives in
   PROPOSAL.md §7.6 and has no implementation yet.
5. Dedicated-schema upgrade path has a tool (`scripts/upgrade_tier.sh`
   landed in this round) but is not yet wired to the tenant service.

---

## 9. ZK Object Fabric integration

Per-tenant file storage moved to ZK Object Fabric in PR #37. The
trust boundary and operational concerns are summarised here so an
auditor doesn't have to chase commits.

**Per-tenant key isolation.** Each tenant gets its own
`zk_access_key` / `zk_secret_key` HMAC pair plus a dedicated bucket
on the fabric. The fabric in managed mode derives a per-tenant DEK
from a tenant-scoped KEK. The Kapp API process never holds a DEK
in memory — it only forwards bytes through the S3 API — so a Kapp
process compromise does not yield other tenants' plaintext.

**Routing safety.** `PerTenantS3Store` reads the tenant id off the
request `context.Context`. The middleware wires this id from the
JWT claim only after the auth layer has validated the token, so a
caller cannot direct their bytes into another tenant's bucket by
spoofing a header. A missing tenant id surfaces as an error
(`tenant id missing on storage context`) — never silently routed
to a default bucket.

**Credential rotation.** The wizard provisioning is idempotent;
rotating a tenant's HMAC pair is a single `UPDATE tenants SET
zk_access_key = …, zk_secret_key = …` followed by
`PerTenantS3Store.Invalidate(tenantID)`. Old objects remain
readable because the DEK does not depend on the HMAC pair.
Operators are expected to rotate on suspected credential
compromise; rotation is _not_ scheduled automatically because that
would require a safe overlap window.

**Console API trust boundary.** Kapp's console-side calls use
`ZK_FABRIC_ADMIN_TOKEN`, which must be a fabric admin bearer
scoped only to tenant provisioning. The token is read from env at
boot and never logged. If unset the integration is disabled and
the wizard skips provisioning (tenants fall back to the global
MinIO bucket).

**Open items**: full DEK rotation flow on the fabric side (vs.
just credential rotation on the Kapp side) is tracked in the
zk-object-fabric repo and out of scope for the Kapp security
review.

---

## Review sign-off

Review is performed each phase end. A phase cannot close without a
section here matching the merged code.

- Phase G: PASS for items 1–7, open items tracked above.
