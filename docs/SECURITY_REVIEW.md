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
5. Dedicated-schema upgrade is exposed as
   `POST /api/v1/admin/tenants/{id}/upgrade-tier` (Phase G). The
   API handler, the `scripts/upgrade_tier.sh` runbook, and any
   future tenant-service RPC all delegate to one shared library
   function `tenant.Promote` (`internal/tenant/tier.go`) which in
   turn invokes the SECURITY DEFINER function
   `public.promote_tenant_to_schema(uuid, text, text[])` installed
   by `migrations/000042_tier_admin_role.sql`.

   The function is owned by a dedicated `kapp_tier_admin` role
   (NOSUPERUSER, NOBYPASSRLS, NOCREATEDB) that holds only:
     * SELECT on every public table (read the source rows under
       the function's `SET LOCAL app.tenant_id` GUC, so RLS still
       filters to the target tenant)
     * UPDATE on the `tenants.schema` column (flip routing at the
       end of the upgrade)
     * USAGE/CREATE on `public` and CREATE on the database (so it
       can create the dedicated schema and tables inside it)
   PUBLIC has no privilege on the function. Callers need EXECUTE,
   which by default is granted only to `kapp_admin` — the same
   pool the API and runbook use today. A future scoped operator
   role can be granted EXECUTE without inheriting BYPASSRLS,
   closing the long-standing item 5 gap from prior reviews.

   The handler emits a `tenant.tier_upgrade` audit entry on
   success. The shell script remains in the repo as a manual
   fallback for break-glass scenarios when the API service is
   unavailable, but operators should prefer the API path because
   it leaves an audit-log trail and authenticates under the same
   JWT envelope as other admin surfaces.

   Status: closed (Phase G PR #3).

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

## 10. Auth sessions (Phase H)

`auth_sessions` stores issued JWT identifiers per tenant per user.
The threat model: a compromised admin in tenant A must not be able
to enumerate / revoke / impersonate sessions in tenant B; a
suspended tenant's tokens must stop working immediately; portal
JWTs must not unlock control-plane endpoints.

**RLS.** `migrations/000013_auth_sessions.sql` enables RLS with a
single policy: rows are visible iff `tenant_id =
current_setting('app.tenant_id')::uuid`. The `tenant_isolation`
policy is exercised by `phase_h_test.go`'s
`TestAuthSessionsRLSIsolatesTenants`. Both insert (issuance) and
delete (revocation) honour the GUC.

**Suspension revocation.** `internal/tenant/lifecycle.go` calls
`auth.SessionStore.RevokeAllForTenant` inside the same transaction
that flips `tenants.status = 'suspended'`. Even if a token is
still in a client cache, the next API call hits the auth
middleware which loads the session row and returns 401 once the
row is gone. There is no "grace window" — suspension is
authoritative.

**Per-tenant session limits.** A tenant on the free plan is
capped at N concurrent sessions per user; the limit is enforced
in `auth.SessionStore.Issue` under a row-level lock so two
concurrent logins cannot both exceed the limit. Limit drops are
plan-driven (downgrade reduces N → oldest sessions get pruned).

**Portal vs standard scope isolation.** Portal JWTs carry a
`scope: "portal"` claim and a `kapp:portal_user_id` subject; the
control-plane middleware rejects portal-scoped tokens before any
handler runs. Conversely, the portal handlers reject any token
that does not carry `scope: "portal"`. The two scopes share the
HMAC signing key but cannot be substituted.

**Open items**: refresh tokens are not yet rotated on use (low
priority — sessions are short-lived).

---

## 11. Helpdesk (sla_policies, ticket_sla_log, customer portal)

Helpdesk tables are tenant-scoped and enforce isolation through
the same default-deny RLS policy used everywhere else. The
customer portal adds a public-facing surface that bypasses the
standard JWT path, so it gets extra attention here.

**RLS on policy + log tables.** `migrations/000018_helpdesk.sql`
enables RLS on `sla_policies` and `ticket_sla_log` with the
canonical `tenant_isolation` predicate. `phase_i_test.go`'s
`TestRLSIsolatesPhaseITables` provisions two tenants and asserts
that tenant B reads zero rows from each table after tenant A
inserts. Both `UpsertPolicy` / `LogSLAEvent` write paths require a
non-nil `app.tenant_id`; missing GUC defaults to deny.

**Portal authentication.** The customer portal issues magic-link
tokens scoped to a single ticket-tenant pair. Tokens have a 60-min
TTL and are single-use (`portal_users` tracks `last_used_at`). The
token claim carries `scope: "portal"` (see §10) and is rejected by
the standard middleware. Portal handlers also enforce a `KType`
guard: the only handlers reachable from a portal token are the
ticket read/comment/attachment endpoints — no records, no agent
tools, no exports.

**FeaturePortal gate.** `services/api/portal_handlers.go` reads
the per-tenant `FeaturePortal` flag before serving any portal
request. Tenants on free/starter plans get a 404 even with a
valid token. The gate prevents accidental portal exposure on a
tenant that disabled it after a plan downgrade.

---

## 12. Reporting (saved_reports, report runner, sharing)

The Phase H report builder accepts a JSON definition (columns,
filters, group-by, aggregations) and compiles it to SQL. The two
risks: (a) cross-tenant reads via SQL injection in column /
filter names, and (b) cross-tenant reads via a shared report.

**RLS on saved_reports.** `migrations/000019_reports.sql` enables
RLS; the `report_sharing` follow-up (this PR) adds `visibility`
+ `shared_with` columns but keeps the RLS predicate identical so
a "public" report in tenant A is still invisible to tenant B.
Visibility is a per-row gate _within_ a tenant — not a
cross-tenant escape.

**SQL injection prevention.** `internal/reporting/builder.go`
constructs SQL by mapping a fixed allow-list of column /
aggregation tokens to safe SQL literals; user-provided values
flow only through `pgx` parameterised arguments, never string
concatenation. `Definition.Validate` rejects any column /
KType / order-by name that does not match
`^[a-zA-Z_][a-zA-Z0-9_.]*$`. The `phase_i_test.go`
`TestReportBuilderValidateRejectsBadInput` asserts the validator
on each axis (`SELECT`, `WHERE`, `ORDER BY`, `GROUP BY`) and is a
required gate before any SQL is emitted.

**Shared-report visibility.** `Store.ListVisible` (this PR)
filters by `owner_id = $userID OR visibility = 'public' OR
shared_with @> [user/role]`. The query stays inside the tenant
RLS scope so a malformed `shared_with` JSON cannot leak rows
across tenants. Soft-deleted records are excluded by every report
(`TestReportBuilderExcludesSoftDeleted`).

---

## 13. Multi-currency (exchange_rates)

Foreign-exchange rates are tenant-scoped because tenants on the
same cell may use different rate sources or audit-mandated rate
overrides.

**RLS on exchange_rates.** `migrations/000017_multi_currency.sql`
enables RLS with the canonical `tenant_isolation` predicate. The
composite primary key (`tenant_id`, `from_currency`,
`to_currency`, `rate_date`) carries `tenant_id` first so every
read is index-bounded inside the tenant. `phase_i_test.go`'s
`TestRLSIsolatesPhaseITables` covers cross-tenant probes.

**Rate lookup isolation.** `ExchangeRateStore.Convert` and
`GetRate` take an explicit `tenantID` and call
`dbutil.WithTenantTx`, so a missing/zero tenant id surfaces as an
error rather than silently picking a "default" rate. The
unrealized gain/loss job iterates tenants explicitly — never
runs as a single tenant-less sweep — so a misconfigured cron
cannot post FX adjustments to the wrong tenant.

---

## 14. Webhooks

Webhooks are an explicit egress channel, so the threat model
extends beyond cross-tenant isolation: a misconfigured webhook
URL must not exfiltrate other tenants' events, and the receiver
must be able to verify authenticity.

**HMAC-SHA256 signatures.** `services/worker/notifications.go#postWebhook`
signs the request body with a per-tenant secret pulled from the
`platform.webhook` KRecord. The signature is sent as
`X-Kapp-Signature: sha256=<hex>` and the payload includes a
`timestamp` field so receivers can reject replays. The secret is
generated at webhook-create time, stored encrypted via the
field-level encryption hook, and never logged.

**Event filter validation.** Webhook subscriptions carry an
`event_filters []string`. The runtime enforces a strict allow-list
match (no glob, no regex) — a malformed entry fails closed (zero
events delivered) rather than fail-open ("deliver everything"). A
short-form regression test in `services/worker/notifications_test.go`
asserts that a malformed `event_filters` entry blocks delivery.

**Async retry decoupling.** Failed deliveries persist a row in
`webhook_deliveries` with `next_retry_at` set. The retry loop is
out-of-band from the outbox drain so a slow / failing customer
endpoint cannot stall unrelated tenants' events. Each retry
re-checks the per-tenant feature flag + endpoint URL — a
disabled webhook stops delivering immediately even if rows are
already queued.

**Per-tenant fan-out.** Delivery is keyed off the originating
event's `tenant_id`; the retry loop joins `webhook_deliveries`
to `webhooks` on `(tenant_id, webhook_id)` so a webhook from
tenant A cannot pick up tenant B's queued event.

---

## 15. Print / PDF

Print templates render KRecord data into HTML/PDF for download.
The risk surface is straightforward: an authenticated user in
tenant A must not be able to render a record from tenant B, and
the download link must not bypass the access check.

**FeaturePrint gate.** `services/api/print_handlers.go` checks
the per-tenant `FeaturePrint` flag before resolving the template
or fetching the underlying record. Tenants without the flag get
a 404 — including a well-crafted attempt that supplies a
template id from another tenant.

**Fetch-based download (no header-less anchor).** The frontend
issues an authenticated `fetch` for the rendered PDF and
materialises a blob URL for download. We deliberately do not
use a plain `<a href="/api/v1/print/...">` because anchor
navigation drops the `Authorization` header in some browser
configurations; an unauthenticated request would be rejected by
the auth middleware but the failure mode is a confusing 401 in a
new tab rather than an inline error toast. The fetch path also
keeps the download under the standard CSRF-token check.

**Template scope.** Print templates are tenant-scoped (RLS on
`print_templates`); only records inside the same tenant can be
rendered against them. The renderer rejects any cross-tenant
record id at the resolver layer — well before any HTML is
emitted.

---

## 16. Data retention

`data_retention_policies` lets a tenant set per-category retention
windows (audit log, events, SLA log, notifications). The risk: a
sweep that ignores RLS could delete rows from another tenant.

**Per-tenant scope.** `internal/platform/retention.go`
`RetentionStore` reads / writes policies under
`dbutil.WithTenantTx`. The sweeper iterates tenants explicitly via
the admin pool, then calls `WithTenantTx(tenantID)` so every
`DELETE` runs under that tenant's RLS predicate. A bug that
forgets the `WithTenantTx` wrapper would surface immediately as a
zero-row delete — the policy table itself is RLS-scoped — rather
than as a silent cross-tenant wipe.

**RLS on the policy table.** `migrations/000030_data_retention.sql`
(via the existing canonical migration) enables RLS on
`data_retention_policies` with the standard `tenant_isolation`
predicate.

**Audit trail.** Each retention sweep emits an
`audit.retention.swept` event with the pre/post row counts per
category. The hash-chained audit log captures the deletion so a
tenant can later prove what was purged and when.

---

## 17. RBAC hardening (Phase RBAC)

Eight gaps from the RBAC analysis are closed in this PR. All
changes layer on top of the tenant_isolation RLS guarantee in §1
— authorization runs *after* a tenant is on the request.

**Multi-role membership.** `user_tenants.role` was a single text
column, so a user could only hold one role per tenant. Migration
`000049_user_tenant_roles.sql` adds a junction table
`user_tenant_roles (tenant_id, user_id, role_name)` with the
standard tenant_isolation policy, backfills active rows from
`user_tenants`, and keeps the legacy column populated as the
"primary" role for backward compatibility.
`PGEvaluator.queryPermissions` now scans every row in the
junction (falling back to the legacy column when the junction is
empty) and unions the permission packs from each role.

**Wildcard matching.** `Authorize` previously did exact string
comparison. `internal/authz/store.go::matchAction` now supports
three patterns: `"*"` (match anything), `"prefix.*"` (match any
action with that prefix), and exact match. Unit tests in
`store_test.go` cover the wildcard, prefix-only, and negative
cases.

**Role hierarchy.** Migration `000050_role_hierarchy.sql` adds a
nullable `parent_role` column to `roles` and seeds the default
chain (owner → tenant.admin → tenant.member, module roles also
inheriting tenant.member). `queryPermissions` walks the chain
with a recursive CTE bounded by `walkRoleChain`'s depth limit (5
levels) to prevent runaway loops if a custom hierarchy is
mis-edited.

**Record-level conditions.** The `permissions.conditions` JSONB
column was inert. `Evaluator.AuthorizeRecord(ctx, t, u, action,
resource, recordAttrs)` now evaluates two condition shapes:
`{"owner_only":true}` (matches when the record's `owner` or
`created_by` attribute equals the actor) and
`{"status_in":[…]}`. `services/api/records.go` calls
`AuthorizeRecord` from `update()` and `delete()` so a `crm.rep`
can only modify their own records unless they hold a higher
role.

**Granular module roles.** `tenant.Wizard.DefaultRoles()` now
seeds eight additional roles — `crm.rep`, `crm.manager`,
`inventory.admin`, `helpdesk.agent`, `helpdesk.manager`,
`sales.rep`, `procurement.rep`, and `reporting.viewer` — so the
KType permission strings already referenced in the schemas
resolve to a real role. The setup wizard UI was switched to a
multi-select to match.

**Role-management API.** `services/api/roles_handlers.go` exposes
CRUD on roles (`GET/POST/PUT/DELETE /api/v1/roles[/{name}]`),
permissions (`/{name}/permissions`), and per-user role
assignments (`/api/v1/users/{id}/roles`). Every route is gated
by `authz.Middleware(eval, "tenant.admin", "")`. Mutations
invalidate the LRU permission cache (`InvalidateUser` /
`InvalidateTenant`) so a revoked role takes effect on the next
request rather than after the 30-second TTL.

**Field-level access.** KType schemas may now declare a
`field_permissions` block. `internal/record/store.go::FilterFields`
strips fields the actor's roles are not allowed to read;
`FieldsForbiddenForWrite` returns the list of fields the actor
cannot mutate so the record handler can return 403 with a
specific error. Roles flow into the record store via
`platform.WithUserRoles` /
`platform.UserRolesFromContext`, which the auth middleware
populates after evaluator-side resolution.

**Middleware wiring.** `services/api/main.go` instantiates a
`PGEvaluator` once at startup and gates each module route group
(`/api/v1/finance`, `/hr`, `/inventory`, `/helpdesk`,
`/reports`, `/insights`, `/audit`, `/agents`, `/records`) with
`authz.Middleware`. `/records` uses a method-aware helper that
selects `krecord.read` for `GET` and `krecord.write` for
`POST/PUT/PATCH/DELETE`. Enforcement is opt-in via the
`KAPP_AUTHZ_ENFORCE` env var so Phase A dev/test setups without
a real JWT context keep working until JWT propagation is in
place; staging and production flip the flag on.

The integration test
`internal/integrationtest/rbac_test.go::TestAuthzMultiRoleAndHierarchy`
exercises the wizard seeding plus all four evaluator surfaces
(multi-role union, wildcard match, hierarchy inheritance,
owner_only condition) end-to-end against the live Postgres
fixture under `KAPP_TEST_DB_URL`.

---

## Review sign-off

Review is performed each phase end. A phase cannot close without a
section here matching the merged code.

- Phase G: PASS for items 1–7, open items tracked above.
- Phase H/I/J/K: PASS for items 9–16 (this PR).
- Phase RBAC: PASS for item 17 (this PR — closes the eight gaps
  identified in the RBAC analysis).
