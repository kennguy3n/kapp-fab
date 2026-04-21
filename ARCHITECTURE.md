# Kapp Business Suite — Architecture

> **Last Updated:** 2025-04-21
>
> Related documents: [README.md](./README.md) · [PROPOSAL.md](./PROPOSAL.md) · [PROGRESS.md](./PROGRESS.md)

---

## 1. Design Philosophy: Efficiency at Scale

Kapp is designed for a single operator to serve **thousands of SME tenants** on shared infrastructure with minimal per-tenant overhead. Every architectural decision is evaluated against this constraint.

### Efficiency Invariants

- **Target.** Single operator → thousands of SME tenants per compute cell with acceptable latency.
- **Zero-idle-cost tenants.** Inactive tenants consume zero compute and minimal storage (only their persisted rows and metadata). No background work runs for a tenant with no active users or pending jobs.
- **Lazy resource allocation.** Per-tenant resources — caches, worker goroutines, connection allocations — are created on first use and reclaimed after an idle threshold. No upfront provisioning per tenant.
- **Shared connection pooling.** Kapp uses a shared PostgreSQL connection pool for all tenants. Tenant context is injected per transaction via `SET LOCAL app.tenant_id`, not per-tenant connections. This avoids the N_tenants × connection_pool_size blow-up.
- **Compact storage.** JSONB for flexible KRecord fields; typed relational tables for high-integrity ledgers (finance, inventory). No per-tenant schemas by default — schemas are shared with `tenant_id` partitioning.
- **Efficient event encoding.** Events are encoded in a compact binary format (or JSONB for DB outbox); publishing is batched; consumers apply backpressure.
- **Minimal memory per tenant.** No in-memory tenant state beyond LRU-cached metadata (keyed by tenant_id). Tenant context is request-scoped; goroutines are not pinned to tenants.
- **Horizontal scaling.** Stateless Go services behind a load balancer. Capacity is added by adding compute nodes, not by migrating tenants across nodes.
- **Resource budgets.** Per-tenant rate limits, storage quotas, API call budgets, worker time budgets. Enforced at middleware and worker layers. Prevents one tenant from degrading neighbors.
- **Cell architecture.** Tenants are grouped into **cells** — a cell is a (shared DB cluster + compute pool + object store) unit. Scaling past a cell's capacity happens by provisioning another cell, not by vertical scaling. Cell assignment is sticky per tenant.

### Non-Goals

- Per-tenant processes, goroutines, or containers by default (reserved for dedicated tiers).
- Per-tenant database schemas by default (reserved for dedicated tiers).
- Warm caches for all tenants — only active tenants' metadata lives in the LRU.

---

## 2. Target Architecture

```
                         ┌──────────────────────┐
                         │       KChat          │
                         │  (cards, commands)   │
                         └──────────┬───────────┘
                                    │
            ┌───────────────────────┴───────────────────────┐
            │                                               │
   ┌────────▼────────┐                            ┌─────────▼─────────┐
   │  KChat Bridge   │                            │    Web (React)    │
   │  (cards/events) │                            │  (admin + panes)  │
   └────────┬────────┘                            └─────────┬─────────┘
            │                                               │
            └───────────────────────┬───────────────────────┘
                                    │
                         ┌──────────▼───────────┐
                         │    API Gateway /     │
                         │        BFF (Go)      │
                         │  auth, rate-limit,   │
                         │  tenant middleware   │
                         └──────────┬───────────┘
                                    │
        ┌───────────────────────────┼───────────────────────────┐
        │                           │                           │
 ┌──────▼───────┐          ┌────────▼─────────┐       ┌─────────▼────────┐
 │   Modular    │          │   Async Workers  │       │   Agent Tools    │
 │  Go Core     │◀────────▶│   (Go, outbox,   │       │   Executor       │
 │  (kernel +   │  events  │    schedulers)   │       │   (dry-run,      │
 │   modules)   │          └────────┬─────────┘       │    permission,   │
 └──────┬───────┘                   │                 │    audit)        │
        │                           │                 └──────────────────┘
        │            ┌──────────────┤
        │            │              │
 ┌──────▼────┐  ┌────▼─────┐  ┌─────▼──────┐
 │ PostgreSQL│  │  Object  │  │  Event Bus │
 │ (shared,  │  │  Storage │  │  (compact, │
 │   RLS,    │  │  (S3)    │  │  batched)  │
 │ partition)│  └──────────┘  └────────────┘
 └───────────┘
```

**Modular-first, microservices-later.** The initial deployment is a **modular Go monolith** — a single binary containing the kernel and all KApp modules. This avoids microservice overhead (latency, deployment complexity, distributed tracing, sidecar sprawl) until scale data justifies the cost. Small operators ship one binary; large operators extract hot modules (e.g., agent-tools, importer) into separate services behind the same API gateway.

---

## 3. Service Boundaries

> **Note:** In the initial deployment, these are **logical modules within a single Go binary**, not separate processes. They can be extracted into separate services as scale demands without API changes.

| Service / Module | Responsibility |
| --- | --- |
| `api` | HTTP/gRPC gateway, auth, tenant middleware, rate limiting, request routing |
| `tenant` | Tenant lifecycle (create, suspend, archive, delete), cell assignment, quotas |
| `authz` | RBAC/ABAC policy evaluation, permission caching, role resolution |
| `ktype` | KType schema registry, validation, code generation, migration |
| `record` | KRecord CRUD, versioning, soft delete, list/filter, JSONB storage |
| `workflow` | State machines, transitions, guards, actions, approval chains |
| `ledger` | Typed append-only ledgers (finance, inventory); double-entry invariants |
| `crm` | CRM-specific KTypes and behaviors |
| `hr` | HR-specific KTypes and behaviors |
| `lms` | LMS-specific KTypes and behaviors |
| `reporting` | Saved queries, aggregations, pivot, chart serialization |
| `audit` | Append-only audit log writer and reader |
| `events` | Outbox pattern, event batching, delivery, consumer management |
| `files` | Attachment upload/download, content-addressable dedup, S3 integration |
| `kchat-bridge` | Card rendering, slash commands, thread actions, KChat API adapter |
| `agent-tools` | AI tool registry, permissioned execution, dry-run, confirmation, audit |
| `worker` | Async job execution, schedulers, retries, dead-letter handling |
| `importer` | Import pipelines (discover, export, normalize, map, validate, stage) |

*(13 kernel/infra services listed as the canonical set; CRM/HR/LMS are KApp modules built atop the kernel.)*

---

## 4. Multi-Tenancy Model

**Default posture: shared infrastructure with strict logical isolation.**

### Default (Shared) Tier

- `tenant_id` on every row in every KApp table.
- **PostgreSQL row-level security (RLS)** enforcing `tenant_id = current_setting('app.tenant_id')::uuid` on every query, as defense-in-depth.
- **Per-tenant encryption keys** derived from a master key (HKDF with tenant_id as salt); derived on demand, not stored separately per tenant for efficiency.
- **Per-tenant quotas and rate limits** (API calls, worker time, storage, record count).
- **Per-tenant backup/export** capability — operator can emit a single-tenant dump on demand.
- **Per-tenant audit stream** — tenant_id is indexed in the audit log for fast per-tenant replay.
- **Shared connection pooling with `SET LOCAL`** for tenant context — *not* per-tenant connection pools.
- **LRU metadata cache** shared across all tenants, keyed by `tenant_id`. Inactive tenants age out naturally.

### Upgrade Tiers

| Tier | Isolation | When |
| --- | --- | --- |
| Shared (default) | Row-level (RLS + tenant_id) | All standard SME tenants |
| Dedicated schema | Separate PostgreSQL schema per tenant | Tenants with compliance requirements that prefer schema-level isolation |
| Dedicated database | Separate DB per tenant in same cluster | Tenants with high volume or strict data-at-rest isolation |
| Dedicated cell | Separate shared infrastructure cell | Regulated tenants, VIP customers |
| Private deployment | Fully isolated Kapp installation | Enterprise customers with their own infrastructure |

### Efficiency Note

The **shared tier must support thousands of tenants per cell** with **sub-millisecond overhead for tenant context switching**. Techniques:

- `SET LOCAL` inside each transaction; no connection state leaks.
- Tenant auth decoded once per request at the gateway; propagated via Go `context.Context`.
- Permission/role caches are short-TTL LRUs keyed by `(tenant_id, user_id)` with invalidation on role change events.
- KType metadata is immutable per version → cacheable globally with tenant_id just selecting the tenant's enabled versions.

---

## 5. Data Storage

### 5.1 PostgreSQL

All core tables live in a shared PostgreSQL cluster. Core tables:

| Table | Purpose |
| --- | --- |
| `tenants` | Tenant registry, status, cell, quota plan |
| `users` | User identities (linked to KChat) |
| `user_tenants` | User membership in tenants with role |
| `roles` | Role definitions per tenant |
| `permissions` | Fine-grained permission grants |
| `ktypes` | KType schema registry (versioned) |
| `krecords` | Generic KRecord storage (JSONB) |
| `workflows` | Workflow definitions |
| `workflow_runs` | Active workflow state per record |
| `approvals` | Approval chain instances |
| `events` | Event outbox (mutations) |
| `audit_log` | Append-only audit trail |
| `imports` | Import jobs and status |
| `files` | Attachment metadata (object storage keys) |
| `notifications` | In-app and delivery notifications |

High-integrity typed tables (finance, inventory):

| Table | Purpose |
| --- | --- |
| `accounts` | Chart of accounts |
| `journal_entries` | Append-only double-entry journal |
| `journal_lines` | Journal entry lines (debit/credit) |
| `ar_invoices` | Sales invoices (posted to ledger) |
| `ap_bills` | Purchase bills (posted to ledger) |
| `inventory_items` | SKU registry |
| `inventory_warehouses` | Storage locations |
| `inventory_moves` | Append-only stock move ledger |
| `stock_levels` | Materialized per-item-per-warehouse levels |

**Efficiency annotations:**

- **Partitioning:** Large tables (`krecords`, `events`, `audit_log`, `inventory_moves`, `journal_lines`) are partitioned by **`tenant_id` range** for efficient vacuum, reduced lock contention, and per-tenant extraction/backup.
- **Indexes:** Composite indexes always **lead with `tenant_id`** so that tenant-scoped scans are cache-friendly and query plans short-circuit efficiently.
- **Connection management:** `pgbouncer` (or equivalent) in **transaction-mode pooling**. Tenant context is set via `SET LOCAL` inside each transaction.

### 5.2 Object Storage (S3-compatible)

Use cases:

1. KRecord attachments (documents, images, contracts).
2. Import source files (CSV, JSON, DB dumps).
3. Export artifacts (tenant dumps, report exports).
4. Artifact documents (AI-generated docs, templates).
5. Backups and snapshots.
6. Email/notification rendered HTML bodies (for audit).
7. Static KApp assets (card images, icons).

**Efficiency:** Use **content-addressable storage** (hash-keyed objects) for deduplication where appropriate — e.g., shared KApp templates, identical attachments across tenants, cached AI-generated outputs.

### 5.3 Event Bus

Event types emitted by the platform:

- `ktype.registered` / `ktype.updated`
- `krecord.created` / `krecord.updated` / `krecord.deleted`
- `workflow.transitioned` / `workflow.completed`
- `approval.requested` / `approval.granted` / `approval.rejected`
- `finance.journal.posted` / `finance.sales_invoice.created` / `finance.ap_bill.posted`
- `inventory.stock_move.recorded`
- `crm.deal.stage_changed` / `crm.activity.logged`
- `hr.leave.requested` / `hr.leave.approved`
- `lms.enrollment.completed`
- `import.started` / `import.completed` / `import.failed`
- `agent.tool.invoked` / `agent.tool.completed`
- `notification.delivered`

**Efficiency:**

- **Compact encoding** (MessagePack or Protobuf for in-flight; JSONB for outbox).
- **Batch publishing** — workers drain the outbox in batches, not per-event.
- **Consumer backpressure** — consumers signal lag; publisher throttles accordingly.
- Events are the backbone for **search indexing, notifications, reporting refresh, and agent triggers** — each consumer is a thin projection.

---

## 6. KType Metadata Model

**KType** is Kapp's metadata-driven business object definition. A single KType schema defines:

- Data model (fields and types)
- Validation rules
- Permissions (role and attribute-based)
- UI views (form, list, kanban, right-pane)
- Chat cards
- API endpoints
- AI agent tools

KType is an original Kapp concept. It is the unit of packaging, versioning, and evolution for business objects.

### Example

```yaml
name: crm.deal
version: 1
title: Deal
description: A revenue opportunity linked to a contact and organization.
fields:
  - name: name
    type: string
    required: true
    max_length: 200
  - name: stage
    type: enum
    values: [qualification, proposal, negotiation, won, lost]
    default: qualification
  - name: amount
    type: number
    min: 0
  - name: currency
    type: string
    pattern: "^[A-Z]{3}$"
    default: USD
  - name: close_date
    type: date
  - name: contact
    type: ref
    ktype: crm.contact
  - name: organization
    type: ref
    ktype: crm.organization
  - name: owner
    type: ref
    ktype: user
    required: true
  - name: notes
    type: text
indexes:
  - [tenant_id, stage]
  - [tenant_id, owner]
  - [tenant_id, close_date]
permissions:
  read: [crm.deal.read]
  create: [crm.deal.write]
  update: [crm.deal.write]
  delete: [crm.deal.admin]
workflow:
  name: crm.deal.pipeline
  transitions:
    - from: qualification
      to: proposal
      action: advance_to_proposal
    - from: proposal
      to: negotiation
      action: advance_to_negotiation
    - from: negotiation
      to: won
      action: mark_won
      post: [finance.create_sales_invoice]
    - from: [qualification, proposal, negotiation]
      to: lost
      action: mark_lost
views:
  form:
    sections:
      - title: Deal
        fields: [name, stage, amount, currency, close_date]
      - title: Relationships
        fields: [contact, organization, owner]
      - title: Notes
        fields: [notes]
  list:
    columns: [name, stage, amount, currency, close_date, owner]
    default_sort: -close_date
  kanban:
    group_by: stage
    card_title: name
    card_subtitle: amount
cards:
  message:
    title: "Deal: {{ name }}"
    subtitle: "{{ stage }} · {{ amount }} {{ currency }}"
    actions: [open, advance, comment]
agent_tools:
  - name: crm.create_deal
    permission: crm.deal.write
  - name: crm.advance_deal
    permission: crm.deal.write
    modes: { dry_run: true, confirmation_required: true }
audit:
  emit_events: true
  record_diffs: true
```

### What the Platform Generates from a KType

- Database storage (JSONB KRecord partition with indexed fields).
- REST API endpoints (`/api/v1/records/crm.deal/...`).
- Event emissions on every mutation.
- Audit log entries with field-level diffs.
- React form, list, and kanban views.
- KChat message card templates and slash command wiring.
- AI agent tool schemas with permission checks.
- Workflow state machine runtime.
- Permission policy evaluation bindings.
- Importer column-mapping UI and validators.

---

## 7. Initial Database Schema

```sql
-- Tenants, users, roles
CREATE TABLE tenants (
    id              UUID PRIMARY KEY,
    slug            TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    cell            TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('active','suspended','archived','deleting')),
    plan            TEXT NOT NULL,
    quota           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id              UUID PRIMARY KEY,
    kchat_user_id   TEXT NOT NULL UNIQUE,
    email           TEXT,
    display_name    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_tenants (
    user_id         UUID NOT NULL REFERENCES users(id),
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    role            TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('active','invited','suspended')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, tenant_id)
);
-- composite index leads with tenant_id for tenant-scoped lookups
CREATE INDEX ON user_tenants (tenant_id, user_id);

CREATE TABLE roles (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    name            TEXT NOT NULL,
    permissions     JSONB NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (tenant_id, name)
);

-- KType registry (shared, versioned)
CREATE TABLE ktypes (
    name            TEXT NOT NULL,
    version         INT NOT NULL,
    schema          JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
);

-- Generic KRecord store
-- partitioned by tenant_id range for efficient vacuum and per-tenant extraction
CREATE TABLE krecords (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    ktype           TEXT NOT NULL,
    ktype_version   INT NOT NULL,
    data            JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',
    version         INT NOT NULL DEFAULT 1,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      UUID,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);
-- composite index leads with tenant_id
CREATE INDEX ON krecords (tenant_id, ktype, updated_at DESC);

-- Workflows
CREATE TABLE workflows (
    tenant_id       UUID NOT NULL,
    name            TEXT NOT NULL,
    version         INT NOT NULL,
    definition      JSONB NOT NULL,
    PRIMARY KEY (tenant_id, name, version)
);

CREATE TABLE workflow_runs (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    workflow        TEXT NOT NULL,
    record_id       UUID NOT NULL,
    state           TEXT NOT NULL,
    history         JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX ON workflow_runs (tenant_id, record_id);

-- Approvals
CREATE TABLE approvals (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    record_ktype    TEXT NOT NULL,
    record_id       UUID NOT NULL,
    chain           JSONB NOT NULL,
    state           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX ON approvals (tenant_id, state);

-- Event outbox
-- partitioned by tenant_id range
CREATE TABLE events (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    type            TEXT NOT NULL,
    payload         JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);
CREATE INDEX ON events (tenant_id, created_at) WHERE delivered_at IS NULL;

-- Audit log
-- partitioned by tenant_id range; append-only
CREATE TABLE audit_log (
    id              BIGSERIAL,
    tenant_id       UUID NOT NULL,
    actor_id        UUID,
    actor_kind      TEXT NOT NULL CHECK (actor_kind IN ('user','agent','system')),
    action          TEXT NOT NULL,
    target_ktype    TEXT,
    target_id       UUID,
    before          JSONB,
    after           JSONB,
    context         JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);

-- Finance: chart of accounts + append-only journal
CREATE TABLE accounts (
    tenant_id       UUID NOT NULL,
    code            TEXT NOT NULL,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('asset','liability','equity','revenue','expense')),
    parent_code     TEXT,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (tenant_id, code)
);

CREATE TABLE journal_entries (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    posted_at       TIMESTAMPTZ NOT NULL,
    memo            TEXT,
    source_ktype    TEXT,
    source_id       UUID,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX ON journal_entries (tenant_id, posted_at);

-- partitioned by tenant_id range; never updated or deleted
CREATE TABLE journal_lines (
    id              BIGSERIAL,
    tenant_id       UUID NOT NULL,
    entry_id        UUID NOT NULL,
    account_code    TEXT NOT NULL,
    debit           NUMERIC(20,4) NOT NULL DEFAULT 0,
    credit          NUMERIC(20,4) NOT NULL DEFAULT 0,
    currency        TEXT NOT NULL,
    memo            TEXT,
    PRIMARY KEY (tenant_id, id),
    CHECK (debit >= 0 AND credit >= 0),
    CHECK (NOT (debit > 0 AND credit > 0))
) PARTITION BY RANGE (tenant_id);
CREATE INDEX ON journal_lines (tenant_id, entry_id);
CREATE INDEX ON journal_lines (tenant_id, account_code);

-- Inventory: items, warehouses, append-only moves
CREATE TABLE inventory_items (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    sku             TEXT NOT NULL,
    name            TEXT NOT NULL,
    uom             TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, sku)
);

CREATE TABLE inventory_warehouses (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    code            TEXT NOT NULL,
    name            TEXT NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, code)
);

-- partitioned by tenant_id range; append-only
CREATE TABLE inventory_moves (
    id              BIGSERIAL,
    tenant_id       UUID NOT NULL,
    item_id         UUID NOT NULL,
    warehouse_id    UUID NOT NULL,
    qty             NUMERIC(20,4) NOT NULL,
    unit_cost       NUMERIC(20,4),
    source_ktype    TEXT,
    source_id       UUID,
    moved_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);
CREATE INDEX ON inventory_moves (tenant_id, item_id, warehouse_id, moved_at);

-- Files / attachments
CREATE TABLE files (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    storage_key     TEXT NOT NULL,
    content_hash    TEXT NOT NULL,
    content_type    TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    uploaded_by     UUID NOT NULL,
    uploaded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX ON files (tenant_id, content_hash);

-- Row-level security policies (defense in depth)
ALTER TABLE krecords       ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_runs  ENABLE ROW LEVEL SECURITY;
ALTER TABLE approvals      ENABLE ROW LEVEL SECURITY;
ALTER TABLE events         ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log      ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts       ENABLE ROW LEVEL SECURITY;
ALTER TABLE journal_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE journal_lines  ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_warehouses ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_moves ENABLE ROW LEVEL SECURITY;
ALTER TABLE files          ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON krecords
    USING (tenant_id = current_setting('app.tenant_id')::uuid);
-- (similar policies for all tenant-scoped tables)
```

---

## 8. Backend Engineering Rules

1. **Go services are stateless.** Tenant context is request-scoped only, propagated via `context.Context`. No package-level tenant state.
2. **Every tenant-scoped table includes `tenant_id`.** Every query is tenant-scoped. No global queries in business code.
3. **Row-level security** is enabled on every tenant-scoped table as defense-in-depth.
4. **Shared connection pools** with `SET LOCAL app.tenant_id` inside every transaction. No per-tenant connection pools.
5. **No per-tenant goroutines, connections, or caches.** Use shared LRU caches keyed by `tenant_id`.
6. **Idempotency keys** are required on all mutating APIs. The gateway rejects unkeyed writes.
7. **Outbox pattern for events.** Emits are written in the same transaction as the state change; a publisher drains in batches.
8. **Never delete financial or audit records.** Corrections use reversal entries. Non-financial deletes are soft (tombstoned).
9. **Separate generic KRecord storage from typed ledger tables.** Finance and inventory integrity is not negotiable to JSONB.
10. **Every mutation emits an event and produces an audit record.** No silent writes.
11. **AI tool calls support dry-run and confirmation.** The execution layer enforces this even if a tool definition forgets to set it.
12. **Per-tenant rate limiting** at the middleware layer. Budgets are configurable per plan.
13. **Request timeout budgets** prevent tenant resource monopolization. Long-running work is pushed to the worker with its own budget.

---

## 9. Frontend Engineering Rules

1. **React + TypeScript.** Strict mode on. No `any`.
2. **Generated API client** from the OpenAPI spec. No hand-rolled fetch calls in feature code.
3. **KType-driven views.** Forms, lists, and kanbans render from KType metadata; per-KApp overrides are thin.
4. **Component library** shared across admin UI and KChat right-pane surfaces.
5. **Accessibility:** keyboard navigation, ARIA, color-contrast checks in Storybook.
6. **State:** server state via a query cache (e.g., React Query); local UI state via component state; no global UI store unless justified.
7. **Card rendering** uses a shared `<Card>` primitive with templates bound to KType card definitions.
8. **Error and empty states** are first-class; every view has explicit designs for them.
9. **Lazy-load KApp UI bundles.** Only load the code for KApps the user actually opens; shared kernel UI is always loaded.

---

## 10. API Design

- **Versioned** under `/api/v1/`; breaking changes introduce `/api/v2/`.
- **All endpoints accept tenant context from the auth token.** No `tenant_id` in URL paths (prevents enumeration and accidental cross-tenant routes).
- **Standard endpoints:**

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/ktypes` | List enabled KTypes |
| `GET` | `/api/v1/ktypes/{name}` | KType schema |
| `POST` | `/api/v1/records/{ktype}` | Create KRecord |
| `GET` | `/api/v1/records/{ktype}` | List KRecords |
| `GET` | `/api/v1/records/{ktype}/{id}` | Get KRecord |
| `PATCH` | `/api/v1/records/{ktype}/{id}` | Update KRecord |
| `DELETE` | `/api/v1/records/{ktype}/{id}` | Soft-delete (where permitted) |
| `POST` | `/api/v1/records/{ktype}/{id}/actions/{action}` | Workflow action |
| `POST` | `/api/v1/workflows/{name}/transitions` | Transition run |
| `GET` | `/api/v1/reports/{name}` | Run report |
| `POST` | `/api/v1/imports` | Start import |
| `GET` | `/api/v1/imports/{id}` | Import status |
| `GET` | `/api/v1/events/stream` | Event stream (SSE/WebSocket) |
| `POST` | `/api/v1/files` | Upload file |
| `POST` | `/api/v1/agents/tools/{name}` | Invoke agent tool |

- **Pagination:** cursor-based, tenant-scoped.
- **Filters:** declarative filter DSL evaluated at the DB layer.
- **Responses:** problem+json for errors; structured error codes.
- **Idempotency:** `Idempotency-Key` header required on mutations.

---

## 11. Agent Tool Model

Agent tools are first-class KApp artifacts. Every tool has:

- A **permission scope** — caller must hold the declared permission for the tenant.
- A **declared schema** for inputs and outputs — auto-generated from the KType where applicable.
- A **dry-run mode** that simulates the effect and returns what would happen without committing.
- A **confirmation surface** for destructive or high-value actions, rendered as a KChat card.
- An **audit record** for every invocation: caller, agent identity, inputs, dry-run vs commit, outputs, tenant, timestamp.

### Example

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

### Agent Rules

- Agents hold a scoped token for `(user, tenant, role)` — they cannot act outside it.
- Agents must declare dry-run intent for destructive calls; the UI shows a confirmation card before commit.
- Agents must surface the source KRecords they used to justify their recommendation (transparency).
- Agents cannot bypass workflows; they call the same APIs as humans.
- All agent actions are tagged in the audit log with agent identity and prompt trace.
