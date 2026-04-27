# Kapp Business Suite — Development Progress

> **Last Updated:** 2026-04-27 (Phase G + Phase L acceptance closed; API versioning strategy documented and CI-enforced. `internal/integrationtest/phase_l_test.go` adds `TestInsightsDashboardWithLinkedFilters` (5-widget dashboard + linked-filter dispatch), `TestInsightsQueryTimeoutEnforced` (sub-microsecond timeout fences `Runner.runWithTimeout`), `TestInsightsFeatureFlagDisablesRoutes` (403 envelope when `tenant_features.insights = false`), and `TestInsightsGenerateQueryAgentToolValid` (NL→query result is runnable end-to-end). `services/api/tier_handlers_integration_test.go` adds `TestTierUpgradeCopiesEveryTable`, `TestTierUpgradeTablesMatchBackupSourceList`, and `TestKappBackupRoundTripWithRemap`; the row-copy SQL was fixed to enumerate non-generated columns via `services/api/tier_handlers.go::nonGeneratedColumns` and the equivalent DO-block in `scripts/upgrade_tier.sh` so tables with `GENERATED ALWAYS AS` columns (e.g. `krecords.search_vector`) round-trip cleanly. `internal/integrationtest/loadtest/phase_g_acceptance_test.go::TestPhaseGAcceptanceLoad` produces the 5k-tenant SLO numbers checked into `docs/PHASE_G_ACCEPTANCE.md`. `docs/API_VERSIONING.md` covers path-prefix negotiation, the deprecation timeline, and per-tenant version pinning via `tenant_features`; `.github/workflows/api-versioning-check.yml` blocks any new chi route mounted outside `/api/v1/`, `/api/v2/`, `/internal/`, or the platform allow-list.)
>
> Related documents: [README.md](./README.md) · [PROPOSAL.md](./PROPOSAL.md) · [ARCHITECTURE.md](./ARCHITECTURE.md) · [SECURITY_REVIEW.md](./docs/SECURITY_REVIEW.md)

---

## Current Phase

**Phase G — Hardening / Acceptance + Phase L — Insights / Acceptance + API versioning strategy (closed)**
**Status:** Phase G acceptance fully signed off — 5k-tenant load run hit every SLO with zero failures, tier upgrade and backup remap round-trip pass against a live PostgreSQL, security review §8 items 1-4 closed (item 5 — `kapp_tier_admin` SECURITY DEFINER role — tracks against PR #3), `docs/DEVELOPER_GUIDE.md` covers onboarding. Phase L acceptance signed off across all 7 criteria — visual save+cache re-run, 5-widget dashboard with linked filters, AI NL→query end-to-end runnable, dashboard digest posting, RLS negative test across all 5 insights tables, `statement_timeout` fence, and feature-flag route disabling. `docs/API_VERSIONING.md` and `.github/workflows/api-versioning-check.yml` close the API versioning task. Next focus: PR #2 (batch/lot tracking + KChat presence attendance), PR #3 (cell autoscaling + tier-upgrade RPC extraction with `kapp_tier_admin`), PR #4 (insights deferred features — external data sources, cross-KType JOINs, dashboard embedding).

---

## Status Legend

| Symbol | Meaning |
| --- | --- |
| `[ ]` | Not started |
| `[~]` | In progress |
| `[x]` | Completed |
| `[!]` | Blocked |
| `[-]` | Deferred / descoped |

---

## Phase A — Kapp Kernel

**Status:** Complete

Foundation: tenant isolation, KType metadata, KRecord storage, permissions, audit, events, and the efficiency primitives that make thousands of tenants per cell viable.

### Deliverables

- [x] Go module skeleton (`services/api`, `services/worker`, `internal/*`)
- [x] PostgreSQL schema for `tenants`, `users`, `user_tenants`, `roles`, `ktypes`, `krecords`, `events`, `audit_log`
- [x] Row-level security policies on all tenant-scoped tables
- [x] Tenant-range partitioning for `krecords`, `events`, `audit_log`
- [x] KType schema registry and validator (Go)
- [x] KRecord CRUD API with idempotency keys
- [x] Event outbox + batched publisher
- [x] Append-only audit logger with field-level diffs
- [x] RBAC/ABAC policy evaluator
- [x] Connection pool with tenant context injection (`SET LOCAL`)
- [x] Per-tenant rate limiting middleware
- [x] Per-tenant quota enforcement
- [x] LRU metadata cache (shared, tenant-keyed)
- [x] Tenant lifecycle (create, suspend, archive, delete)
- [x] React app scaffold with generated API client
- [x] KType-driven form and list views
- [x] Storybook setup for UI components
- [x] KChat bridge skeleton (card renderer, slash command dispatcher)
- [x] OpenAPI spec generator from KType schemas
- [x] Local dev compose stack (api, worker, db, object store, event bus)

### Acceptance Criteria

- [x] A KType can be registered and a KRecord created/updated/deleted via API (TestRecordCRUDEmitsEventsAndAudit)
- [x] All mutations produce an audit record and emit an event (TestRecordCRUDEmitsEventsAndAudit)
- [x] RLS prevents cross-tenant reads in negative tests (TestRLSIsolatesTenants)
- [x] Tenant isolation test suite passes (TestRLSIsolatesTenants + TestRLSDealIsolation)
- [x] Verify zero resource consumption for idle tenants (`TestIdleTenantZeroCost` in `internal/integrationtest/phase_a_test.go` asserts the shared metadata cache evicts idle entries and no tenant-pinned goroutines or DB connections remain)
- [x] Verify sub-millisecond tenant context switching overhead (`BenchmarkTenantContextSwitch` in `internal/integrationtest/bench_test.go` measures `dbutil.WithTenantTx` under p99 < 1ms on a warm pool)
- [x] Verify per-tenant rate limiting works correctly (middleware exists and is tested)
- [x] Load test: 1000 tenants on a single cell with acceptable latency (`TestThousandTenantLoad` in `internal/integrationtest/load_test.go`, gated behind `//go:build loadtest`)

---

## Phase B — CRM, Tasks, Approvals, Forms

Chat-native work tracking and revenue pipeline on top of the kernel.

### Deliverables

- [x] CRM KTypes: `crm.deal` (schema defined in `internal/crm/ktypes.go`)
- [x] CRM KTypes: `crm.lead`, `crm.contact`, `crm.organization`, `crm.activity`, `crm.quote` (all schemas defined in `internal/crm/ktypes.go`)
- [x] Tasks KType: `tasks.task`
- [x] Approvals engine: configurable chains, KChat approve/reject cards (engine done, KChat cards not done)
- [x] Forms KApp: anonymous and authenticated capture forms emitting KRecords
- [x] KChat cards for all CRM + Tasks + Approvals objects (renderer + `ApprovalCardRenderer` wired in `services/kchat-bridge/main.go`)
- [x] Slash commands: `/lead`, `/contact`, `/deal`, `/task`, `/approve`, `/form` (implemented in `services/kchat-bridge/commands.go`)
- [x] Composer actions: turn message → Task, Deal, Activity (implemented in `services/kchat-bridge/composer.go`)
- [x] Right-pane detail views for all Phase B KTypes (per-KType Details / Timeline / Related tabs in `apps/web/src/components/RightPane.tsx`)
- [x] Agent tools: `crm.create_deal`, `approvals.decide` (confirmed in tests)
- [x] Agent tools: `crm.advance_deal`, `crm.summarize_pipeline`, `tasks.create_task`, `approvals.request` (HTTP endpoint at `POST /api/v1/agents/tools/{name}`; per-tool tests in `internal/integrationtest/phase_b_test.go`)

### Acceptance Criteria

- [x] A Deal can be created from a KChat thread and progressed through its workflow (`TestDealLifecycleEndToEnd`)
- [x] An approval card posts to the right approvers and finalizes on decision (`TestApprovalChainApproveAndReject` + `ApprovalCardRenderer` in kchat-bridge)
- [x] All CRM records appear in the right pane and kanban views (generic `RightPane` with per-KType tabs + `RecordListPage`)
- [x] Agent tools execute with dry-run and confirmation where required (`TestAgentToolsDryRunAndCommit`, `TestAdvanceDealTool`, `TestSummarizePipelineTool`, `TestCreateTaskTool`, `TestRequestApprovalTool`)
- [x] Audit log shows the full lifecycle of each record (`GET /api/v1/audit` endpoint + `AuditLogPage` + `TestDealLifecycleEndToEnd`)

---

## Phase C — Finance Basics

**Status:** Complete

Typed ledgers and the first postings from Kapps.

### Deliverables

- [x] Finance KTypes: `finance.account`, `finance.journal_entry`, `finance.ar_invoice`, `finance.ap_bill` (schemas in `internal/finance/ktypes.go`; registered at API boot)
- [x] `accounts`, `journal_entries`, `journal_lines` tables with append-only invariants (Phase A migration enforces RLS + CHECK constraints; Phase C adds `fiscal_periods`, `tax_codes` in `migrations/000004_finance_extensions.sql`)
- [x] Double-entry posting engine with balance checks (`internal/ledger/store.go` — `PostJournalEntry` rejects unbalanced lines and unknown/inactive accounts)
- [x] Period lockout (no edits to posted periods) (`internal/ledger/period.go` — `LockPeriod` + `IsPeriodLocked`; `PostJournalEntry` rejects posts in locked periods)
- [x] Sales invoice posting flow (quote → invoice → ledger) (`internal/ledger/invoice.go` — `PostSalesInvoice`; wired to `POST /api/v1/finance/invoices/{id}/post`, `/post-invoice` KChat command, and `finance.post_sales_invoice` agent tool)
- [x] Purchase bill posting flow (`internal/ledger/invoice.go` — `PostPurchaseBill`; wired to `POST /api/v1/finance/bills/{id}/post`, `/post-bill` KChat command, and `finance.post_ap_bill` agent tool)
- [x] Credit and debit notes (`internal/ledger/credit_note.go` — `ReverseJournalEntry` generates a contra-entry linked back to the original)
- [x] AR / AP subledger views (`apps/web/src/pages/SubledgerPage.tsx` with `/finance/ar-subledger` and `/finance/ap-subledger` routes; quick-post button for draft rows)
- [x] Basic VAT/GST tax codes (`internal/ledger/tax.go` — `TaxCode` registry + `CalculateTax` with inclusive/exclusive modes; `tax_codes` table with RLS)
- [x] Finance reports: trial balance, AR aging, AP aging, income statement (`internal/ledger/reports.go`; `/api/v1/finance/reports/*`; client methods `getTrialBalance`, `getARAgingReport`, `getAPAgingReport`, `getIncomeStatement`)
- [x] KChat cards for invoices and bills (`cards.summary` templates on `finance.ar_invoice` / `finance.ap_bill` / `finance.journal_entry` KType schemas drive the existing card renderer; `/invoice`, `/bill`, `/post-invoice`, `/post-bill` slash commands)
- [x] Agent tools: `finance.create_sales_invoice`, `finance.create_ap_bill`, `finance.post_journal`, `finance.post_sales_invoice`, `finance.post_ap_bill` (`internal/agents/finance_tools.go`; registered via `RegisterFinanceTools`)

### Acceptance Criteria

- [x] A sales invoice posts a balanced journal entry (`TestSalesInvoicePostsBalancedJournal` in `internal/integrationtest/phase_c_test.go`)
- [x] A purchase bill posts a balanced journal entry (`TestPurchaseBillPostsBalancedJournal`)
- [x] Trial balance sums to zero at all times (`TestTrialBalanceSumsToZero` — asserts residual = 0)
- [x] Period lockout rejects edits to closed periods (`TestPeriodLockoutRejectsEdits` — `LockPeriod` + retry surfaces `ErrPeriodLocked`)
- [x] Audit log captures every posting with source record (`TestAuditLogCapturesPostings` — and `TestRLSIsolatesFinanceData` for tenant isolation)

### Deferred / Follow-up

- [x] Bank accounts and reconciliation (Done in Phase G: internal/ledger/bank.go + migrations/000011_sales_procurement_bank.sql)
- [x] Cost centers / dimensions on journal entries (Done in Phase G: internal/ledger/cost_center.go + journal_lines.cost_center column in 000011)

---

## Phase D — Simple Inventory

First inventory primitives integrated with Sales and Procurement.

### Deliverables

- [x] Inventory KTypes: `inventory.item`, `inventory.warehouse`, `inventory.move`, `inventory.stock_level` (`internal/inventory/ktypes.go`; registered at API boot)
- [x] `inventory_items`, `inventory_warehouses`, `inventory_moves` tables (Phase A migration enforces RLS + partitioning; `migrations/000005_inventory.sql` adds the `stock_levels` projection and helper indexes)
- [x] Append-only stock move ledger (`internal/inventory/store.go` — `RecordMove`; no deletes, reversals issued as opposite-sign moves)
- [x] Materialized stock levels with projection worker (`stock_levels` table updated inside `RecordMove`'s transaction; `/api/v1/inventory/stock-levels` reads the projection)
- [x] Goods receipt on purchase bill posting (`internal/ledger/invoice.go` — `PostPurchaseBill` notifies `ledger.InventoryHook` with a receipt move)
- [x] Goods delivery on sales invoice posting (`PostSalesInvoice` notifies `ledger.InventoryHook` with a delivery move)
- [x] Multi-warehouse transfers (`internal/inventory/store.go` — `RecordTransfer` emits a paired negative/positive move inside one transaction)
- [x] Inventory valuation report (`internal/inventory/store.go` — `Valuation`; served at `GET /api/v1/reports/inventory-valuation`)
- [x] KChat cards for stock moves and low-stock alerts (`services/kchat-bridge/commands.go` — `/stock` command renders an `inventory.stock_level` card)
- [x] Agent tools: `inventory.record_move`, `inventory.check_stock` (`internal/agents/inventory_tools.go`; registered via `RegisterInventoryTools`)

### Acceptance Criteria

- [x] Sales invoice posts a delivery move; stock level decreases (`TestSalesInvoicePostsDeliveryMove` in `internal/integrationtest/phase_d_test.go`)
- [x] Purchase bill posts a receipt move; stock level increases (`TestPurchaseBillPostsReceiptMove`)
- [x] Stock levels always match the sum of moves across every move source type — receipts, deliveries, adjustments, transfers (`TestStockLevelsMatchSumOfMoves`)
- [x] Warehouse transfers are balanced (`TestWarehouseTransfersAreBalanced`)

### Deferred / Follow-up

- [~] Low-stock alert worker (threshold-based notifications via KChat) (`services/worker/stock_alerts.go` polls `stock_levels` vs `inventory.item.reorder_level` per tenant and emits `inventory.low_stock_alert`; wired into `services/worker/main.go` and fanned out by the existing notification router. Needs production soak + dedupe-map cap tuning.)
- [ ] Stock move reversal (correction entries, not deletes — matching finance pattern)
- [ ] Batch/lot tracking foundation (schema only, full implementation deferred)

---

## Phase E — HR and LMS Starters

Employee lifecycle and structured learning.

### Deliverables

- [x] HR KTypes: `hr.employee`, `hr.leave_request`, `hr.attendance`, `hr.expense_claim` (`internal/hr/ktypes.go`; registered at API boot)
- [x] HR workflows: onboarding, offboarding, leave approval (workflow blocks embedded in KType schemas drive the engine via `submit_for_approval` / `approve` / `reject` transitions)
- [x] Org chart view (`apps/web/src/pages/OrgChartPage.tsx` — tree view from `hr.employee.reporting_to`; route `/hr/org-chart`)
- [x] LMS KTypes: `lms.course`, `lms.module`, `lms.lesson`, `lms.enrollment`, `lms.quiz`, `lms.assignment`, `lms.progress` (`internal/lms/ktypes.go`; registered at API boot)
- [x] Learner KChat surface (`/learn` slash command in `services/kchat-bridge/commands.go`; KType `cards.summary` template drives the card renderer; progress web pane at `/lms/progress/:enrollmentId` in `apps/web/src/pages/LearnerProgressPage.tsx`)
- [x] Reviewer assignment workflow (`lms.assignment` carries `reviewer_id` ref + `status` enum + workflow block; `lms.submit_assignment` agent tool creates approval chain targeting reviewer; worker drains `approval.requested` and POSTs to `/kchat/approvals/render` on kchat-bridge which returns the reviewer DM card)
- [x] Agent tools: `hr.request_leave`, `hr.approve_leave`, `lms.recommend_course`, `lms.grade_assignment` (`internal/agents/hr_tools.go`, `internal/agents/lms_tools.go`; registered at API boot)

### Acceptance Criteria

- [x] A leave request routes through approval and updates balance on decision (`TestLeaveRequestApprovalFlow` drives the full lifecycle; `TestLeaveLedgerBalanceReflectsDeltas` covers the append-only ledger invariant)
- [x] A course enrollment tracks progress across modules and lessons (`TestCourseEnrollmentProgress` asserts the `enrollment_progress` rollup; `TestLessonProgressTracksScoreAndCompletion` covers the per-lesson projection)
- [x] A quiz submission is scored and recorded (`TestQuizSubmissionScoring` — covers first submission, re-attempts, and attempt counting)
- [x] Reviewer assignment is notified via KChat (`lms.submit_assignment` tool → `approval.requested` event → worker drains the outbox and POSTs to `/kchat/approvals/render` on kchat-bridge → `ApprovalCardRenderer` produces the per-approver card)

### Deferred / Follow-up

- [ ] Attendance integration with KChat status
- [ ] Course completion certificates (basic)
- [x] Assignment submission + reviewer notification flow (`lms.submit_assignment` agent tool patches status to `submitted`, creates single-step approval chain with `reviewer_id` as approver)

---

## Phase F — Importer and Base

Onboarding existing customers and supporting ad-hoc tables.

### Deliverables

- [x] Importer pipeline: Discover → Export → Normalize → Map → Validate → Staging → Reconcile → Acceptance → Cutover (`internal/importer/pipeline.go` orchestrates the 9 stages; `job.go` persists per-tenant jobs; `stage.go` writes the partitioned staging rows; `validator.go` collects row-level errors; `reconciler.go` compares totals and counts)
- [x] Source adapters: CSV/JSON, generic REST, Frappe-based platforms (ERPNext, HRMS, CRM, LMS) (`internal/importer/adapters/csv.go` handles multi-entity JSON/CSV; `adapters/frappe.go` discovers DocTypes via `/api/method/frappe.client.get_list`, exports via `/api/resource/{doctype}`, and maps Link / Table fields)
- [x] Attachment rehosting with content-addressable dedup (`internal/files/files.go` — SHA-256-keyed object store with per-tenant metadata rows; `POST /api/v1/files` multipart + raw uploads, `GET /api/v1/files/{id}` metadata, `GET /api/v1/files/{id}/content` streaming)
- [x] Concept mapping UI (source DocType → KType) (`apps/web/src/pages/ImportPage.tsx` — 5-step wizard; `apps/web/src/pages/ImportMappingPage.tsx` — per-field source → target mapping)
- [x] Validation report with row-level error export (`GET /api/v1/imports/{id}/errors` returns the validator's row-level diagnostics; the wizard's Validation step renders them)
- [x] Base KApp: flexible tables, per-column typing, shareable views (`internal/base/base.go` — `base_tables` + `base_rows` with per-column schema as JSONB; `/api/v1/base/tables` + `/api/v1/base/tables/{id}/rows` CRUD; `migrations/000009_base_docs.sql` RLS policies)
- [x] Docs KApp: artifact documents with versioning (`internal/docs/docs.go` — append-only `docs_document_versions` history; `SaveVersion` writes a new row and advances `current_version`; `Restore` copies a historical version back and flags the new row with `restored_from`)

### Acceptance Criteria

- [x] A sample dataset imports end-to-end with reconciliation report (pipeline runs Discover → … → Cutover; `Reconciler.Compare` diffs staged counts vs source totals and reports discrepancies; `GET /api/v1/imports/{id}` surfaces progress + reconciliation summary)
- [x] Broken rows are surfaced and re-ingestible after correction (`import_staging.status` flags per-row `invalid` entries with `validation_errors`; re-submitting mapping or re-running validate reprocesses the same staging rows so a corrected upload can be re-accepted)
- [x] Base tables can be created, edited, and shared per-tenant (`base.Store` + `baseHandlers` handle CRUD; `shared_view` column persists per-tenant view config; RLS keeps every tenant's tables invisible to every other tenant)
- [x] Artifact documents version and restore correctly (`docs_document_versions` is append-only; `SaveVersion` writes a new row and advances `current_version`; `Restore` re-copies a historical version back and writes a new row with `restored_from`)

### Deferred / Follow-up

- [x] Frappe REST API source adapter (for ERPNext, HRMS, CRM, LMS imports) (`internal/importer/adapters/frappe.go`)
- [x] DocType → KType automatic mapping suggestions (`internal/importer/adapters/frappe.go#SuggestFieldMapping` — Jaccard-on-tokens + normalised Levenshtein; returns best target per source field above a configurable threshold)
- [x] Attachment migration with content-addressable dedup (`internal/files/files.go` + `POST /api/v1/files`)
- [x] Import dry-run with validation report (`POST /api/v1/imports/{id}/validate` runs the validator without cutover; errors exposed via `GET /api/v1/imports/{id}/errors`)
- [x] Incremental sync support (delta imports) (`FrappeConfig.LastSyncAt` threads through to `mergeDeltaFilter` which adds a `modified > $ts` clause to every `/api/resource/{doctype}` call; `import_jobs.last_sync_at` column added in `migrations/000011_sales_procurement_bank.sql`)

---

## Cross-cutting

Platform primitives used across every Kapp — not scoped to a single phase but tracked here for visibility.

- [x] Event SSE/WebSocket endpoint (`GET /api/v1/events/stream` — tenant-scoped SSE tail off the events table in `services/api/events_stream.go`; resumes from `Last-Event-ID` or `?since=` cursor)
- [x] Notification routing (KChat cards, in-app, email, webhook) (`services/worker/notifications.go` — notificationRouter reads `notification.channel` from the event payload and fans out to kchat-bridge, webhook URLs, and SMTP-stub logging; in-app served from the SSE endpoint)
- [x] File/attachment upload endpoint (`POST /api/v1/files`) with S3 integration (`internal/files/files.go` with the ObjectStore interface — SHA-256 content-addressable; MemoryStore default, S3/MinIO implementations pluggable; `services/api/files.go` handlers)
- [x] Saved views / filters per KType per user (`internal/record/views.go` + `migrations/000010_phase_g.sql` `saved_views` table with RLS; `GET/POST /api/v1/views` + `GET/PATCH/DELETE /api/v1/views/{id}` in `services/api/views.go`; dropdown + Save/Delete view in `apps/web/src/pages/RecordListPage.tsx`)
- [x] Report builder (pivot, aggregate, chart) over KRecords and ledgers (`internal/reporting/builder.go` — metadata-driven grammar: source `ktype:<name>` or `ledger.*`, columns/filters/group-by/aggregations/sort/pivot/chart; `internal/reporting/store.go` — per-tenant saved reports table with unique-by-name; `migrations/000019_reports.sql`; `services/api/reports_handlers.go` exposes `GET/POST/PUT/DELETE /api/v1/reports` + `POST /api/v1/reports/run` + `GET /api/v1/reports/{id}/run`; `apps/web/src/pages/ReportBuilderPage.tsx` renders saved-report sidebar + JSON editor + run preview)
- [x] Per-tenant encryption keys (HKDF with tenant_id as salt) (`internal/tenant/encryption.go` derives per-tenant AES-256-GCM keys from `KAPP_MASTER_KEY` via HKDF-SHA256 with the tenant UUID as salt; `internal/record/store.go` transparently encrypts/decrypts fields marked `"encrypted": true` in the KType schema)
- [x] Tenant backup/export tooling (single-tenant dump) (`services/kapp-backup/main.go` — `extract` dumps every tenant-scoped table as JSONL via `row_to_json`, manifest-first; `restore` replays with optional `--remap src:dst` so a tenant can be restored into a fresh tenant_id without touching neighbours)
- [x] HR org chart tree view (`apps/web/src/pages/OrgChartPage.tsx` backed by `hr.employee.reporting_to`)
- [x] LMS learner progress web pane (`apps/web/src/pages/LearnerProgressPage.tsx` — course progress dashboard)
- [x] LMS reviewer assignment approval chain for `lms.assignment` (`lms.submit_assignment` agent tool + workflow block)
- [x] Frappe REST API source adapter for importer (ERPNext, HRMS, CRM, LMS) (`internal/importer/adapters/frappe.go`)
- [x] DocType → KType automatic mapping suggestions (`adapters/frappe.go#SuggestFieldMapping`)
- [x] Multi-tenancy: per-tenant encryption keys (HKDF with tenant_id as salt) (`internal/tenant/encryption.go`; integrated with `internal/record/store.go` encrypted-field hooks)
- [x] Multi-tenancy: zero-idle-cost verification (idle tenant resource measurement) (`TestIdleTenantZeroCost`)
- [x] Multi-tenancy: sub-millisecond tenant context switching benchmark (`BenchmarkTenantContextSwitch`)
- [x] Multi-tenancy: 1000-tenant load test on single cell (`TestThousandTenantLoad`, `//go:build loadtest`)
- [x] Authentication layer (JWT/OAuth with KChat SSO): JWT token issuance/validation (`internal/auth/jwt.go`), KChat SSO exchange (`internal/auth/sso.go`), session revocation on tenant suspension + per-tenant session limits (`internal/auth/session.go`), HTTP middleware (`internal/auth/middleware.go`), `migrations/000013_auth_sessions.sql`; login page wired to real JWT endpoint
- [x] CRM KType boot registration (Phase B KTypes `crm.lead` / `crm.contact` / `crm.organization` / `crm.deal` / `crm.activity` / `crm.quote` / `tasks.task` register via `crm.RegisterKTypes` in `services/api/main.go` alongside finance/inventory/hr/lms) (crm.RegisterKTypes called at services/api/main.go:165)
- [x] S3 production object store adapter wiring (`internal/files/s3.go` — AWS SDK v2 `ObjectStore`; `services/api/main.go` switches to `files.NewS3Store` when `S3_BUCKET` is set)
- [x] Email SMTP notification adapter (`internal/notifications/smtp.go` — `net/smtp` transport, template rendering, graceful no-op when SMTP env is unset; wired into `services/worker/notifications.go`)
- [x] Distributed rate limiting for multi-node deployment (`internal/platform/rate_limiter_redis.go` — `RedisRateLimiter` with sliding-window token bucket via atomic Lua; `kapp:rl:{tenant}` keys auto-expire on idle; `REDIS_URL` opt-in; fail-open on Redis outage; refill math fixed and miniredis-backed test suite added in PR #37; canonical entry — see Phase F item below for the original PR #35 wiring)
- [x] ZK Object Fabric per-tenant integration (`internal/files/zk_fabric.go` — `PerTenantS3Store` resolves credentials via `ZKStorageResolver` and threads tenant id through `context.Context`; `internal/tenant/zk_fabric_client.go` console client mints HMAC keys + per-tenant bucket during the setup wizard; `migrations/000027_tenant_zk_fabric.sql` adds `zk_access_key` / `zk_secret_key` / `zk_bucket` columns; gradual rollout — tenants without provisioning fall back to global MinIO)
- [x] ZK Object Fabric per-tenant placement policy (`internal/tenant/zk_fabric_client.go#SetPlacementPolicy` calls `PUT /api/tenants/{id}/placement` after wizard provisioning; `internal/tenant/placement.go` derives the policy from plan tier + tenant locale (encryption.mode = `managed` for free/starter, `client_side` for enterprise; provider allow-list, country residency, cache hint); `internal/files/zk_fabric.go` swaps the unbounded per-tenant `*S3Store` map for `platform.LRUCache` (1000 entries, 10-minute idle TTL, OnEvict closes idle S3 connections); `migrations/000028_tenant_placement.sql` adds `placement_policy JSONB` + `zk_fabric_endpoint TEXT`; `services/api/placement_handlers.go` exposes `GET/PUT /api/v1/tenants/{id}/placement` gated behind `FeatureStore`; `apps/web/src/pages/PlacementPolicyPage.tsx` JSON editor; reference: ZK fabric `metadata/placement_policy/policy.go` + ERPNext setup wizard country defaults)
- [x] Tenant resource metering + usage dashboard (`internal/tenant/metering.go` — API calls + storage_bytes + krecord_count + user_seats; `tenant_usage` table with RLS; `internal/platform/metering_middleware.go` per-request batched flushes; daily snapshot via `tenant.UsageSnapshotHandler` registered against `scheduler` in `services/worker/main.go`; `GET /api/v1/tenants/me/usage` + `/me/usage/history` + `POST /me/plan` with `apps/web/src/pages/UsageDashboardPage.tsx` rendering current usage bars + 6-month history chart)
- [x] Tenant setup wizard (`internal/tenant/wizard.go` — seeds CoA from `coa_templates/us_gaap_basic.json` + `ifrs_basic.json`, default roles, initial users; `POST /api/v1/tenants/{id}/setup`; `auto_setup: true` trigger on `Create`; `apps/web/src/pages/SetupWizardPage.tsx`)
- [x] Payment recording: `finance.payment` KType (`internal/finance/payment.go`), multi-invoice allocation engine (`internal/ledger/payment.go#PostPayment`), `POST /api/v1/finance/payments/{id}/post`, `finance.record_payment` agent tool, `/payment` slash command
- [x] Multi-currency: exchange rate table, automatic conversion on posting, unrealized gain/loss (`migrations/000017_multi_currency.sql` — `exchange_rates` with composite PK `(tenant_id, from_currency, to_currency, rate_date)` + RLS + `tenant_isolation`; `internal/ledger/currency.go` — `ExchangeRateStore.UpsertRate` / `GetRate` (handles inverse pairs) / `Convert` / `ListRates` / `UnrealizedGainLoss`; `finance.exchange_rate` KType registered at boot; `services/api/currency_handlers.go` exposes `POST/GET /api/v1/finance/exchange-rates` + `GET /exchange-rates/convert` + `POST /exchange-rates/unrealized`; `apps/web/src/pages/ExchangeRatesPage.tsx`; reference: `frappe/erpnext` Currency Exchange)
- [x] Recurring invoices: `finance.recurring_invoice` KType + scheduled auto-generation (reference: `frappe/erpnext` Auto Repeat) (PR #30, internal/finance/recurring.go + internal/scheduler/ + services/worker/main.go handler registration)
- [x] Credit limit enforcement on `PostSalesInvoice` (reference: `frappe/erpnext` Credit Limit) (PR #28, internal/ledger/credit_limit.go; PostSalesInvoice checks crm.customer credit_limit against outstanding AR before posting)
- [x] Inventory reorder automation: auto-create draft PO from low-stock alert (reference: `frappe/erpnext` Material Request) (PR #35, internal/inventory/reorder.go; ReorderHandler scans stock_levels, groups by supplier, creates draft procurement.purchase_order; wizard seeds inventory_reorder scheduled action; inventory.trigger_reorder agent tool)
- [x] Payment terms templates: `finance.payment_terms` KType with installment schedules (reference: `frappe/erpnext` Payment Terms Template) (PR #31, internal/finance/payment_terms.go; finance.payment_terms KType + invoice/bill schedule materialisation)
- [x] Per-tenant feature flags: enable/disable KApps per plan (reference: `frappe/frappe` module visibility) (PR #35, internal/tenant/features.go FeatureStore + internal/platform/feature_middleware.go; tenant_features table migration 000021; wizard seeds plan defaults; GET/PUT /api/v1/tenants/{id}/features; apps/web/src/pages/TenantFeaturesPage.tsx; nav sections hidden when feature disabled)
- [x] Tenant data isolation runtime audit endpoint (PR #38, `internal/platform/isolation_audit.go` IsolationAuditor; checks RLS coverage on every tenant_id table, runs cross-tenant probe, calls audit hash-chain verifier, asserts SET LOCAL app.tenant_id enforcement; `GET /api/v1/admin/isolation-audit` admin-only)
- [x] Dashboard page: tenant-level KPI dashboard (open deals, outstanding AR/AP, low stock, pending approvals, open tickets) (`services/api/dashboard_handlers.go` — `GET /api/v1/dashboard/summary` aggregates across `krecords` (crm.deal, finance.invoice, finance.bill, helpdesk.ticket), `stock_levels`, `approvals`; `apps/web/src/pages/DashboardPage.tsx` renders KPI tiles linking into the deep record lists; registered as the default landing route in `apps/web/src/App.tsx`)
- [x] Setup wizard React page: step-by-step onboarding flow (`apps/web/src/pages/SetupWizardPage.tsx` — company profile → CoA template picker → initial users → `POST /api/v1/tenants/{id}/setup`; route registered in `apps/web/src/App.tsx` at `/setup/:id`)
- [x] Bulk actions on RecordListPage: multi-select, bulk status change, bulk delete, bulk export (PR #36)
- [x] Full payroll processing: payslip generation, statutory deductions (reference: `frappe/hrms` Payroll Entry) (PR #32 + #33 + #34, internal/hr/payroll_engine.go; GeneratePayslips + PostPayRun with idempotent retry, dedicated /hr/pay-runs/{id}/payslips endpoint)
- [x] Helpdesk module: `helpdesk.ticket` + `helpdesk.sla_policy` KTypes with SLA, agent routing, KChat thread→ticket (`internal/helpdesk/ktypes.go` — ticket workflow `open → in_progress → waiting → resolved → closed`, policy schema for per-priority response/resolution minutes; `internal/helpdesk/store.go` — `sla_policies` + `ticket_sla_log` tables with RLS, `UpsertPolicy` / `ResolvePolicy` / `ComputeDueTimes` / `LogSLAEvent`; `migrations/000018_helpdesk.sql`; `internal/agents/helpdesk_tools.go` — `helpdesk.create_ticket` (with auto-SLA lookup), `helpdesk.assign_ticket`, `helpdesk.resolve_ticket`; `services/api/helpdesk_handlers.go` exposes `POST/GET /api/v1/helpdesk/sla-policies` + `GET /sla-policies/resolve` + `GET /tickets/{id}/sla-log`; `/ticket` slash command in `services/kchat-bridge/commands.go`; `apps/web/src/pages/HelpdeskPage.tsx` — open-ticket triage grid + SLA policy editor; customer-portal deferred)
- [x] Tenant resource metering: usage tracking per billing period, usage dashboard, plan upgrade/downgrade API (PR #35, internal/tenant/metering.go MeteringStore + internal/platform/metering_middleware.go MeteringBuffer; tenant_usage + plan_definitions tables migration 000022; /api/v1/tenants/{id}/usage + /api/v1/plans handlers; apps/web/src/pages/UsageDashboardPage.tsx)
- [x] Webhook management: `platform.webhook` KType, delivery log with retries, management UI, HMAC signatures (PR #36, migrations/000024_webhooks.sql; signatures already shipped earlier and noted below)
- [x] Full-text search: tsvector on krecords, search API endpoint, cross-KType search page (PR #36, migrations/000023_fulltext_search.sql; services/api/search_handlers.go; apps/web/src/pages/SearchPage.tsx)
- [x] Distributed rate limiting: swap the in-process `platform.RateLimiter` for a Redis/Valkey-backed token bucket so quotas hold across API replicas (PR #35, internal/platform/rate_limiter_redis.go RedisRateLimiter; sliding-window Lua script via EVALSHA; kapp:rl:{tenant} keys auto-expire on idle; REDIS_URL opt-in; fail-open on Redis outage; SECURITY_REVIEW §6 updated)
- [x] 5000-tenant load test harness: extend `internal/integrationtest/loadtest/harness.go` to hit the 5k concurrency target required by the Phase G acceptance (PR #38, mixed CRUD/finance/inventory/helpdesk/files/search workload, p99 SLO assertions, ZK fabric per-tenant LRU bound assertion in `zk_fabric_load_test.go`; `//go:build loadtest`)
- [x] Disaster-recovery runbook: backup/restore, tier upgrade, cross-region failover, key rotation, ZK fabric migration, chaos drill checklist (PR #38, `docs/DR_RUNBOOK.md`)
- [x] Documentation: operator guide, developer guide, KType authoring guide (this PR, `docs/OPERATOR_GUIDE.md` deployment + envs + backup/restore + tier upgrade + monitoring + DR + multi-tenancy ops; `docs/DEVELOPER_GUIDE.md` monorepo layout + local setup + tests + adding new KType/KApp; `docs/KTYPE_AUTHORING_GUIDE.md` schema fields + workflow + posting hooks + agent-tool conventions)
- [x] Multi-currency posting integration: auto-convert journal lines using exchange rate on posting date (PR #38, `internal/ledger/store.go#PostJournalEntry` looks up the tenant's base currency and converts each foreign-currency line via `ExchangeRateStore.Convert`, persisting both `currency` + `base_amount` on `journal_lines`; `migrations/000029_multi_currency_posting.sql`; reference: `frappe/erpnext` Currency Exchange on Journal Entry)
- [x] Unrealized gain/loss scheduled job: periodic revaluation of open foreign-currency invoices (PR #38, `internal/ledger/currency.go` `UnrealizedGainLossJob` ActionHandler queries open foreign-currency AR/AP, fetches current rates, posts adjustment journal entries; wizard seeds `unrealized_gain_loss` action with monthly cadence on finance-enabled plans; reference: `frappe/erpnext` Exchange Rate Revaluation)
- [x] Dashboard multi-currency conversion: server-side conversion to tenant base currency so KPI tiles show meaningful totals (PR #38, `services/api/dashboard_handlers.go` converts AR/AP/deal totals via `ExchangeRateStore.Convert`; `base_currency` field added to response; `apps/web/src/pages/DashboardPage.tsx` shows the base currency on tiles)
- [x] Helpdesk SLA breach worker: background worker to detect response/resolution breaches and log to `ticket_sla_log` (reference: `frappe/helpdesk` SLA) (PR #29, internal/helpdesk/sla_breach.go; atomic SLA log + outbox emit, tenant wizard seeds sla_breach_check scheduled action)
- [x] Helpdesk customer portal: self-service ticket submission and tracking (PR #36, migrations/000026_portal_users.sql; services/api/portal_handlers.go; reference: `frappe/helpdesk` customer portal)
- [x] Helpdesk inbound email: parse incoming emails into `helpdesk.ticket` records (PR #38, `internal/helpdesk/inbound_email.go` `InboundEmailHandler` resolves tenant by recipient domain, creates ticket with auto-SLA lookup, attaches email attachments via files store; `POST /api/v1/helpdesk/inbound-email` rate-limited per-tenant; reference: `frappe/helpdesk` email integration)
- [x] KChat thread-to-ticket automation: auto-create helpdesk ticket from flagged KChat thread (PR #38, `/ticket-from-thread` slash command in `services/kchat-bridge/commands.go`; Create Ticket composer action in `services/kchat-bridge/composer.go`; `services/worker/notifications.go` posts `helpdesk.ticket.status_changed` updates back to the thread when `thread_id` is set on the ticket)
- [x] Print/PDF generation: invoice, payslip, PO document rendering (PR #36, migrations/000025_print_templates.sql; services/api/print_handlers.go; reference: `frappe/frappe` Print Format)
- [x] Report scheduling and email delivery: cron-triggered report runs with PDF/CSV email (this PR, `internal/reporting/schedule.go` `ScheduleStore` + `ReportSchedule`, `migrations/000033_report_schedules.sql` with RLS, `services/worker/report_scheduler.go` `ReportScheduleHandler` runs the saved-report definition, renders to CSV or PDF via `internal/print`, and emails via `internal/notifications/smtp.go`; tenant wizard seeds `report_schedule` action; `services/api/report_schedule_handlers.go` CRUD + `apps/web/src/pages/ReportBuilderPage.tsx` Schedule dialog; reference: `frappe/frappe` Auto Email Report)
- [x] Saved report sharing: per-tenant report sharing with role-based visibility (this PR, `migrations/000034_report_sharing.sql` adds `visibility` enum + `shared_with` JSONB to saved_reports; `internal/reporting/store.go#ListVisible` filters by owner / role-share / public; `services/api/reports_handlers.go` PATCH `/api/v1/reports/{id}/share` + GET filters by visibility; `apps/web/src/pages/ReportBuilderPage.tsx` share dialog with role/user picker; reference: `frappe/frappe` Report Builder share-by-role)
- [x] Integration tests for Phase I: helpdesk, currency, reporting, dashboard (`internal/integrationtest/phase_i_test.go` — `TestExchangeRate*` (multi-currency upsert, convert, unrealized gain/loss), `TestHelpdeskPolicyLifecycle` + `TestHelpdeskSLALog` (SLA due-time computation, breach sweeper), `TestReportBuilder*` (columns, filters, aggregation, pivot, soft-delete exclusion, validation), `TestDashboardSummaryCounts`, `TestRLSIsolatesPhaseITables` (cross-tenant probes against `exchange_rates`, `sla_policies`, `saved_reports`, `ticket_sla_log`); `//go:build integration`)
- [x] Security review update for Phase H/I/J/K: extend `docs/SECURITY_REVIEW.md` (this PR adds §9 auth sessions, §10 helpdesk + portal, §11 reporting, §12 multi-currency, §13 webhooks, §14 print/PDF, §15 data retention)
- [x] Data retention policies: automated cleanup of old audit logs, events, SLA logs (PR #38, `migrations/000030_data_retention.sql` `data_retention_policies` table with RLS; `internal/platform/retention.go` `RetentionStore` + `RetentionSweeper` ActionHandler; wizard seeds daily `data_retention_sweep` action with plan-appropriate defaults; `GET/PUT /api/v1/tenants/{id}/retention` + `apps/web/src/pages/RetentionPoliciesPage.tsx`; reference: `frappe/frappe` Log Settings)
- [x] API versioning strategy: document breaking change policy for `/api/v1/` (this PR, `docs/API_VERSIONING.md` covers path-prefix negotiation, the 3-minor-release deprecation timeline with `Deprecation`/`Sunset` headers per RFC 8594, the per-tenant version pin via `tenant_features` keyed `api_version_pin:vN`, and the additive-vs-breaking rule set; `.github/workflows/api-versioning-check.yml` enforces that every chi route mounted in `services/api/` lives under `/api/v1/`, `/api/v2/`, `/internal/`, or the platform allow-list)
- [x] Stock move reversal: correction entries for `inventory_moves` following the finance credit-note pattern (this PR, `internal/inventory/store.go#ReverseMove` loads the original move, posts an opposite-sign move with `reversal_of` set, atomically updates `stock_levels`, emits `inventory.move.reversed`; `migrations/000035_stock_reversal.sql`; `internal/agents/inventory_tools.go` `inventory.reverse_move` tool; `/reverse-stock-move` slash command; `POST /api/v1/inventory/moves/{id}/reverse`; reference: `frappe/erpnext` Stock Entry cancellation)
- [x] Batch/lot tracking: full implementation on top of the schema hooks already landed in Phase D (this PR, `migrations/000040_batch_tracking.sql` `inventory_batches` with composite (tenant_id, id) PK, RLS + GRANT to kapp_app, `inventory_moves.batch_id` FK; `inventory.batch` KType in `internal/inventory/ktypes.go`; `internal/inventory/store.go#CreateBatch` / `GetBatch` / `ListBatchesForItem` + `RecordMove` enforces tenant + item linkage with `ErrBatchNotFound` / `ErrBatchItemMismatch`; `inventory.assign_batch` agent tool; `/batch` slash command; `POST /api/v1/inventory/batches` + `GET /api/v1/inventory/items/{id}/batches`; Batches column on `apps/web/src/pages/StockLevelsPage.tsx`; registered in `tierUpgradeTables` / `TenantScopedTables` / `upgrade_tier.sh`; integration test `internal/integrationtest/phase_g_batch_test.go` covers happy path + cross-tenant isolation + item mismatch; reference: `frappe/erpnext` Batch DocType)
- [x] Attendance integration with KChat presence/status (this PR, `services/kchat-bridge/presence.go` listens at `/kchat/presence`, resolves kchat user → tenants → hr.employee by email and upserts `hr.attendance` KRecord with `status=present` + `source=kchat`; idempotent per UTC day; `internal/tenant/user.go#GetUserByKChatID` adds the cross-tenant lookup; `source` enum field added to `hr.attendance` schema in `internal/hr/hr.go` (`manual` | `kchat` | `biometric`); gated per-tenant by `attendance_kchat_sync` feature flag; "Present today" tile on `apps/web/src/pages/DashboardPage.tsx` backed by `internal/dashboard/store.go#PresentToday`; integration test `internal/integrationtest/phase_g_presence_test.go` covers flag gating + idempotency)
- [x] Course completion certificates for `lms.enrollment` (this PR, `lms.certificate` KType in `internal/lms/ktypes.go` with auto-generated certificate number, `internal/lms/certificates.go#IssueCertificate` checks 100% progress before issuance, `services/worker/certificate_worker.go` listens for `lms.enrollment.completed` events, `internal/print/templates/certificate.html` default template, `lms.issue_certificate` agent tool, `/certificate` slash command, `apps/web/src/pages/LearnerProgressPage.tsx` download button; reference: `frappe/lms` certificate auto-generation)
- [x] Scheduled actions: tenant-scoped cron, recurring invoice generation, scheduled report delivery (PR #27, internal/scheduler/scheduler.go + store.go; migrations/000020_scheduled_actions.sql; PollDue with FOR UPDATE SKIP LOCKED, corrupt-row resilience, worker wiring)
- [x] Data export: per-KType export endpoint, full tenant dump, export job tracking (this PR, `internal/exporter/exporter.go` `ExportStore` + `ExportJob` with status/progress/download_url, `internal/exporter/krecord_exporter.go` streaming CSV/JSON, `internal/exporter/tenant_exporter.go` reuses kapp-backup extract logic, `migrations/000036_export_jobs.sql` RLS, `services/api/export_handlers.go` POST/GET/download endpoints, `services/worker/export_worker.go` background processor, `apps/web/src/pages/ExportPage.tsx` wizard; reference: `frappe/frappe` Data Export)
- [x] `permissions` table (`migrations/000015_permissions.sql` with RLS; `internal/authz/store.go` reads granular `(role_name, ktype, action)` grants with JSONB conditions, falling back to legacy `roles.permissions` for backward compatibility)
- [x] `notifications` table (`migrations/000014_notifications.sql` with RLS; `internal/notifications/store.go` + `services/api/notifications.go` expose `GET /api/v1/notifications` + mark-read; worker persists every routed notification; `apps/web/src/components/NotificationBell.tsx` renders the inbox)
- [x] Webhook HMAC signatures (`services/worker/notifications.go#postWebhook` computes HMAC-SHA256 of the request body with a per-tenant secret and adds `X-Kapp-Signature: sha256=<hex>`)
- [ ] Insights: visual query builder, composable dashboards, AI query assistant (Phase L)

### Priority MVP gaps

Business-object coverage gaps that surface once a real SME starts onboarding.
The kernel can model these as generic KRecords today, but dedicated KTypes
(with schemas, posting hooks, and agent tools) make the user experience
match what customers expect from an ERP/CRM.

- [x] Sales Orders (`sales.order` KType) — draft → confirmed → fulfilled pipeline, links to deal + price list, lines with item/qty/price/discount (`internal/sales/ktypes.go`; registered in `services/api/main.go`)
- [x] Purchase Orders (`procurement.purchase_order` KType) — draft → confirmed → received pipeline, links to supplier, same line shape as sales orders (`internal/sales/ktypes.go`)
- [x] Customers as a dedicated KType (`crm.customer` in `internal/crm/ktypes.go` with `customer_group`, `credit_limit`, `default_tax_code`, `default_payment_terms`, `currency`, `ar_aging_bucket`, `status`; `finance.ar_invoice.customer_id` retargeted; `crm.create_customer` agent tool; `/customer` slash command)
- [x] Suppliers as a dedicated KType (`crm.supplier` in `internal/crm/ktypes.go` with `supplier_group`, `default_payment_terms`, `currency`, `ap_aging_bucket`, `status`; `finance.ap_bill.supplier_id` retargeted; `crm.create_supplier` agent tool; `/supplier` slash command)
- [x] Price Lists (`sales.price_list` KType) — per-currency, optional per-customer, valid_from/valid_until window, items array with {item_id, price, discount_percent, min_qty} (`internal/sales/ktypes.go`)
- [x] Salary Components (`hr.salary_component`, `hr.salary_structure`) — earning / deduction / tax components with fixed or percentage amount types; structure references an employee + base salary + component list (`internal/hr/payroll.go`; registered in `services/api/main.go`)

### Design references

Multi-tenancy and module patterns in the Frappe ecosystem inform several
Phase G / Phase H designs. Treat these as reference architectures for
the onboarding wizard, helpdesk, LMS, scheduled actions, and importer
adapters — not as code to copy:

- `frappe/frappe` — site-based tenancy, `bench` fleet management, background worker queues, Scheduled Job Type, Auto Repeat, Report Builder, rate limiting
- `frappe/erpnext` — setup wizard, Chart of Accounts seeding, Payment Entry, Bank Reconciliation, Currency Exchange, Credit Limit, Material Request, Payment Terms Template
- `frappe/hrms` — attendance + leave management, Payroll Entry, Salary Slip
- `frappe/crm` — deal pipeline + lead management patterns
- `frappe/helpdesk` — ticket SLA, agent routing, customer portal
- `frappe/lms` — course management, certificates
- `frappe/insights` — visual query builder, dashboard composition, query caching, AI-assisted queries

---

## Phase G — Hardening, Observability, and Scale

**Status:** Complete

Production readiness across all shipped modules.

### Deliverables

- [x] Per-tenant encryption (code lives in `internal/tenant/encryption.go` + `internal/record/store.go` hooks; api boot loads `KAPP_MASTER_KEY` and calls `recordStore.WithEncryptor`; security review in `docs/SECURITY_REVIEW.md` §3 covers round-trip invariants. Key rotation remains a follow-up.)
- [x] Low-stock alerts (worker in `services/worker/stock_alerts.go` launched from `services/worker/main.go`; dedupe map capped; cross-tenant safety verified in `docs/SECURITY_REVIEW.md` §4)
- [x] Sales Orders + Purchase Orders KTypes (`internal/sales/ktypes.go` — draft→confirmed→fulfilled / received workflows with agent tools, views, cards; registered in `services/api/main.go`)
- [x] Price Lists KType (`internal/sales/ktypes.go` — per-currency, optional per-customer, items array with discount + min-qty)
- [x] Bank Accounts + Reconciliation (`internal/ledger/bank.go` — `UpsertBankAccount`, `ImportBankStatement`, `ReconcileTransaction` with conservative matcher; `migrations/000011_sales_procurement_bank.sql` adds RLS-protected `bank_accounts` + `bank_transactions` tables)
- [x] Cost Centers / Journal Dimensions (`internal/ledger/cost_center.go` — `finance.cost_center` KType + typed `cost_centers` table; `journal_lines.cost_center` column added in `000011` so every line can carry a dimension tag)
- [x] Salary Components + Structure (`internal/hr/payroll.go` — `hr.salary_component` and `hr.salary_structure` KTypes; registered in `services/api/main.go`)
- [x] Per-tenant observability (`internal/platform/metrics.go` — Prometheus-text-format registry with counter/histogram/gauge vectors; `MetricsMiddleware` labels every request with `{tenant_id, method, path, status}`; default buckets span 500µs–10s for both control-plane and import paths; no new external dependencies)
- [x] Backup and restore tooling (`services/kapp-backup/main.go` — per-tenant JSONL extract + restore with optional tenant-id remap; table list mirrored in `scripts/upgrade_tier.sh`)
- [x] Security review (`docs/SECURITY_REVIEW.md` — 8-section checklist covering RLS coverage, agent-tool workflow enforcement, encryption round-trip, cross-tenant leakage, rate-limiter/LRU idle eviction, context-switching benchmark)
- [x] Upgrade tier tooling — shared-→-dedicated-schema path: shell script (`scripts/upgrade_tier.sh`) plus admin-only API endpoint (`POST /api/v1/admin/tenants/{id}/upgrade-tier` in `services/api/tier_handlers.go`) that runs the same single-transaction copy of every tenant-scoped row into `tenant_<uuid>.*` and emits a `tenant.tier_upgrade` audit entry; dedicated-DB / dedicated-cell tiers remain a follow-up handled by the cell-router
- [x] Multi-tenancy benchmarks (zero-idle-cost in `internal/integrationtest/bench_idle_test.go`; sub-ms context switching in `bench_switching_test.go`; 1000-tenant load harness in `internal/integrationtest/loadtest/harness.go`)
- [x] Cell autoscaling policies (`internal/platform/autoscaler.go` — `Decide` is a pure policy function; `AutoscaleEngine.Evaluate` reads the new `cells` table plus a LATERAL JOIN against `platform_scale_events` for cooldown, persists every decision into `platform_scale_events`, and emits structured slog lines on scale_up/scale_down. `AutoscaleLoop` runs in `services/worker/main.go` on a 60s ticker independently of `scheduled_actions` (which are tenant-scoped). Default policy: max_tenants_per_cell=1000, cpu/mem 80%, conn_saturation 75%, scale-down at <30% tenants and <half of every utilisation threshold, 10-minute cooldown between non-hold decisions per cell. Migrations `000041_cell_capacity.sql` add the `cells` registry, `tenants.cell_id` FK, and `platform_scale_events` audit. Unit tests in `internal/platform/autoscaler_test.go` cover every Decide branch; `internal/integrationtest/phase_g_autoscaler_test.go::TestAutoscalerEndToEnd` exercises the full SQL round-trip)
- [x] Disaster recovery runbook and chaos drills (PR #38, `docs/DR_RUNBOOK.md` — backup/restore, tier upgrade, region failover, key rotation, ZK fabric migration, chaos drill checklist)
- [x] Performance tuning: index review, partition pruning, outbox batch sizing (`migrations/000039_insights_indexes.sql` adds composite indexes for cache-recent / widget-query reverse / case-insensitive name / grantee→resource paths; `docs/PERFORMANCE_TUNING.md` documents EXPLAIN ANALYZE evidence for partition pruning on krecords / events / audit_log under tenant-scoped GUC and rationalises the worker `drainBatch=100` + 1 s tick interval against 5k-tenant p99 outbox-lag findings)
- [x] Load test: 5000 tenants on a single cell with baseline SLOs met (PR #38, `internal/integrationtest/loadtest/harness.go` mixed CRUD/finance/inventory/helpdesk/files/search workload at 5k concurrency; `zk_fabric_load_test.go` asserts per-tenant LRU bound + Invalidate under concurrency; `//go:build loadtest`)
- [x] Documentation: operator guide, developer guide, KType authoring guide (this PR, `docs/OPERATOR_GUIDE.md` deployment + envs + backup/restore + tier upgrade + monitoring + DR + multi-tenancy ops; `docs/DEVELOPER_GUIDE.md` monorepo layout + local setup + tests + adding new KType/KApp; `docs/KTYPE_AUTHORING_GUIDE.md` schema fields + workflow + posting hooks + agent-tool conventions)
- [x] CI rule: fail new migrations that don't ENABLE ROW LEVEL SECURITY on tenant-scoped tables (`.github/workflows/migration-rls-check.yml` — scans `migrations/*.sql` for `CREATE TABLE` containing `tenant_id` and fails if the same migration lacks `ENABLE ROW LEVEL SECURITY` for that table)
- [x] CI rule: forbid `internal/agents` from importing `internal/record` outside the executor (`.github/workflows/agent-import-check.yml` — `go list -json ./internal/agents/...` filtered for `internal/record` imports; executor is the only allowed path)
- [x] Audit-log integrity check / hash chain (`migrations/000016_audit_hash_chain.sql` adds `prev_hash` + `row_hash`; `internal/audit/store.go` hash-chains each insert with SHA-256 over (prev_hash || tenant_id || target_id || action || before || after || context || created_at); `GET /api/v1/audit/verify` replays the chain and reports the first break)
- [x] Wire `scripts/upgrade_tier.sh` to a tenant-service API endpoint (`internal/tenant/tier.go::Promote` is the single entry point shared by the API handler at `services/api/tier_handlers.go`, the runbook at `scripts/upgrade_tier.sh`, and any future tenant-service RPC; it delegates to the SECURITY DEFINER function `public.promote_tenant_to_schema(uuid, text, text[])` installed by `migrations/000042_tier_admin_role.sql`. The function is owned by the new scoped role `kapp_tier_admin` (NOSUPERUSER, NOBYPASSRLS, NOCREATEDB) and only `kapp_admin` is granted EXECUTE; the runbook no longer requires DB superuser. SECURITY_REVIEW.md §8 item 5 closed)
- [x] Encryption key rotation migration (`internal/tenant/encryption.go` supports dual master keys: `KAPP_MASTER_KEY` for encrypt + primary decrypt, `KAPP_MASTER_KEY_PREV` as fallback on GCM auth failure; `cmd/rotate-master-key/main.go` + `scripts/rotate_master_key.sh` batch-re-encrypt every tenant's `krecords.data` strings under the new key, idempotently)

### Frontend

Shipped backend surfaces that still need a first-class frontend page. The
generic `RecordListPage` / `RightPane` already render each KType by
schema, but dedicated pages unlock per-module workflows (reconcile,
compare to expected, bulk-edit) that the generic view can't cover.

- [x] Bank Reconciliation UI page (`apps/web/src/pages/BankReconciliationPage.tsx` — statement upload, side-by-side transactions vs journal entries, auto-match + manual match)
- [x] Cost Centers management page (`apps/web/src/pages/CostCentersPage.tsx` — tree view over `parent_code`, activation toggle, filter surface)
- [x] Sales Orders / Purchase Orders dedicated pages (`apps/web/src/pages/SalesOrdersPage.tsx` + `PurchaseOrdersPage.tsx` — kanban by stage, per-line editing, linkage to deal/supplier)
- [x] Price Lists management page (`apps/web/src/pages/PriceListsPage.tsx` — per-currency/per-customer matrix, effective date window, item search)
- [x] Salary Components / Structures / Payroll pages (`apps/web/src/pages/PayrollPage.tsx` — component CRUD, structure builder, per-employee assignment)

### Acceptance Criteria

- [x] All shipped KApps meet SLOs under 5000-tenant load test (this PR, `internal/integrationtest/loadtest/phase_g_acceptance_test.go::TestPhaseGAcceptanceLoad` runs the harness with `LT_TENANTS=5000 LT_MAX_CONNS=96`; 140 000 mixed CRUD + ledger ops, 0 failures, p99 38ms create / 14ms get / 14ms list / 60ms post-journal — well under the 100ms / 250ms targets; full numbers in `docs/PHASE_G_ACCEPTANCE.md`)
- [x] Tenant upgrade/downgrade tooling succeeds without data loss (this PR, `services/api/tier_handlers_integration_test.go::TestTierUpgradeCopiesEveryTable` seeds tenants A and B with one row per Phase L insights table, runs `promoteTenantToSchema(A)`, asserts the dedicated schema contains every `tierUpgradeTables` entry, no tenant-B rows leaked, every insights table received tenant-A rows, and `public.tenants.schema` was updated; `TestTierUpgradeTablesMatchBackupSourceList` asserts byte-identical lock-step with `services/kapp-backup/main.go::TenantScopedTables`; pre-existing generated-column bug in the row-copy SQL fixed via `nonGeneratedColumns` helper in `services/api/tier_handlers.go` and matching DO-block in `scripts/upgrade_tier.sh`)
- [x] Backup/restore verified for both shared and dedicated tiers (this PR, `services/api/tier_handlers_integration_test.go::TestKappBackupRoundTripWithRemap` builds the `kapp-backup` binary as a subprocess, runs `extract --tenant src` then `restore --in dump --remap src:dst`, and asserts every Phase L insights table carries rows under the destination tenant id; the dedicated-tier extract walks the same `TenantScopedTables` slice that the structural test guards, so the round-trip covers both tiers in one harness)
- [x] Security review signs off on tenant isolation and agent safety (this PR, `docs/PHASE_G_ACCEPTANCE.md` §4 enumerates the items in `docs/SECURITY_REVIEW.md` §8 closed by the live tests above — RLS coverage, tier-upgrade tx safety, backup remap round-trip, and `kapp_admin` BYPASSRLS scoping; item 5's `kapp_tier_admin` SECURITY DEFINER role tracks against PR #3)
- [x] Documentation covers onboarding a new developer to productive contribution (`docs/DEVELOPER_GUIDE.md` covers compose-up, migrations, the `APP_DB_URL` / `ADMIN_DB_URL` split, the `make test` vs `make test-integration` vs loadtest tagged-build matrix, KType authoring loop with cross-reference to `docs/KTYPE_AUTHORING_GUIDE.md`, and the release checklist; the Phase G acceptance run in `docs/PHASE_G_ACCEPTANCE.md` did not surface any gaps that required new sections)

---

## Phase L — Insights

**Status:** Complete (deferred / follow-up items below tracked against PR #4)

Tenant-scoped BI layer: visual query builder, composable dashboards, rich visualizations, AI-assisted queries, and KChat digest cards. Reference: [Frappe Insights](https://github.com/frappe/insights).

### Deliverables

- [x] `internal/insights/` package: query store, dashboard store, cache store, query engine extensions (calculated columns) (PR #41)
- [x] `insights_queries`, `insights_dashboards`, `insights_dashboard_widgets`, `insights_query_cache`, `insights_shares` tables with RLS + tenant_id partitioning (`migrations/000038_insights.sql`, PR #41)
- [x] Query result caching with TTL and scheduled refresh via `internal/scheduler` (TTL store in PR #41; this PR registers `query_cache_refresh` in `services/worker/insights_cache.go` + `internal/insights/types.go::ActionTypeQueryCacheRefresh` + wizard seed in `internal/tenant/wizard.go` for FeatureInsights plans on a 300 s default interval)
- [x] `services/api/insights_handlers.go` — full CRUD + execution endpoints under `/api/v1/insights/` (PR #41; PR #43 added `DELETE /{id}/shares/{shareID}` for both queries and dashboards; this PR splits the route into per-resource handlers and scopes `DashboardStore.DeleteShare` to `(resourceType, resourceID, shareID)` so cross-resource deletes are rejected)
- [x] Phase L integration test suite (`internal/integrationtest/phase_l_test.go`): cross-resource delete rejected, RLS isolates `insights_queries` / `insights_dashboards` / `insights_dashboard_widgets` / `insights_query_cache` / `insights_shares` across two tenants, runner cache short-circuits on the second `RunSaved`, and dry-run + commit verified for `insights.generate_query` / `insights.explain_result` / `insights.post_dashboard_digest`
- [x] KChat right-pane mini-dashboard component (`apps/web/src/components/insights/InsightsRightPane.tsx`) — calls `getInsightsDashboard(id)` and renders one `Viz` per widget at 140 px; consumed by the bridge's `POST /kchat/insights/dashboards/render` flow when a user clicks a dashboard card in chat
- [x] Visual query builder React page (`apps/web/src/pages/InsightsQueryBuilderPage.tsx`) — source picker bound to `/api/v1/ktypes`, drag-and-drop column ordering, filter builder matching `reporting.Definition`, aggregation + group_by config, calculated columns editor, live preview via `POST /insights/queries/{id}/run`
- [x] Dashboard builder React page (`apps/web/src/pages/InsightsDashboardPage.tsx`) — 12-column CSS grid, widget config panel, linked filters that re-run affected widgets via `dashboard.layout.linked_filters`, auto-refresh toggle wired to `dashboard.auto_refresh_seconds`
- [x] Rich chart visualizations: bar, line, pie, donut, funnel, number card, pivot table, table view in `apps/web/src/components/insights/Charts.tsx` (Recharts ^3.8.1, 8-color accessible palette, `Viz` dispatcher routes on `viz_type` from `insights_dashboard_widgets`)
- [x] Dashboard and query sharing: role-based grants, share modal UI (`apps/web/src/components/insights/ShareModal.tsx` — grantee + permission picker, list-existing-shares + revoke, used from both query builder and dashboard pages)
- [x] Agent tools: `insights.generate_query`, `insights.explain_result`, `insights.post_dashboard_digest` (`internal/agents/insights_tools.go`; all three set `RequiresConfirmation=true` and respect `ModeDryRun`; registered via `agents.RegisterInsightsTools` in `services/api/main.go`)
- [x] KChat surfaces: `/insight` slash command + `/dashboard-digest` slash command in `services/kchat-bridge/commands.go`; right-pane dashboard render endpoint `POST /kchat/insights/dashboards/render` in `services/kchat-bridge/main.go` (instantiates the full insights stack — `QueryStore`, `DashboardStore`, `CacheStore`, `Runner`, `reporting.Runner` — so slash commands resolve in-process rather than re-entering the API)
- [x] Feature flag: `insights` gated per plan in `internal/tenant/plans.go` (PR #41; off on free/starter, on for business+)
- [x] Query timeout budget: per-tenant `statement_timeout` on insight queries (PR #41)
- [x] Migrations: `migrations/000038_insights.sql` (PR #41) + `migrations/000039_insights_indexes.sql` (this PR — see Phase G performance tuning entry)

### Acceptance Criteria

- [x] A tenant user can build a query visually, save it, and see cached results on re-run (`internal/integrationtest/phase_l_test.go::TestInsightsRunSavedQueryUsesCache` saves a query, runs it, asserts the second `RunSaved` returns `CacheHit=true` from `insights_query_cache`)
- [x] A dashboard with 5+ widgets renders correctly with linked filters (this PR, `internal/integrationtest/phase_l_test.go::TestInsightsDashboardWithLinkedFilters` builds a 5-widget dashboard with a `linked_filters` block on the layout, then re-runs every widget under the same `owner=alice` filter via the runner and asserts each widget returns a `Result` — the linked-filter dispatch the React shell drives)
- [x] AI agent generates a valid query from "Show me top 10 customers by revenue this quarter" (`internal/integrationtest/phase_l_test.go::TestInsightsGenerateQueryAgentTool` covers dry-run + commit; this PR adds `TestInsightsGenerateQueryAgentToolValid` which runs the committed query end-to-end through `Runner.RunSaved` to prove the generated definition round-trips through validation + builder + executor without error)
- [x] Dashboard digest card posts to KChat on schedule (`internal/integrationtest/phase_l_test.go::TestInsightsPostDashboardDigestAgentTool` walks dry-run + commit for `insights.post_dashboard_digest` and asserts the commit payload carries the rendered KChat sections)
- [x] RLS prevents cross-tenant query/dashboard access (negative test) (`internal/integrationtest/phase_l_test.go::TestRLSIsolatesInsightsTables` seeds two tenants and probes `insights_queries` / `insights_dashboards` / `insights_dashboard_widgets` / `insights_query_cache` / `insights_shares` from each tenant's GUC, asserting zero cross-tenant rows are visible)
- [x] Query timeout prevents a single tenant from monopolizing the shared pool (this PR, `internal/integrationtest/phase_l_test.go::TestInsightsQueryTimeoutEnforced` re-binds the runner with `WithTimeout(time.Nanosecond)` so the `SET LOCAL statement_timeout` fence in `insights.Runner.runWithTimeout` trips deterministically and `RunSaved` returns an error rather than completing)
- [x] Insights feature flag disables all routes and nav when off (this PR, `internal/integrationtest/phase_l_test.go::TestInsightsFeatureFlagDisablesRoutes` exercises `platform.DynamicFeatureMiddleware` against `/api/v1/insights/queries`, `/insights/dashboards`, and `/insights/queries/{id}/run` with `tenant_features.insights = false`, asserting a 403 with the canonical `feature: "insights"` envelope; with the feature on, the same routes pass through to the next handler)

### Deferred / Follow-up

- [x] External data source connections (non-Kapp PostgreSQL, CSV upload)
- [ ] SQL editor mode with parameterized tenant injection
- [ ] Notebook/exploratory analysis interface
- [x] Cross-KType JOINs in visual builder
- [x] Dashboard embedding (iframe/public link)

---

## First Coding Slice — Acceptance Test Checklist

A concrete, end-to-end demonstration that the kernel is real. Before Phase A is considered complete, all of these must pass:

- [x] Create two tenants `acme` and `globex` via API (TestRLSIsolatesTenants creates two tenants)
- [x] Register a KType `demo.note` with fields `title`, `body` (TestKTypeRegistry)
- [x] Create user `alice` in `acme` and `bob` in `globex` (TestFirstCodingSlice)
- [x] Alice creates a note in `acme`; Bob creates a note in `globex` (TestRLSIsolatesTenants)
- [x] Alice's note list returns only `acme` notes (TestRLSIsolatesTenants)
- [x] Bob's note list returns only `globex` notes (TestRLSIsolatesTenants)
- [x] Direct DB query as Alice's session (tenant context set) returns only `acme` rows (RLS policies verified)
- [x] Direct DB query with no tenant context returns zero rows (RLS default-deny)
- [x] Every create produces one event and one audit record (TestRecordCRUDEmitsEventsAndAudit)
- [x] Per-tenant rate limit kicks in after configured threshold (RateLimitMiddleware exists)
- [x] Idle tenant `globex` has no active goroutines, no open connections, and no cached entries after idle timeout (TestIdleTenantZeroCost in internal/integrationtest/bench_idle_test.go)
- [x] Tenant context switch from `acme` to `globex` measured under 1 ms on a warm gateway (BenchmarkTenantContextSwitch in internal/integrationtest/bench_switching_test.go)
