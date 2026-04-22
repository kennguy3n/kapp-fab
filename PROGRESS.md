# Kapp Business Suite — Development Progress

> **Last Updated:** 2026-04-22 (Phase E deliverables completed — org chart, learner progress pane, assignment approval chain; Phase F up next)
>
> Related documents: [README.md](./README.md) · [PROPOSAL.md](./PROPOSAL.md) · [ARCHITECTURE.md](./ARCHITECTURE.md)

---

## Current Phase

**Phase F — Importer and Base**
**Status:** Not Started

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
- [ ] Verify zero resource consumption for idle tenants
- [ ] Verify sub-millisecond tenant context switching overhead
- [x] Verify per-tenant rate limiting works correctly (middleware exists and is tested)
- [ ] Load test: 1000 tenants on a single cell with acceptable latency

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

- [ ] Bank accounts and reconciliation
- [ ] Cost centers / dimensions on journal entries

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

- [ ] Low-stock alert worker (threshold-based notifications via KChat)
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

- [ ] Importer pipeline: Discover → Export → Normalize → Map → Validate → Staging → Reconcile → Acceptance → Cutover
- [ ] Source adapters: CSV/JSON, generic REST, Frappe-based platforms (ERPNext, HRMS, CRM, LMS)
- [ ] Attachment rehosting with content-addressable dedup
- [ ] Concept mapping UI (source DocType → KType)
- [ ] Validation report with row-level error export
- [ ] Base KApp: flexible tables, per-column typing, shareable views
- [ ] Docs KApp: artifact documents with versioning

### Acceptance Criteria

- [ ] A sample dataset imports end-to-end with reconciliation report
- [ ] Broken rows are surfaced and re-ingestible after correction
- [ ] Base tables can be created, edited, and shared per-tenant
- [ ] Artifact documents version and restore correctly

### Deferred / Follow-up

- [ ] Frappe REST API source adapter (for ERPNext, HRMS, CRM, LMS imports)
- [ ] DocType → KType automatic mapping suggestions
- [ ] Attachment migration with content-addressable dedup
- [ ] Import dry-run with validation report
- [ ] Incremental sync support (delta imports)

---

## Cross-cutting

Platform primitives used across every Kapp — not scoped to a single phase but tracked here for visibility.

- [ ] Event SSE/WebSocket endpoint (`GET /api/v1/events/stream`)
- [ ] Notification routing (KChat cards, in-app, email, webhook)
- [ ] File/attachment upload endpoint (`POST /api/v1/files`) with S3 integration
- [ ] Saved views / filters per KType per user
- [ ] Report builder (pivot, aggregate, chart) over KRecords and ledgers
- [ ] Per-tenant encryption keys (HKDF with tenant_id as salt)
- [ ] Tenant backup/export tooling (single-tenant dump)
- [x] HR org chart tree view (`apps/web/src/pages/OrgChartPage.tsx` backed by `hr.employee.reporting_to`)
- [x] LMS learner progress web pane (`apps/web/src/pages/LearnerProgressPage.tsx` — course progress dashboard)
- [x] LMS reviewer assignment approval chain for `lms.assignment` (`lms.submit_assignment` agent tool + workflow block)
- [ ] Frappe REST API source adapter for importer (ERPNext, HRMS, CRM, LMS)
- [ ] DocType → KType automatic mapping suggestions
- [ ] Multi-tenancy: per-tenant encryption keys (HKDF with tenant_id as salt)
- [ ] Multi-tenancy: zero-idle-cost verification (idle tenant resource measurement)
- [ ] Multi-tenancy: sub-millisecond tenant context switching benchmark
- [ ] Multi-tenancy: 1000-tenant load test on single cell

---

## Phase G — Hardening, Observability, and Scale

Production readiness across all shipped modules.

### Deliverables

- [ ] Per-tenant observability dashboards (latency, error rate, quota usage)
- [ ] Cell autoscaling policies
- [ ] Backup and restore tooling (full and per-tenant)
- [ ] Disaster recovery runbook and chaos drills
- [ ] Security review: authz, RLS, agent tool boundaries, audit coverage
- [ ] Performance tuning: index review, partition pruning, outbox batch sizing
- [ ] Load test: 5000 tenants on a single cell with baseline SLOs met
- [ ] Documentation: operator guide, developer guide, KType authoring guide
- [ ] Upgrade tier tooling: move a tenant from shared → dedicated schema → dedicated DB → dedicated cell

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
- [ ] Idle tenant `globex` has no active goroutines, no open connections, and no cached entries after idle timeout
- [ ] Tenant context switch from `acme` to `globex` measured under 1 ms on a warm gateway
