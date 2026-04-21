# Kapp Business Suite — Development Progress

> **Last Updated:** 2026-04-21
>
> Related documents: [README.md](./README.md) · [PROPOSAL.md](./PROPOSAL.md) · [ARCHITECTURE.md](./ARCHITECTURE.md)

---

## Current Phase

**Phase A — Kapp Kernel**
**Status:** In Progress

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

Foundation: tenant isolation, KType metadata, KRecord storage, permissions, audit, events, and the efficiency primitives that make thousands of tenants per cell viable.

### Deliverables

- [x] Go module skeleton (`services/api`, `services/worker`, `internal/*`)
- [ ] PostgreSQL schema for `tenants`, `users`, `user_tenants`, `roles`, `ktypes`, `krecords`, `events`, `audit_log`
- [ ] Row-level security policies on all tenant-scoped tables
- [ ] Tenant-range partitioning for `krecords`, `events`, `audit_log`
- [ ] KType schema registry and validator (Go)
- [ ] KRecord CRUD API with idempotency keys
- [ ] Event outbox + batched publisher
- [ ] Append-only audit logger with field-level diffs
- [ ] RBAC/ABAC policy evaluator
- [ ] Connection pool with tenant context injection (`SET LOCAL`)
- [ ] Per-tenant rate limiting middleware
- [ ] Per-tenant quota enforcement
- [ ] LRU metadata cache (shared, tenant-keyed)
- [ ] Tenant lifecycle (create, suspend, archive, delete)
- [ ] React app scaffold with generated API client
- [ ] KType-driven form and list views
- [ ] Storybook setup for UI components
- [ ] KChat bridge skeleton (card renderer, slash command dispatcher)
- [ ] OpenAPI spec generator from KType schemas
- [x] Local dev compose stack (api, worker, db, object store, event bus)

### Acceptance Criteria

- [ ] A KType can be registered and a KRecord created/updated/deleted via API
- [ ] All mutations produce an audit record and emit an event
- [ ] RLS prevents cross-tenant reads in negative tests
- [ ] Tenant isolation test suite passes (see "First Coding Slice" below)
- [ ] Verify zero resource consumption for idle tenants
- [ ] Verify sub-millisecond tenant context switching overhead
- [ ] Verify per-tenant rate limiting works correctly
- [ ] Load test: 1000 tenants on a single cell with acceptable latency

---

## Phase B — CRM, Tasks, Approvals, Forms

Chat-native work tracking and revenue pipeline on top of the kernel.

### Deliverables

- [ ] CRM KTypes: `crm.lead`, `crm.contact`, `crm.organization`, `crm.deal`, `crm.activity`, `crm.quote`
- [ ] Tasks KType: `tasks.task`
- [ ] Approvals engine: configurable chains, KChat approve/reject cards
- [ ] Forms KApp: anonymous and authenticated capture forms emitting KRecords
- [ ] KChat cards for all CRM + Tasks + Approvals objects
- [ ] Slash commands: `/lead`, `/contact`, `/deal`, `/task`, `/approve`, `/form`
- [ ] Composer actions: turn message → Task, Deal, Activity
- [ ] Right-pane detail views for all Phase B KTypes
- [ ] Agent tools: `crm.create_deal`, `crm.advance_deal`, `crm.summarize_pipeline`, `tasks.create_task`, `approvals.request`, `approvals.decide`

### Acceptance Criteria

- [ ] A Deal can be created from a KChat thread and progressed through its workflow
- [ ] An approval card posts to the right approvers and finalizes on decision
- [ ] All CRM records appear in the right pane and kanban views
- [ ] Agent tools execute with dry-run and confirmation where required
- [ ] Audit log shows the full lifecycle of each record

---

## Phase C — Finance Basics

Typed ledgers and the first postings from Kapps.

### Deliverables

- [ ] Finance KTypes: `finance.account`, `finance.journal_entry`, `finance.ar_invoice`, `finance.ap_bill`
- [ ] `accounts`, `journal_entries`, `journal_lines` tables with append-only invariants
- [ ] Double-entry posting engine with balance checks
- [ ] Period lockout (no edits to posted periods)
- [ ] Sales invoice posting flow (quote → invoice → ledger)
- [ ] Purchase bill posting flow
- [ ] Credit and debit notes
- [ ] AR / AP subledger views
- [ ] Basic VAT/GST tax codes
- [ ] Finance reports: trial balance, AR aging, AP aging, income statement
- [ ] KChat cards for invoices and bills
- [ ] Agent tools: `finance.create_sales_invoice`, `finance.create_ap_bill`, `finance.post_journal`

### Acceptance Criteria

- [ ] A sales invoice posts a balanced journal entry
- [ ] A purchase bill posts a balanced journal entry
- [ ] Trial balance sums to zero at all times
- [ ] Period lockout rejects edits to closed periods
- [ ] Audit log captures every posting with source record

---

## Phase D — Simple Inventory

First inventory primitives integrated with Sales and Procurement.

### Deliverables

- [ ] Inventory KTypes: `inventory.item`, `inventory.warehouse`, `inventory.move`, `inventory.stock_level`
- [ ] `inventory_items`, `inventory_warehouses`, `inventory_moves` tables
- [ ] Append-only stock move ledger
- [ ] Materialized stock levels with projection worker
- [ ] Goods receipt on purchase bill posting
- [ ] Goods delivery on sales invoice posting
- [ ] Multi-warehouse transfers
- [ ] Inventory valuation report
- [ ] KChat cards for stock moves and low-stock alerts
- [ ] Agent tools: `inventory.record_move`, `inventory.check_stock`

### Acceptance Criteria

- [ ] Sales invoice posts a delivery move; stock level decreases
- [ ] Purchase bill posts a receipt move; stock level increases
- [ ] Stock levels always match the sum of moves
- [ ] Warehouse transfers are balanced (one source decrement, one destination increment)

---

## Phase E — HR and LMS Starters

Employee lifecycle and structured learning.

### Deliverables

- [ ] HR KTypes: `hr.employee`, `hr.leave_request`, `hr.attendance`, `hr.expense_claim`
- [ ] HR workflows: onboarding, offboarding, leave approval
- [ ] Org chart view
- [ ] LMS KTypes: `lms.course`, `lms.module`, `lms.lesson`, `lms.enrollment`, `lms.quiz`, `lms.assignment`, `lms.progress`
- [ ] Learner KChat surface (enrollment card, `/learn` command, progress pane)
- [ ] Reviewer assignment workflow
- [ ] Agent tools: `hr.request_leave`, `hr.approve_leave`, `lms.recommend_course`, `lms.grade_assignment`

### Acceptance Criteria

- [ ] A leave request routes through approval and updates balance on decision
- [ ] A course enrollment tracks progress across modules and lessons
- [ ] A quiz submission is scored and recorded
- [ ] Reviewer assignment is notified via KChat

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

- [ ] Create two tenants `acme` and `globex` via API
- [ ] Register a KType `demo.note` with fields `title`, `body`
- [ ] Create user `alice` in `acme` and `bob` in `globex`
- [ ] Alice creates a note in `acme`; Bob creates a note in `globex`
- [ ] Alice's note list returns only `acme` notes
- [ ] Bob's note list returns only `globex` notes
- [ ] Direct DB query as Alice's session (tenant context set) returns only `acme` rows
- [ ] Direct DB query with no tenant context returns zero rows (RLS default-deny)
- [ ] Every create produces one event and one audit record
- [ ] Per-tenant rate limit kicks in after configured threshold
- [ ] Idle tenant `globex` has no active goroutines, no open connections, and no cached entries after idle timeout
- [ ] Tenant context switch from `acme` to `globex` measured under 1 ms on a warm gateway
