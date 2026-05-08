# Kapp Business Suite — Phase Roadmap

> **Last Updated:** 2026-05-08
>
> Related documents: [README.md](./README.md) · [PROPOSAL.md](./PROPOSAL.md) · [ARCHITECTURE.md](./ARCHITECTURE.md) · [PROGRESS.md](./PROGRESS.md)

This document is the canonical, high-level phase definition for Kapp Business
Suite. It describes the scope and goals of each delivery phase and the
cross-cutting work that runs alongside them.

For the detailed deliverable checklists, acceptance-criteria status, file
references, and PR-by-PR changelog, see [PROGRESS.md](./PROGRESS.md).

---

## Status Overview

| Phase | Title | Status |
| --- | --- | --- |
| A | Kapp Kernel | Complete |
| B | CRM, Tasks, Approvals, Forms | Complete |
| C | Finance Basics | Complete |
| D | Simple Inventory | Complete |
| E | HR and LMS Starters | Complete |
| F | Importer and Base | Complete |
| G | Hardening, Observability, and Scale | Complete |
| L | Insights | Complete |
| M | Vertical Depth | In Progress |

Phase letters skip to align with the existing PROGRESS.md numbering — Phases
H–K rolled their work back into the cross-cutting platform layer rather than
shipping as standalone phases. See PROGRESS.md "Cross-cutting" for that
detail.

---

## Phase A — Kapp Kernel

**Status:** Complete

The foundation that makes the rest of Kapp viable: tenant isolation, KType
metadata, KRecord storage, permissions, audit, events, and the efficiency
primitives that let a single cell host thousands of tenants.

**Scope.**

- Go service skeleton (`services/api`, `services/worker`, `internal/*`).
- PostgreSQL schema for `tenants`, `users`, `user_tenants`, `roles`, `ktypes`,
  `krecords`, `events`, `audit_log` with row-level security and tenant-range
  partitioning.
- KType schema registry, validator, and KRecord CRUD with idempotency keys.
- Event outbox + batched publisher and append-only audit logger with
  field-level diffs.
- RBAC/ABAC policy evaluator and connection pool with tenant context injection
  via `SET LOCAL`.
- Per-tenant rate limiting, quotas, LRU metadata cache, and tenant lifecycle
  (create / suspend / archive / delete).
- React app scaffold with generated API client, KType-driven form/list views,
  and Storybook setup.
- KChat bridge skeleton (card renderer, slash command dispatcher) and the
  local dev compose stack.

**Goal.** Demonstrate the kernel by registering a KType, creating a KRecord,
emitting an event, writing an audit row, and proving zero cross-tenant
leakage under RLS — all with sub-millisecond tenant context-switching
overhead and zero idle-tenant cost.

See [PROGRESS.md §Phase A](./PROGRESS.md#phase-a--kapp-kernel) for the full
deliverable checklist and acceptance-criteria status.

---

## Phase B — CRM, Tasks, Approvals, Forms

**Status:** Complete

Chat-native work tracking and revenue pipeline on top of the kernel.

**Scope.**

- CRM KTypes: `crm.lead`, `crm.contact`, `crm.organization`, `crm.deal`,
  `crm.activity`, `crm.quote`.
- Tasks KType `tasks.task`, configurable approval chains with KChat
  approve/reject cards, and the Forms KApp (anonymous + authenticated capture
  forms emitting KRecords).
- KChat cards, slash commands (`/lead`, `/contact`, `/deal`, `/task`,
  `/approve`, `/form`), composer actions (turn message → Task / Deal /
  Activity), and per-KType right-pane detail views.
- Agent tools across CRM / tasks / approvals (`crm.create_deal`,
  `crm.advance_deal`, `crm.summarize_pipeline`, `tasks.create_task`,
  `approvals.request`, `approvals.decide`).

**Goal.** Drive a deal from a KChat thread through approval to a posted
status card with a complete audit trail, all without leaving the channel.

See [PROGRESS.md §Phase B](./PROGRESS.md#phase-b--crm-tasks-approvals-forms)
for deliverable status.

---

## Phase C — Finance Basics

**Status:** Complete

Typed ledgers and the first postings from KApps.

**Scope.**

- Finance KTypes: `finance.account`, `finance.journal_entry`,
  `finance.ar_invoice`, `finance.ap_bill`.
- Append-only `accounts` / `journal_entries` / `journal_lines` tables with
  RLS, `CHECK` invariants, `fiscal_periods`, and `tax_codes`.
- Double-entry posting engine with balance checks and period lockout.
- Sales invoice and purchase bill posting flows wired to KChat slash
  commands and finance agent tools (`finance.create_sales_invoice`,
  `finance.create_ap_bill`, `finance.post_journal`,
  `finance.post_sales_invoice`, `finance.post_ap_bill`).
- Credit / debit notes (contra-entries), AR / AP subledger views,
  basic VAT/GST tax codes, and finance reports (trial balance, AR aging,
  AP aging, income statement).

**Goal.** Sales invoices and purchase bills post balanced journal entries,
trial balance always sums to zero, period lockouts hold, and every posting
is captured in the audit log alongside its source record.

See [PROGRESS.md §Phase C](./PROGRESS.md#phase-c--finance-basics) for
deliverable status.

---

## Phase D — Simple Inventory

**Status:** Complete

First inventory primitives, integrated with Sales and Procurement.

**Scope.**

- Inventory KTypes: `inventory.item`, `inventory.warehouse`,
  `inventory.move`, `inventory.stock_level`.
- Append-only `inventory_moves` ledger with materialized `stock_levels`
  projection updated inside the move transaction.
- Goods receipt on purchase bill posting and goods delivery on sales
  invoice posting, plus multi-warehouse transfers as paired moves.
- Inventory valuation report, KChat cards for stock moves and low-stock
  alerts, and agent tools (`inventory.record_move`,
  `inventory.check_stock`).

**Goal.** Stock levels always equal the sum of moves across receipts,
deliveries, adjustments, and transfers; transfers are balanced; and
finance/inventory stay coupled through posting hooks.

See [PROGRESS.md §Phase D](./PROGRESS.md#phase-d--simple-inventory) for
deliverable status.

---

## Phase E — HR and LMS Starters

**Status:** Complete

Employee lifecycle and structured learning.

**Scope.**

- HR KTypes: `hr.employee`, `hr.leave_request`, `hr.attendance`,
  `hr.expense_claim` with onboarding / offboarding / leave-approval
  workflows and a tree-rendered org chart view.
- LMS KTypes: `lms.course`, `lms.module`, `lms.lesson`, `lms.enrollment`,
  `lms.quiz`, `lms.assignment`, `lms.progress`.
- Learner KChat surface (`/learn` slash command, summary cards, web
  progress pane) and reviewer assignment workflow with single-step
  approval chains.
- Agent tools: `hr.request_leave`, `hr.approve_leave`,
  `lms.recommend_course`, `lms.grade_assignment`,
  `lms.submit_assignment`.

**Goal.** Leave requests route through approval and update balances,
course enrollments track progress per module/lesson, quiz submissions are
scored and recorded, and reviewer assignments fan out via KChat.

See [PROGRESS.md §Phase E](./PROGRESS.md#phase-e--hr-and-lms-starters)
for deliverable status.

---

## Phase F — Importer and Base

**Status:** Complete

Onboarding existing customers and supporting ad-hoc tables.

**Scope.**

- Importer pipeline: Discover → Export → Normalize → Map → Validate →
  Staging → Reconcile → Acceptance → Cutover.
- Source adapters: CSV / JSON, generic REST, and Frappe-based platforms
  (ERPNext, HRMS, CRM, LMS), including DocType → KType automatic
  mapping suggestions and incremental delta sync.
- Attachment rehosting with content-addressable dedup, concept-mapping
  UI, validation report with row-level error export, and dry-run
  validation endpoint.
- Base KApp: flexible per-tenant tables with per-column typing and
  shareable views.
- Docs KApp: artifact documents with append-only versioning and
  restore.

**Goal.** A representative SME dataset imports end-to-end with a
reconciliation report; broken rows can be corrected and re-ingested;
Base tables and Docs versioning round-trip cleanly.

See [PROGRESS.md §Phase F](./PROGRESS.md#phase-f--importer-and-base)
for deliverable status.

---

## Phase G — Hardening, Observability, and Scale

**Status:** Complete

Production readiness across all shipped modules.

**Scope.**

- Sales / procurement KTypes (sales orders, purchase orders, price lists)
  and HR salary components / structures.
- Bank accounts, bank reconciliation, cost centers, and journal
  dimensions.
- Per-tenant encryption (HKDF tenant keys, transparent field encryption,
  key-rotation CLI) and per-tenant observability (metrics middleware
  labelled with `{tenant_id, method, path, status}`).
- Backup and restore tooling (`services/kapp-backup`), tier-upgrade
  tooling (shared → dedicated schema with a scoped `kapp_tier_admin`
  SECURITY DEFINER role).
- Cell autoscaling policy and 5 000-tenant load-test harness.
- Disaster recovery runbook, performance tuning notes, audit-log hash
  chain, and CI rules (RLS coverage, agent import boundary, API
  versioning).
- Operator / developer / KType-authoring guides under `docs/`.

**Goal.** All shipped KApps meet SLOs under a 5 000-tenant load test on
a single cell; tier upgrade and backup/restore are verified on both
shared and dedicated tiers; and the security review signs off on tenant
isolation and agent safety.

See [PROGRESS.md §Phase G](./PROGRESS.md#phase-g--hardening-observability-and-scale)
for deliverable status.

---

## Phase L — Insights

**Status:** Complete

Tenant-scoped BI layer over KRecords and typed ledgers.

**Scope.**

- `internal/insights/` package: query store, dashboard store, cache
  store, and query-engine extensions (calculated columns, cross-KType
  joins).
- Tables `insights_queries`, `insights_dashboards`,
  `insights_dashboard_widgets`, `insights_query_cache`, and
  `insights_shares` — all RLS-protected and tenant-partitioned.
- Visual query builder, dashboard builder, rich chart visualizations
  (bar, line, pie, donut, funnel, number card, pivot, table) using
  Recharts.
- KChat surfaces: `/insight` slash command, dashboard digest cards,
  right-pane mini dashboards.
- AI agent tools: `insights.generate_query`,
  `insights.explain_result`, `insights.post_dashboard_digest`.
- Per-query result caching with TTL and scheduled refresh, per-tenant
  `statement_timeout`, plan-gated `insights` feature flag, and
  shipped follow-ups (external data source connections, cross-KType
  joins, dashboard embedding via iframe / public link).

**Goal.** A tenant user can build a query visually, save it, and see
cached results on re-run; a 5+-widget dashboard renders correctly with
linked filters; AI generates a valid query from a natural-language
prompt; dashboard digest cards post on schedule; and RLS keeps every
tenant's queries / dashboards / shares isolated.

See [PROGRESS.md §Phase L](./PROGRESS.md#phase-l--insights) for
deliverable status.

---

## Phase M — Vertical Depth

**Status:** In Progress

Deepens the existing modules with the features that production SMEs ask
for once the core platform is stable. Shipped via PRs #50–#61. Most of
the planned scope is complete — the remaining open item is the
notebook / exploratory analysis interface deferred from Phase L.

**Scope.**

- Insights SQL editor mode — visual + raw SQL with per-tenant
  `statement_timeout` and RLS preserved (PR #50).
- Country tax packs — US and AU on the payroll engine (PR #51).
- Shift scheduling — KTypes, agent tool, `/shift` slash command,
  presence-based late detection, and calendar UI (PR #52).
- Performance review / appraisal surface (PR #53).
- Projects and milestones — KTypes, agent tools, `/project` slash
  command, and Gantt views (PR #54).
- POS module — KTypes, finalize flow, offline queue, and register UI
  (PR #55).
- Advanced accounting consolidation — operator-scoped, admin-only
  cross-tenant rollup (PR #56).
- Webhook v2 — conditional matching, per-webhook retries, jittered
  exponential backoff, and a delivery-log UI (PR #57).
- Phase M follow-up fixes addressing review feedback on PRs #53 / #54 /
  #55 / #57 (PR #58).
- Demo mode with a mock data layer and module screenshots (PR #59).
- RBAC enhancements — multi-role, role hierarchy, wildcards, condition
  expressions, field-level authorization, and a role management API
  (PR #60).
- RBAC authz enforce toggle (`KAPP_AUTHZ_ENFORCE`) so the new
  evaluator can be rolled out gradually (PR #61).

**Goal.** Close the highest-impact vertical gaps in HR (shift
scheduling, performance reviews, country tax packs), Projects (Gantt
+ milestones), Sales (POS), Finance (consolidation), and Platform
(webhook v2, RBAC depth, demo mode) so a real SME can run end-to-end
on Kapp without falling back to the generic KRecord shell.

See [PROGRESS.md §Phase M](./PROGRESS.md#phase-m--vertical-depth) for
the per-PR deliverable status.

---

## Cross-cutting

These items aren't scoped to a single phase but are tracked alongside
them in PROGRESS.md because they underpin every module:

- Authentication (JWT issuance/validation, KChat SSO, session
  revocation, per-tenant session limits) and authorization
  (granular permissions table with JSONB conditions).
- Multi-tenancy primitives: per-tenant encryption keys (HKDF),
  zero-idle-cost verification, sub-millisecond context-switching
  benchmarks, 1 000-tenant and 5 000-tenant load tests, distributed
  rate limiting backed by Redis, per-tenant feature flags, metering,
  data retention, and the runtime isolation audit endpoint.
- ZK Object Fabric integration: per-tenant credentials, buckets,
  routing via `PerTenantS3Store`, placement-policy editor, and
  bounded LRU of per-tenant clients.
- Reporting and notifications: report builder over KRecords / ledgers,
  scheduled report email delivery, saved-report sharing, notification
  routing (KChat / in-app / email / webhook) with HMAC signatures.
- Search, full-text indexing, print/PDF generation, exporter (per-KType
  CSV/JSON + full-tenant dump), tenant resource metering, and webhook
  management.
- Frontend pages backing the shipped backend surfaces: dashboard,
  setup wizard, bank reconciliation, cost centers, sales / purchase
  orders, price lists, payroll, and the Insights builder.

See [PROGRESS.md §Cross-cutting](./PROGRESS.md#cross-cutting) for the
detailed per-item status and file references.

---

## How to use this document

- **Reading the roadmap:** Use this file to understand what each phase
  means and how phases relate to each other. It does not change between
  PRs unless the phase scope itself changes.
- **Tracking progress:** Use [PROGRESS.md](./PROGRESS.md) for the live
  per-deliverable checklist, acceptance-criteria results, and PR-level
  changelog. Every `[ ]`, `[~]`, `[x]`, `[!]`, and `[-]` in the legend
  lives there, not here.
- **Reading the architecture:** Use [ARCHITECTURE.md](./ARCHITECTURE.md)
  for service boundaries, multi-tenancy posture, the database schema,
  the agent tool model, and the Insights engine design.
- **Reading the product proposal:** Use [PROPOSAL.md](./PROPOSAL.md)
  for the business framing, feature scope, and MVP-cut rationale.
