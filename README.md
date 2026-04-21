# Kapp Business Suite

> **Last Updated:** 2025-04-21

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
- **Object storage:** S3-compatible
- **Event bus:** Compact-encoded, batched event streaming
- **Deployment:** Docker, Helm, Terraform

---

## License

Proprietary. All rights reserved.
