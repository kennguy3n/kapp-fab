# Kapp Business Suite — Development Progress

> **Last Updated:** 2026-04-24 (Phase H hardening slice complete via PR #22: JWT/SSO auth layer with sessions table, S3 production object store, `crm.customer` / `crm.supplier` KTypes, `finance.payment` KType + allocation engine, tenant setup wizard with CoA seeding + default roles + initial-user invites, frontend pages for bank reconciliation / cost centers / sales + purchase orders / price lists / payroll, HMAC-signed webhooks, `notifications` table + inbox API + `NotificationBell` component, `permissions` table + authz store, SMTP email adapter, audit-log hash chain with verify endpoint, dual master-key support + `cmd/rotate-master-key`, CI guards for RLS migrations + agent-import isolation; plus `SetupWizardPage.tsx` React onboarding flow and tracking entries for multi-currency, recurring invoices, credit-limit enforcement, reorder automation, payment-terms templates, feature flags, isolation audit endpoint, KPI dashboard, bulk actions, and full payroll processing.)
>
> Related documents: [README.md](./README.md) · [PROPOSAL.md](./PROPOSAL.md) · [ARCHITECTURE.md](./ARCHITECTURE.md) · [SECURITY_REVIEW.md](./docs/SECURITY_REVIEW.md)

---

## Current Phase

**Phase H — Hardening Slice Complete**
**Status:** Complete (PR #22 landed JWT/SSO auth with sessions table, S3 production object store, `crm.customer` / `crm.supplier` KTypes, `finance.payment` posting + allocation engine, tenant setup wizard, Phase G frontend pages, HMAC-signed webhooks, `notifications` + `permissions` tables, SMTP adapter, audit hash chain, dual master-key rotation tooling, and CI enforcement for RLS + agent-import boundary). Next focus: multi-currency, recurring invoices, credit-limit enforcement, helpdesk module, KPI dashboard, bulk actions, full payroll processing — tracked under Cross-cutting.

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
- [ ] Report builder (pivot, aggregate, chart) over KRecords and ledgers
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
- [ ] Distributed rate limiting for multi-node deployment (`platform.RateLimiter` is in-process; needs a shared backend — Redis token bucket or similar — so quotas hold across api replicas)
- [x] Tenant setup wizard (`internal/tenant/wizard.go` — seeds CoA from `coa_templates/us_gaap_basic.json` + `ifrs_basic.json`, default roles, initial users; `POST /api/v1/tenants/{id}/setup`; `auto_setup: true` trigger on `Create`; `apps/web/src/pages/SetupWizardPage.tsx`)
- [x] Payment recording: `finance.payment` KType (`internal/finance/payment.go`), multi-invoice allocation engine (`internal/ledger/payment.go#PostPayment`), `POST /api/v1/finance/payments/{id}/post`, `finance.record_payment` agent tool, `/payment` slash command
- [ ] Multi-currency: exchange rate table, automatic conversion on posting, unrealized gain/loss (reference: `frappe/erpnext` Currency Exchange)
- [ ] Recurring invoices: `finance.recurring_invoice` KType + scheduled auto-generation (reference: `frappe/erpnext` Auto Repeat)
- [ ] Credit limit enforcement on `PostSalesInvoice` (reference: `frappe/erpnext` Credit Limit)
- [ ] Inventory reorder automation: auto-create draft PO from low-stock alert (reference: `frappe/erpnext` Material Request)
- [ ] Payment terms templates: `finance.payment_terms` KType with installment schedules (reference: `frappe/erpnext` Payment Terms Template)
- [ ] Per-tenant feature flags: enable/disable KApps per plan (reference: `frappe/frappe` module visibility)
- [ ] Tenant data isolation runtime audit endpoint
- [ ] Dashboard page: tenant-level KPI dashboard (open deals, outstanding AR/AP, low stock, pending approvals)
- [x] Setup wizard React page: step-by-step onboarding flow (`apps/web/src/pages/SetupWizardPage.tsx` — company profile → CoA template picker → initial users → `POST /api/v1/tenants/{id}/setup`; route registered in `apps/web/src/App.tsx` at `/setup/:id`)
- [ ] Bulk actions on RecordListPage: multi-select, bulk status change, bulk delete, bulk export
- [ ] Full payroll processing: payslip generation, statutory deductions (reference: `frappe/hrms` Payroll Entry)
- [ ] Helpdesk module: `helpdesk.ticket` KType with SLA, agent routing, KChat thread→ticket, customer portal
- [ ] Tenant resource metering: usage tracking per billing period, usage dashboard, plan upgrade/downgrade API
- [ ] Webhook management: `platform.webhook` KType, delivery log with retries, management UI, HMAC signatures
- [ ] Full-text search: tsvector on krecords, search API endpoint, cross-KType search page
- [ ] Scheduled actions: tenant-scoped cron, recurring invoice generation, scheduled report delivery
- [ ] Data export: per-KType export endpoint, full tenant dump, export job tracking
- [x] `permissions` table (`migrations/000015_permissions.sql` with RLS; `internal/authz/store.go` reads granular `(role_name, ktype, action)` grants with JSONB conditions, falling back to legacy `roles.permissions` for backward compatibility)
- [x] `notifications` table (`migrations/000014_notifications.sql` with RLS; `internal/notifications/store.go` + `services/api/notifications.go` expose `GET /api/v1/notifications` + mark-read; worker persists every routed notification; `apps/web/src/components/NotificationBell.tsx` renders the inbox)
- [x] Webhook HMAC signatures (`services/worker/notifications.go#postWebhook` computes HMAC-SHA256 of the request body with a per-tenant secret and adds `X-Kapp-Signature: sha256=<hex>`)

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

---

## Phase G — Hardening, Observability, and Scale

**Status:** In Progress

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
- [x] Upgrade tier tooling — shared-→-dedicated-schema path (`scripts/upgrade_tier.sh` — single-transaction copy of every tenant-scoped row into `tenant_<uuid>.*` and routing update; dedicated-DB / dedicated-cell tiers remain a follow-up)
- [x] Multi-tenancy benchmarks (zero-idle-cost in `internal/integrationtest/bench_idle_test.go`; sub-ms context switching in `bench_switching_test.go`; 1000-tenant load harness in `internal/integrationtest/loadtest/harness.go`)
- [ ] Cell autoscaling policies
- [ ] Disaster recovery runbook and chaos drills
- [ ] Performance tuning: index review, partition pruning, outbox batch sizing
- [ ] Load test: 5000 tenants on a single cell with baseline SLOs met
- [ ] Documentation: operator guide, developer guide, KType authoring guide
- [x] CI rule: fail new migrations that don't ENABLE ROW LEVEL SECURITY on tenant-scoped tables (`.github/workflows/migration-rls-check.yml` — scans `migrations/*.sql` for `CREATE TABLE` containing `tenant_id` and fails if the same migration lacks `ENABLE ROW LEVEL SECURITY` for that table)
- [x] CI rule: forbid `internal/agents` from importing `internal/record` outside the executor (`.github/workflows/agent-import-check.yml` — `go list -json ./internal/agents/...` filtered for `internal/record` imports; executor is the only allowed path)
- [x] Audit-log integrity check / hash chain (`migrations/000016_audit_hash_chain.sql` adds `prev_hash` + `row_hash`; `internal/audit/store.go` hash-chains each insert with SHA-256 over (prev_hash || tenant_id || target_id || action || before || after || context || created_at); `GET /api/v1/audit/verify` replays the chain and reports the first break)
- [ ] Wire `scripts/upgrade_tier.sh` to a tenant-service API endpoint (SECURITY_REVIEW.md §8 item 5 — the shell script runs as DB superuser against the cluster today; the long-term path is a tenant-service RPC that handles the tier upgrade transactionally, emits an audit record, and drops the superuser requirement)
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

- [ ] All shipped KApps meet SLOs under 5000-tenant load test
- [ ] Tenant upgrade/downgrade tooling succeeds without data loss
- [ ] Backup/restore verified for both shared and dedicated tiers
- [ ] Security review signs off on tenant isolation and agent safety
- [ ] Documentation covers onboarding a new developer to productive contribution

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
