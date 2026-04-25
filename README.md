# Kapp Business Suite

> **Last Updated:** 2026-04-25

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
  apps/
    web/                         # React app
    storybook/                   # UI component development
  services/
    api/                         # Go API gateway / BFF
    worker/                      # Go async workers
    importer/                    # Go import pipelines
    kchat-bridge/                # KChat card + command bridge
    agent-tools/                 # AI tool execution service
  packages/
    ui/                          # React component library
    schema/                      # KType schema definitions
    cards/                       # Card templates
    client/                      # Generated API client
  internal/
    tenant/
    authz/
    ktype/
    record/
    workflow/
    ledger/
    crm/
    hr/
    lms/
    reporting/
    audit/
    events/
    files/
  migrations/
    postgres/
  deploy/
    docker/
    helm/
    terraform/
  docs/
    adr/
    api/
    product/
  tests/
    integration/
    isolation/
    migration/
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

---

## Deferred Scope

- Manufacturing
- Full payroll
- Country tax packs
- Advanced accounting consolidation
- Advanced LMS certificates
- POS
- Website/CMS
- Unrestricted low-code scripting
- Generic marketplace

---

## Key Documents

- [PROPOSAL.md](./PROPOSAL.md) — Full business and product proposal.
- [ARCHITECTURE.md](./ARCHITECTURE.md) — Technical architecture and engineering design.
- [PROGRESS.md](./PROGRESS.md) — Development progress tracker with phases and acceptance criteria.

---

## Tech Stack

- **Language (backend):** Go
- **Language (frontend):** TypeScript + React
- **Database:** PostgreSQL (row-level security, partitioning by tenant)
- **Object storage:** [ZK Object Fabric](https://github.com/kennguy3n/zk-object-fabric) — S3-compatible gateway with per-tenant zero-knowledge encryption (managed mode); each Kapp tenant gets its own HMAC credentials and bucket so file attachments are encrypted under tenant-specific data-encryption keys.
- **Event bus:** Compact-encoded, batched event streaming
- **Cache + rate limit:** Redis (sliding-window token bucket via atomic Lua, shared across API replicas)
- **Deployment:** Docker, Helm, Terraform

## ZK Object Fabric Integration

File attachments uploaded through `internal/files` no longer land in the platform-wide MinIO bucket. Each tenant is provisioned a dedicated bucket and HMAC credential pair on the ZK Object Fabric during the setup wizard via the fabric's console API at `:8081`. The `PerTenantS3Store` then routes Put/Get calls based on the tenant id threaded through `context.Context`, falling back to the global MinIO bucket for tenants that have not yet been provisioned (gradual rollout).

Key properties:

- **Per-tenant key isolation.** Every tenant's objects are encrypted under that tenant's DEK, managed by the fabric. The platform operator cannot read another tenant's bytes without rotating that tenant's keys.
- **Content-addressable dedup stays in tenant scope.** SHA-256 keying is preserved inside each tenant's bucket; we deliberately do not deduplicate across tenants because that would break ZK isolation.
- **Backward compatible.** Tenants without `zk_access_key` columns set on their `tenants` row continue to use the global MinIO bucket. Operators can roll out the fabric gradually.

---

## License

Proprietary. All rights reserved.
