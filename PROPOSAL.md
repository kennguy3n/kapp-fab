# Kapp Business Suite — Proposal

> **Last Updated:** 2026-04-25
>
> Related documents: [README.md](./README.md) · [ARCHITECTURE.md](./ARCHITECTURE.md) · [PROGRESS.md](./PROGRESS.md)

---

## 1. Executive Summary

Kapp Business Suite is a **proprietary, Go + React, multi-tenant, KChat-native business platform** for SMEs. It delivers a full business operating layer — ERP-lite, Finance, Sales, Procurement, Inventory, HR, CRM, LMS, Projects, Approvals, Forms, Base tables, Docs/artifacts, and AI agents — natively integrated into KChat so operators can run their company without context-switching between apps.

**Core innovation:** a metadata-driven business object model called **KType**. A single KType schema definition generates the data model, validation, APIs, forms, list/kanban views, chat cards, audit trail, permissions, and AI agent tools for a business object. This collapses what would normally require thousands of lines of boilerplate into a single declarative artifact.

**Efficiency posture:** Kapp is designed so a single operator can serve **thousands of SME tenants on shared infrastructure** with minimal per-tenant overhead, **lazy resource allocation**, and **zero cost for idle tenants**. Tenants cost nothing while inactive; resources (caches, connections, workers) allocate on first use and reclaim on idle.

### Design Decisions

| Area | Decision |
| --- | --- |
| Product model | KChat KApps surfaced through chat cards, right panes, APIs, and AI agent tools |
| Frontend | React + TypeScript |
| Backend | Go modular core |
| Data model | Metadata-driven KTypes + typed ledgers for finance/inventory |
| Multi-tenancy | Shared infrastructure, row-level security, lazy allocation, zero-idle-cost |
| Efficiency | Sub-millisecond tenant switching, connection pooling, compact event encoding, minimal memory per tenant |

---

## 2. Product Vision

### 2.1 Definition

Kapp Business Suite is a set of KChat-native business applications for SMEs. It covers:

- **ERP-lite** — Finance, Sales, Procurement, Inventory
- **CRM** — Leads, contacts, organizations, deals, quotes, activities
- **HR** — Employees, leave, attendance, onboarding, offboarding
- **LMS** — Courses, enrollments, progress, quizzes
- **Projects** — Work, tasks, milestones
- **Approvals** — Configurable chains over any KType
- **Forms** — Metadata-driven capture forms
- **Base** — Spreadsheet-like flexible tables
- **Docs/artifacts** — Structured documents and AI-generated artifacts
- **AI agents** — KChat AI coworkers operating business data through permissioned tools

Each KApp surfaces through **three coordinated modes**:

1. **Chat UI** — cards, slash commands, thread actions, and right-pane views embedded directly in KChat channels and DMs.
2. **API** — REST/OpenAPI endpoints and event streams for integrations and headless automation.
3. **AI tools** — permissioned agent actions that allow KChat AI coworkers to read, write, and orchestrate business data safely.

**Example flow:**

> A customer negotiation thread in KChat → sales rep creates a **Deal** from the thread → generates a **Quotation** → routes a discount for **approval** → issues a **Sales Invoice** on approval → a **status card** posts back to the thread → an AI agent **monitors payment** status and nudges the customer when the invoice is overdue.

### 2.2 Product Principle

> **Conversation → Structured object → Workflow → Audit trail → Report → Agent action.**

This is the single invariant Kapp optimizes for. Every feature must strengthen the chain. Every chat interaction can become a first-class business record, every record can traverse a workflow, every step leaves an audit trail, every audit feeds reports, and every report can trigger AI agent action.

---

## 3. Feature Scope

### 3.1 Platform Core

| Capability | Kapp Implementation |
| --- | --- |
| Metadata model | **KType** — declarative business object definition (data, permissions, views, cards, API, agent tools) |
| Business record | **KRecord** — tenant-scoped, validated, versioned instance of a KType |
| Forms | Auto-generated from KType view definitions; drag-drop layout overrides |
| Lists | Filtered, paged, tenant-scoped collections with saved views |
| Kanban | State-based columns driven by KType status fields and workflow transitions |
| Reports & Insights | Visual query builder, composable dashboards, rich visualizations (bar, line, pie, donut, funnel, number card, pivot), AI-assisted queries, scheduled digests, and KChat dashboard cards — all over KRecords and typed ledgers |
| Workflows | State machines with guards, actions, notifications, and approval chains |
| Permissions | Role + attribute-based access control (RBAC + ABAC), enforced at DB row and API layer |
| Audit | Append-only audit log: who/what/when/before/after for every mutation |
| Attachments | S3-backed files with per-tenant zero-knowledge encryption via [ZK Object Fabric](https://github.com/kennguy3n/zk-object-fabric); content-addressable deduplication within tenant scope (no cross-tenant dedup, by design) |
| Notifications | KChat cards, in-app, email, webhook — routed by workflow and subscription |
| Agent tools | Permissioned AI tool definitions auto-generated from KType + agent policies |
| Import/export | Bulk import pipelines, CSV/JSON export, snapshot backups |
| App packaging | KApp manifest bundles KType definitions, cards, tools, migrations, and policies |

KType and KRecord are **original Kapp concepts**. KType defines *what a business object is*; KRecord is *an instance of one*. Both are tenant-scoped and enforce the efficiency and isolation invariants described in [ARCHITECTURE.md](./ARCHITECTURE.md).

### 3.2 ERP-lite Module

#### Finance

| Feature | MVP | Later |
| --- | --- | --- |
| Chart of accounts | ✓ | |
| Journal entries (typed append-only ledger) | ✓ | |
| AR / AP subledgers | ✓ | |
| Bank accounts and reconciliations | ✓ | |
| Tax codes (simple VAT/GST) | ✓ | |
| Multi-currency with FX rates | | ✓ |
| Period close and lockout | ✓ | |
| Consolidation across tenants | | ✓ |
| Country-specific tax packs | | ✓ |
| Cost centers / dimensions | ✓ | |
| Budgets | | ✓ |

#### Sales and Procurement

| Feature | MVP | Later |
| --- | --- | --- |
| Customers / suppliers | ✓ | |
| Quotations | ✓ | |
| Sales orders | ✓ | |
| Sales invoices (posted to ledger) | ✓ | |
| Credit notes | ✓ | |
| Purchase requisitions | | ✓ |
| Purchase orders | ✓ | |
| Purchase bills (posted to ledger) | ✓ | |
| Debit notes | ✓ | |
| Price lists / discounts | ✓ | |
| Sales commissions | | ✓ |
| Returns and RMAs | | ✓ |

#### Inventory

| Feature | MVP | Later |
| --- | --- | --- |
| Items / SKUs | ✓ | |
| Warehouses / locations | ✓ | |
| Stock moves (typed append-only ledger) | ✓ | |
| Stock levels and valuations | ✓ | |
| Goods receipts / deliveries | ✓ | |
| Lot / batch / serial tracking | | ✓ |
| Cycle counts | | ✓ |
| Landed costs | | ✓ |
| Multi-warehouse transfers | ✓ | |
| Reorder points and suggestions | | ✓ |

#### Manufacturing

All manufacturing features are **Later**. Manufacturing introduces domain complexity (BOMs, routings, work orders, capacity planning, shop-floor control) disproportionate to MVP value for general SME customers. It is deferred until a concrete customer demand is proven and financial/inventory primitives are stable enough to build on.

### 3.3 CRM Module

| Object | Key Fields | KChat Surface |
| --- | --- | --- |
| Lead | name, source, owner, status, score, contact_info | Card on capture; right-pane detail; `/lead` command |
| Contact | name, email, phone, organization, owner | Mention card; right-pane; `/contact` lookup |
| Organization | name, domain, industry, size, owner | Mention card; org-profile pane |
| Deal | name, stage, amount, currency, close_date, contact, organization | Thread card; kanban in right pane; `/deal` command |
| Activity | type (call/email/meeting), subject, date, contact, deal | Auto-logged from chat actions; thread card |
| Task | title, assignee, due_date, linked_to | Card with complete button; `/task` command |
| Quote | lines, discount, total, status, deal | Card + approval routing; `/quote` command |

**Agent examples:**

1. *"Qualify this lead based on its activity and score."* — reads lead + activities, returns qualification recommendation with justification.
2. *"Generate a quote draft for this deal at a 10% discount."* — calls `sales.create_quote` in dry-run; surfaces for approval.
3. *"Summarize open deals closing this quarter for the sales channel."* — reads deals filtered by close_date and stage; posts a digest card.
4. *"Nudge contacts who haven't replied in 7 days."* — schedules nudge messages; all sends logged as Activity records.
5. *"Convert this thread into a Deal."* — extracts entities from chat history and calls `crm.create_deal` for review.

### 3.4 HR Module

#### HR Core

| Feature | MVP | Later |
| --- | --- | --- |
| Employee records | ✓ | |
| Onboarding / offboarding workflows | ✓ | |
| Leave types and balances | ✓ | |
| Leave requests with approval | ✓ | |
| Attendance | ✓ | |
| Shift scheduling | | ✓ |
| Performance reviews | | ✓ |
| Expense claims | ✓ | |
| Documents (contracts, IDs) | ✓ | |
| Org chart | ✓ | |

#### Payroll

| Feature | MVP | Later |
| --- | --- | --- |
| Salary components (earnings/deductions) | ✓ | |
| Pay runs with approval | | ✓ |
| Payslips | | ✓ |
| Statutory tax withholding | | ✓ |
| Country-specific payroll packs | | ✓ |
| Bank file generation | | ✓ |
| Year-end / tax filings | | ✓ |

> **Country pack note:** statutory tax and filing rules are highly jurisdictional. Kapp ships payroll as pluggable **country packs** so each jurisdiction can be added independently without changing the core payroll engine.

### 3.5 LMS Module

| Object / Feature | Purpose |
| --- | --- |
| Course | Structured learning unit with modules, lessons, and resources |
| Module | Ordered group of lessons within a course |
| Lesson | Content unit (text, video, embed) with completion tracking |
| Enrollment | Tenant user enrolled in a course with progress state |
| Quiz | Multiple-choice / short-answer assessment with scoring |
| Assignment | Submission-based evaluation (reviewed by a reviewer role) |
| Progress | Per-user per-course completion, scoring, and activity timeline |
| Cohort | Group of learners progressing together |

**KChat surfaces:**

- Enrollment card posted to learner's DM with next-lesson CTA
- `/learn` slash command to start or resume a course
- Progress right-pane in learner profile
- Agent tool: `lms.recommend_course` based on role, activity, and gaps
- Reviewer notifications for assignment submissions

### 3.6 Approvals, Forms, Base, Docs

| KApp | Purpose |
| --- | --- |
| Approvals | Configurable multi-step approval chains over any KType record; routed to KChat with approve/reject cards |
| Forms | Metadata-driven capture forms (anonymous or authenticated) that emit KRecords on submission |
| Base | Spreadsheet-like flexible tables for ad-hoc structured data that doesn't justify a full KType |
| Docs | Structured documents and AI-generated artifacts (briefs, decks, reports) with versioning and permissions |

---

### 3.7 Object Storage Integration

Kapp file attachments live on a per-tenant zero-knowledge object storage layer provided by [ZK Object Fabric](https://github.com/kennguy3n/zk-object-fabric).

| Property | Implementation |
| --- | --- |
| Per-tenant credentials | Each tenant gets its own HMAC access/secret pair plus a dedicated bucket, minted by the Kapp setup wizard against the fabric's console API at `:8081`. The credentials live on the `tenants` row in `zk_access_key` / `zk_secret_key` / `zk_bucket`. |
| Encryption mode | **Managed.** The fabric handles per-tenant data-encryption-key derivation and AES-GCM encryption transparently — Kapp does not need to change its upload/download logic and never sees plaintext keys. |
| Routing | The `PerTenantS3Store` reads the tenant id off the request `context.Context` and either dispatches to the tenant's bucket or falls back to the platform-wide MinIO bucket when ZK columns are blank (gradual rollout). |
| Deduplication | Content-addressable (SHA-256) **within each tenant's bucket only**. Cross-tenant dedup is intentionally not supported because it would break ZK isolation. |
| Placement policy | Inherited from the tenant's fabric configuration (e.g. region, replication, retention) — Kapp does not override placement at the application layer. |
| Failure mode | If `ZK_FABRIC_ENDPOINT` is unset, the integration is disabled and the global MinIO path stays in place. If a per-tenant lookup fails the request errors out — the limiter never falls back to a neighbour's bucket. |

Reference downstream integrations on the fabric side: kmail, zk-drive, and Kapp Business Suite.

---

### 3.8 Insights Module

Kapp Insights is a tenant-scoped BI layer that lets SME users explore, visualize, and share business data without writing code. Reference architecture: [Frappe Insights](https://github.com/frappe/insights), adapted for Kapp's KType metadata model and multi-tenant isolation.

#### Core Concepts

| Concept | Description |
| --- | --- |
| **Query** | A saved, tenant-scoped analytical query over KRecords or typed ledgers. Defined via visual builder or SQL editor. Produces a tabular result set. |
| **Dashboard** | A composable canvas of widgets (charts, tables, number cards, filters) backed by one or more Queries. Tenant-scoped, shareable by role. |
| **Data Source** | An abstraction over queryable data. MVP sources: `ktype:<name>` (any KType's KRecords), `ledger.*` (finance/inventory typed tables). Later: external PostgreSQL connections, CSV uploads. |
| **Visualization** | A chart or table rendering of a Query result. Types: bar, line, pie, donut, funnel, scatter, number card, pivot table. |
| **Calculated Column** | A derived field defined by an expression over existing columns (e.g., `amount * quantity`, `DATEDIFF(due_date, NOW())`). Evaluated server-side in the query engine. |
| **Cache** | Per-query result cache with configurable TTL. Scheduled refresh via the existing `internal/scheduler` framework. |

#### Visual Query Builder

The visual query builder replaces the current JSON editor with a drag-and-drop interface:

1. **Pick source** — select a KType or ledger table from a searchable list (metadata from KType registry).
2. **Select columns** — drag fields from the source schema; add calculated columns via expression editor.
3. **Add filters** — visual filter rows with column/operator/value pickers; date range shortcuts.
4. **Group & aggregate** — drag columns to group-by; pick aggregation (sum, count, avg, min, max) per measure column.
5. **Sort & limit** — drag to reorder; set result cap.
6. **Join** (Later) — link two KType sources on a ref field for cross-entity queries.
7. **Preview** — live result preview with auto-refresh on definition change.

The builder serializes to the existing `reporting.Definition` grammar (extended with calculated columns and join specs).

#### Dashboard Builder

Dashboards are composable canvases:

- **Widgets**: chart, table, number card (single KPI), filter control.
- **Layout**: grid-based positioning (row/column/width/height per widget).
- **Linked filters**: a filter widget can drive the WHERE clause of multiple query widgets on the same dashboard.
- **Auto-refresh**: configurable per-dashboard refresh interval; uses query cache.
- **Sharing**: per-tenant, role-gated. Dashboard owner can grant read or edit to specific roles.

#### AI Query Assistant

Leverages the existing agent-tools infrastructure:

- `insights.generate_query` agent tool — accepts a natural-language question (e.g., "What are my top 10 customers by revenue this quarter?"), resolves available KTypes and their fields from the registry, and returns a `reporting.Definition` for review.
- `insights.explain_result` agent tool — accepts a query result and produces a plain-language summary suitable for posting to KChat.
- Dry-run by default; the user confirms before saving.

#### KChat Surfaces

| Surface | Purpose |
| --- | --- |
| `/insight` slash command | Run a saved query or dashboard by name; post result as a card |
| Dashboard digest card | Scheduled posting of a dashboard snapshot to a channel (daily/weekly) |
| Right-pane dashboard | View a dashboard in the KChat right pane without leaving the conversation |
| Agent: `insights.generate_query` | Natural-language query generation |
| Agent: `insights.explain_result` | Plain-language result summary |

#### Efficiency Considerations

- **Query result caching**: per-tenant, per-query LRU cache with configurable TTL. Cache keys include `(tenant_id, query_hash, filter_params)`. Idle cache entries evict naturally (zero-idle-cost).
- **Query timeout budget**: each query execution has a per-tenant timeout (default 30s) enforced via `statement_timeout` in the transaction. Prevents a single tenant's complex query from monopolizing the shared pool.
- **Row limit**: hard cap at 10,000 rows per query result (configurable per plan). Aggregation queries are encouraged over raw dumps.
- **Dashboard widget limit**: max 20 widgets per dashboard (configurable per plan) to bound rendering and query fan-out.

---

## 4. KChat Integration Model

### 4.1 UI Surfaces

| Surface | Purpose |
| --- | --- |
| **Message card** | Inline structured widget embedded in a chat message (e.g., Deal card, Invoice card); actionable via buttons |
| **Thread** | Persistent conversation anchored to a business object; auto-linked Activities and status updates |
| **Right pane** | Full object detail view without leaving the channel; forms, timeline, related records |
| **App dock** | Pinned KApp launchers in the sidebar; quick access to lists, kanbans, and reports |
| **Slash commands** | `/deal`, `/invoice`, `/task`, `/approve`, etc., for fast object creation and lookup |
| **Composer actions** | Turn a selected message into a Task, Deal, Activity, or other KRecord |
| **AI assistant** | Natural-language interface to KApp agent tools, with confirmation and audit |

### 4.2 API Surfaces

All endpoints are tenant-scoped via auth token. Tenant ID never appears in URL paths (prevents enumeration).

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/ktypes` | List available KTypes for the tenant |
| `GET` | `/api/v1/ktypes/{name}` | Retrieve a KType definition |
| `POST` | `/api/v1/records/{ktype}` | Create a KRecord |
| `GET` | `/api/v1/records/{ktype}` | List KRecords (filter, page, sort) |
| `GET` | `/api/v1/records/{ktype}/{id}` | Retrieve a KRecord |
| `PATCH` | `/api/v1/records/{ktype}/{id}` | Update a KRecord |
| `DELETE` | `/api/v1/records/{ktype}/{id}` | Soft-delete a KRecord (non-financial) |
| `POST` | `/api/v1/records/{ktype}/{id}/actions/{action}` | Invoke a workflow action |
| `POST` | `/api/v1/workflows/{name}/transitions` | Execute a workflow transition |
| `GET` | `/api/v1/reports/{name}` | Run a report |
| `POST` | `/api/v1/imports` | Create an import job |
| `GET` | `/api/v1/imports/{id}` | Get import job status |
| `GET` | `/api/v1/events/stream` | Subscribe to tenant event stream (SSE/WebSocket) |
| `POST` | `/api/v1/files` | Upload attachment |
| `POST` | `/api/v1/agents/tools/{name}` | Invoke an AI agent tool |

**Enforcement requirements:**

- All endpoints require auth token; tenant context derived from token claims only.
- Per-tenant rate limits enforced at the middleware layer.
- Every mutation generates an audit record and emits an event.
- Idempotency keys required for all mutating endpoints.
- Row-level security enforced at the database layer as defense-in-depth.

### 4.3 Agent Tool Model

Every KType can declare AI agent tools. Each tool has:

- A permission scope tied to the caller's role and the target tenant.
- A declared schema (inputs, outputs) auto-generated from the KType.
- A dry-run mode that previews effects without committing.
- A confirmation surface for destructive or high-value actions.
- Full audit of invocations: caller, agent, inputs, dry-run vs commit, outputs.

**Example tool:**

```yaml
name: finance.create_sales_invoice
description: Create a sales invoice for a customer from an approved quote or deal.
permission: finance.invoice.write
inputs:
  customer_id:
    type: ref
    ktype: crm.organization
    required: true
  deal_id:
    type: ref
    ktype: crm.deal
    required: false
  lines:
    type: array
    required: true
    item:
      type: object
      fields:
        item_id: { type: ref, ktype: inventory.item, required: true }
        quantity: { type: number, required: true, min: 0 }
        unit_price: { type: number, required: true, min: 0 }
        discount_pct: { type: number, required: false, min: 0, max: 100 }
  currency:
    type: string
    required: true
    pattern: "^[A-Z]{3}$"
  due_date:
    type: date
    required: true
outputs:
  invoice_id:
    type: id
  total:
    type: number
  status:
    type: enum
    values: [draft, pending_approval, posted]
modes:
  dry_run: true
  confirmation_required: true
audit:
  emit_event: finance.sales_invoice.created
```

**Agent rules:**

- Agents must hold a scoped permission token for the calling user's tenant and role.
- Agents must declare dry-run intent for destructive actions; the KChat UI shows a confirmation card.
- Agents must surface the source KRecords they read to justify their action (transparency).
- Agents cannot bypass workflows; they call the same APIs as humans.
- All agent actions are tagged in the audit log with agent identity and prompt trace.

---

## 5. Data Migration & Import

Kapp supports importing data from existing business systems so customers can onboard without a rip-and-replace. Supported sources include **Frappe-based platforms (ERPNext, HRMS, CRM, LMS)** and other common SME systems (CSV, JSON, generic SaaS exports).

### 5.1 Import Sources

| Source Artifact | Importer Support |
| --- | --- |
| Metadata export (object schemas) | Mapped to KType definitions via a normalization layer |
| Business records via REST API | Streaming read into KRecord staging tables |
| Attachments | Rehosted to the tenant's ZK Object Fabric bucket (per-tenant DEK, managed encryption mode); content-addressable deduplication within that bucket only |
| Workflows | Mapped to Kapp workflow definitions where semantically equivalent; flagged for manual review otherwise |
| Permissions | Role mappings imported into Kapp RBAC; attribute rules imported where expressible |
| Reports | Imported as saved queries against KRecords and ledgers; visualizations mapped to Kapp report types |
| Document templates | Imported as Kapp artifact templates |
| Custom logic | Presented to operator for conversion to Kapp workflows, agent tools, or backend code (no automatic code import) |

### 5.2 Import Pipeline

```
Discover  →  Export  →  Normalize  →  Map  →  Validate  →  Import staging  →  Reconcile  →  User acceptance  →  Final cutover
```

1. **Discover** — inventory source system entities, counts, dependencies.
2. **Export** — pull data from source (API, DB dump, file export).
3. **Normalize** — convert to a canonical intermediate representation.
4. **Map** — apply source → Kapp concept mapping (see 5.3).
5. **Validate** — schema, referential integrity, uniqueness, data quality checks.
6. **Import staging** — load into tenant-scoped staging tables.
7. **Reconcile** — compare totals, counts, and checksums against source; flag discrepancies.
8. **User acceptance** — operator review and sign-off on samples.
9. **Final cutover** — promote staging to live; lock source system; enable Kapp writes.

### 5.3 Concept Mapping

| Source System Concept | Kapp Concept |
| --- | --- |
| Source DocType | KType |
| Source record | KRecord |
| Custom field | KType field |
| Link field | KType `ref` field |
| Table child row | KRecord nested array field |
| Workflow | Kapp workflow state machine |
| Permission rule | Kapp RBAC/ABAC policy |
| Role | Kapp role |
| Report | Kapp report (saved query + visualization) |
| Print format | Kapp artifact template |
| Journal entry | Finance ledger entry |
| Stock ledger entry | Inventory ledger entry |

---

## 6. Key Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| **Scope explosion** | Strict MVP-first sequencing: CRM / tasks / approvals → finance → inventory → HR / LMS. Any new module must justify itself against the MVP principle. |
| **Accounting correctness** | Use **typed append-only ledgers**, not generic JSON only. Double-entry invariants enforced in DB schema and Go transaction logic. Period lockout prevents retroactive edits. |
| **Tenant data leakage** | Row-level security at the DB layer, tenant isolation test suite, per-tenant encryption keys, exhaustive audit, strict tenant middleware that fails closed. |
| **AI unsafe actions** | Dry-run by default for destructive tools, confirmation cards for high-value actions, permission checks at both API and tool layers, full audit, source display for every agent recommendation. |
| **Tenant resource abuse** | Per-tenant quotas (storage, API calls, workers), per-tenant rate limiting, lazy resource allocation with idle reclamation, circuit breakers per tenant so one tenant cannot degrade others. |
| **Heavy UI** | Build **KChat-native cards and right panes first**; full admin screens are secondary. Avoid duplicating UX surfaces that KChat already provides. |

---

## 7. Recommended MVP Cut

### Build (12)

1. Kapp kernel (KType, KRecord, tenant isolation, permissions, audit, events)
2. CRM (Lead, Contact, Organization, Deal, Activity, Task, Quote)
3. Tasks
4. Approvals
5. Forms
6. Finance basics (chart of accounts, journal entries, typed ledger)
7. Sales invoices
8. Purchase bills
9. Simple inventory (items, warehouses, stock moves)
10. Data importer
11. KChat cards and agent tools across all MVP KApps
12. Insights (visual query builder, composable dashboards, AI query assistant, KChat digest cards)

### Defer (9)

1. Manufacturing
2. Full payroll
3. Country tax packs
4. Advanced accounting consolidation
5. Advanced LMS certificates
6. POS
7. Website/CMS
8. Unrestricted low-code scripting
9. Generic marketplace
