# Kapp Business Suite

> **Last Updated:** 2026-05-03

**An extreme-lightweight, multi-tenant business operating platform for KChat — built in Go + React for efficient operation at scale with thousands of SME tenants.**

---

## Overview

Kapp Business Suite is a set of KChat-native business applications for SMEs covering ERP-lite, Finance, Sales, Procurement, Inventory, HR, CRM, LMS, Projects, Approvals, Forms, Base tables, Docs/artifacts, and AI agents.

Each KApp surfaces through three coordinated modes:

- **Chat UI** — cards, commands, thread actions, and right-pane views embedded directly in KChat conversations.
- **API** — REST/OpenAPI endpoints plus event streams for programmatic and cross-system integration.
- **AI tools** — permissioned agent actions that let KChat AI coworkers read, write, and orchestrate business data safely.

Kapp is designed from the ground up for operators running **thousands of tenants on shared infrastructure** with minimal per-tenant resource overhead, zero cost for idle tenants, and sub-millisecond tenant context switching.

---

## Product Principle

> **Conversation → Structured object → Workflow → Audit trail → Report → Agent action.**

Every business interaction in Kapp flows through this chain: what begins as a chat message can become a first-class business record, traverse an approval workflow, leave a full audit trail, feed reports, and be acted on by AI agents — without ever leaving KChat.

---

## Core Design Decisions

| Area | Decision |
| --- | --- |
| Product model | KChat KApps surfaced through chat cards, right panes, APIs, and AI agent tools |
| Frontend | React + TypeScript |
| Backend | Go modular core — stateless, minimal memory footprint, efficient connection pooling |
| Data model | Metadata-driven KTypes + typed ledgers for finance/inventory |
| Multi-tenancy | Shared infrastructure with `tenant_id`, row-level security, lazy resource allocation, zero-idle-cost tenants, quotas, optional dedicated resources |
| Efficiency target | Thousands of SME tenants per compute cell with sub-millisecond tenant context switching |

---

## Monorepo Structure

```
kapp/
  .github/
    workflows/              # CI: ci.yml, migration-rls-check, agent-import-check, api-versioning-check
  apps/
    web/                    # React + TypeScript frontend
    storybook/              # UI component development
  cmd/
    rotate-master-key/      # Master key rotation CLI
  docs/                     # Operator guide, developer guide, KType authoring, DR runbook, etc.
  internal/
    agents/                 # AI agent tool implementations
    audit/                  # Append-only audit log
    auth/                   # JWT, KChat SSO, sessions
    authz/                  # RBAC/ABAC policy evaluation
    base/                   # Base (flexible tables) KApp
    crm/                    # CRM KTypes and behaviors
    dashboard/              # Tenant KPI dashboard aggregation
    dbutil/                 # Database utilities (tenant tx, SET LOCAL)
    docs/                   # Docs/artifacts KApp
    events/                 # Event outbox and publishing
    exporter/               # Data export (per-KType, full tenant)
    files/                  # File/attachment storage (S3, ZK Fabric)
    finance/                # Finance KTypes (payment, recurring, payment terms)
    forms/                  # Forms KApp
    helpdesk/               # Helpdesk KTypes, SLA, inbound email
    hr/                     # HR KTypes, payroll engine, tax packs
    importer/               # Import pipeline and source adapters
    insights/               # BI query engine, dashboard store, cache
    integrationtest/        # Integration and load tests
    inventory/              # Inventory KTypes, stock moves, batches, reorder
    ktype/                  # KType schema registry and validation
    ledger/                 # Typed ledgers (finance, inventory), posting engine
    lms/                    # LMS KTypes, certificates
    notifications/          # Notification routing (SMTP, webhook)
    platform/               # Metrics, autoscaler, rate limiter, feature flags, metering, retention
    print/                  # PDF/print template rendering
    projects/               # Projects + milestones KTypes
    record/                 # KRecord CRUD, versioning, encryption
    reporting/              # Report builder, saved reports, scheduling
    sales/                  # Sales/procurement KTypes, POS, price lists
    scheduler/              # Tenant-scoped scheduled actions
    tenant/                 # Tenant lifecycle, wizard, encryption, metering, ZK fabric client
    workflow/               # State machines, transitions, approval chains
  migrations/               # PostgreSQL migrations (000001–000050)
  packages/
    cards/                  # Card templates
    client/                 # Generated API client
    schema/                 # KType schema definitions
    ui/                     # React component library
  scripts/                  # upgrade_tier.sh, rotate_master_key.sh, capture-screenshots
  services/
    agent-tools/            # AI tool execution service
    api/                    # Go API gateway / BFF
    importer/               # Go import pipeline service
    kapp-backup/            # Per-tenant backup/restore CLI
    kchat-bridge/           # KChat card + command + presence bridge
    worker/                 # Go async workers (notifications, SLA, cache, exports, etc.)
```

---

## MVP Scope

1. **Kapp kernel** — KType metadata engine, KRecord storage, tenant isolation, permissions, audit, events.
2. **CRM** — Leads, Contacts, Organizations, Deals, Activities, Quotes.
3. **Tasks** — Work tracking with assignments, states, and due dates.
4. **Approvals** — Configurable approval chains with KChat surfaces.
5. **Forms** — Metadata-driven form capture with validation.
6. **Finance basics** — Chart of accounts, journal entries, typed append-only ledger.
7. **Sales invoices** — Quote-to-invoice flow with posting to the finance ledger.
8. **Purchase bills** — Supplier bills with approval and posting.
9. **Simple inventory** — Items, warehouses, stock moves with typed ledger.
10. **Data importer** — Pipelines for onboarding data from existing business systems.
11. **KChat cards and agent tools** — Chat-native surfaces and permissioned AI actions across all MVP KApps.
12. **Insights** — Visual query builder, composable dashboards, rich chart visualizations, AI-assisted queries, and KChat digest cards over KRecords and typed ledgers.

---

## Deferred Scope

- Manufacturing
- Advanced LMS certificates
- Website/CMS
- Unrestricted low-code scripting
- Generic marketplace
- Insights: notebook/exploratory analysis interface

---

## Key Documents

- [PROPOSAL.md](./PROPOSAL.md) — Full business and product proposal.
- [ARCHITECTURE.md](./ARCHITECTURE.md) — Technical architecture and engineering design.
- [PHASES.md](./PHASES.md) — Phase roadmap and per-phase scope definitions.
- [PROGRESS.md](./PROGRESS.md) — Development progress tracker with phases and acceptance criteria.

---

## Quick Start

1. Clone the repo and start infrastructure:
   ```
   make compose-up
   ```

2. Generate the protobuf Go bindings (one-time after a fresh clone — the `gen/` tree is gitignored and reproduced from `proto/`):
   ```
   make proto-gen
   ```
   This installs the `protoc-gen-go` and `protoc-gen-go-grpc` plugins on first run, then drives `buf generate`. Re-run it whenever a `.proto` file changes.

3. Run all migrations:
   ```
   make migrate
   ```

4. Start the API server:
   ```
   make run-api
   ```

5. Start the async worker:
   ```
   make run-worker
   ```

6. Start the KChat bridge:
   ```
   make run-kchat-bridge
   ```

---

## Running Tests

```sh
# Unit tests
make test

# Integration tests (requires running PostgreSQL from docker-compose)
make test-integration

# Load tests (gated behind build tag)
KAPP_TEST_DB_URL="..." go test -tags=integration,loadtest -v ./internal/integrationtest/loadtest/...

# Lint
make lint
```

---

## Tech Stack

- **Language (backend):** Go
- **Language (frontend):** TypeScript + React
- **Database:** PostgreSQL (row-level security, partitioning by tenant)
- **Object storage:** [ZK Object Fabric](https://github.com/kennguy3n/zk-object-fabric) — S3-compatible gateway with per-tenant zero-knowledge encryption (managed mode); each Kapp tenant gets its own HMAC credentials and bucket so file attachments are encrypted under tenant-specific data-encryption keys.
- **Event bus:** Compact-encoded, batched event streaming
- **Cache + rate limit:** Redis (sliding-window token bucket via atomic Lua, shared across API replicas)
- **Deployment:** Docker, Helm, Terraform
- **Charting:** Recharts for Insights visualizations (`recharts ^3.8.1`, used by `apps/web/src/components/insights/Charts.tsx`)

## ZK Object Fabric Integration

File attachments uploaded through `internal/files` no longer land in the platform-wide MinIO bucket. Each tenant is provisioned a dedicated bucket and HMAC credential pair on the ZK Object Fabric during the setup wizard via the fabric's console API at `:8081`. The `PerTenantS3Store` then routes Put/Get calls based on the tenant id threaded through `context.Context`, falling back to the global MinIO bucket for tenants that have not yet been provisioned (gradual rollout).

Key properties:

- **Per-tenant key isolation.** Every tenant's objects are encrypted under that tenant's DEK, managed by the fabric. The platform operator cannot read another tenant's bytes without rotating that tenant's keys.
- **Content-addressable dedup stays in tenant scope.** SHA-256 keying is preserved inside each tenant's bucket; we deliberately do not deduplicate across tenants because that would break ZK isolation.
- **Backward compatible.** Tenants without `zk_access_key` columns set on their `tenants` row continue to use the global MinIO bucket. Operators can roll out the fabric gradually.

---

## License

Proprietary. All rights reserved.
