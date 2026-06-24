# Honest Competitive Assessment

A comparison is only useful to a buyer if it admits where the competition wins. This post is written to survive scrutiny from someone who already uses Xero, ERPNext, or Odoo and will fact-check us. The short version: **Kapp's edge is breadth on one multi-tenant platform with strong tenant isolation; the established players' edge is depth, polish, and ecosystem in their home domain.**

## Where each competitor actually plays

| Competitor | Home domain | Genuinely strong at |
|---|---|---|
| **Xero** | Accounting | Bank feeds & reconciliation, accountant ecosystem, UX polish |
| **QuickBooks** | Accounting | US market depth, payroll add-ons, accountant familiarity |
| **ERPNext** | Open-source ERP | Breadth, full MRP, free/self-host, large community |
| **Odoo** | Modular business apps | App marketplace, manufacturing & inventory depth, website/e-commerce |
| **SAP Business One** | SME ERP | Deep finance/MRP, mature localisations, enterprise-grade |
| **NetSuite** | Cloud ERP | Multi-entity consolidation, scale, reporting |
| **TalentLMS** | LMS | Fast course setup, clean learner UX |
| **Moodle / Docebo** | LMS | Course authoring depth, SCORM/xAPI maturity, plugins |

## Feature-by-feature, honestly

Legend: ✅ strong · 🟡 functional/covers the 80% · ⬜ not a focus / weaker

| Dimension | Kapp | Xero | QuickBooks | ERPNext | Odoo | SAP B1 | TalentLMS | Moodle |
|---|---|---|---|---|---|---|---|---|
| Core accounting / GL | 🟡 | ✅ | ✅ | ✅ | ✅ | ✅ | ⬜ | ⬜ |
| Bank feeds & smart rec | 🟡¹ | ✅ | ✅ | 🟡 | 🟡 | ✅ | ⬜ | ⬜ |
| Multi-currency | ✅ | 🟡 | 🟡 | ✅ | ✅ | ✅ | ⬜ | ⬜ |
| Statutory tax packs (60+ countries) | ✅ | 🟡² | 🟡² | 🟡 | 🟡 | ✅ | ⬜ | ⬜ |
| CRM / deal pipeline | 🟡 | ⬜ | ⬜ | 🟡 | ✅ | 🟡 | ⬜ | ⬜ |
| Inventory & valuation | 🟡 | ⬜ | 🟡 | ✅ | ✅ | ✅ | ⬜ | ⬜ |
| Manufacturing (BOM/WO/routing/capacity/job cards) | 🟡 | ⬜ | ⬜ | ✅ | ✅ | ✅ | ⬜ | ⬜ |
| Recruitment / ATS | 🟡 | ⬜ | ⬜ | 🟡 | 🟡 | 🟡 | ⬜ | ⬜ |
| HR / payroll breadth | 🟡 | ⬜ | 🟡 | ✅ | 🟡 | ✅ | ⬜ | ⬜ |
| LMS (paths, learner analytics) | 🟡 | ⬜ | ⬜ | 🟡 | 🟡 | ⬜ | ✅ | ✅ |
| Single platform, all of the above | ✅ | ⬜ | ⬜ | ✅ | ✅ | 🟡 | ⬜ | ⬜ |
| Multi-tenant isolation (RLS per tenant) | ✅ | n/a³ | n/a³ | 🟡⁴ | 🟡⁴ | ⬜ | n/a³ | 🟡⁴ |
| Extensibility (custom KTypes/fields) | ✅ | 🟡 | 🟡 | ✅ | ✅ | 🟡 | 🟡 | ✅ |
| Customer portal & self-service | 🟡 | ⬜ | ⬜ | 🟡 | ✅ | 🟡 | ⬜ | ⬜ |

¹ Bank-feed provider integration (Plaid/GoCardless) is built but not yet wired end-to-end on `main` — see "What we are honestly not yet" below. CSV import + auto-match works today.
² Strong in their core regions; broad statutory localisation usually needs partner add-ons.
³ SaaS-isolated by vendor; not a model the customer operates.
⁴ Typically one-tenant-per-instance or schema-per-tenant; not row-level isolation in a shared table.

## Where Kapp genuinely leads

- **Breadth on one platform with real isolation.** Every screenshot in this series — CRM, sales, finance, helpdesk, projects, inventory, manufacturing, HR, LMS, insights, admin, and portal — runs on the *same binary and database*, isolated by Postgres row-level security per `tenant_id`. Competitors achieve breadth either by being a single domain or by running heavier per-instance deployments.
- **No reconciliation between modules.** A deal becomes an invoice, a receivable, and a trial-balance line — one number, not three systems agreeing. A completed work order moves inventory and can hit the ledger in the same transaction.
- **Statutory localisation as a first-class, code-reviewed asset.** 60+ country tax packs with withholding logic and matching charts of accounts, selectable at setup.
- **Extensible by data model.** New record types and fields (KTypes) are a platform primitive, so a tenant can model their own objects without a fork.
- **Built-in operational governance.** Audit log, feature flags, usage metering, webhooks, and retention policies are not afterthoughts; they are part of the same tenant-scoped surface.

## Where competitors are still better (and we should say so)

- **Xero / QuickBooks** remain the gold standard for *pure accounting*: live bank feeds, reconciliation UX, and a deep accountant/advisor ecosystem. An accounting-only SME with a bookkeeper who lives in Xero has little reason to switch today.
- **ERPNext / Odoo** have deeper, more mature **manufacturing and inventory** (full MRP runs, subcontracting, lot/serial, quality, backward scheduling) and a large app marketplace and community. Odoo's website/e-commerce and customer portal are in another league.
- **SAP Business One / NetSuite** beat us on enterprise-grade finance, multi-entity consolidation, and the depth of mature localisations for complex regulatory regimes.
- **Moodle / Docebo / TalentLMS** are purpose-built LMS platforms with far richer authoring, SCORM/xAPI maturity, assessment types, and plugins. If training *is* your business, a dedicated LMS wins.
- **HubSpot / Salesforce** still dominate marketing automation, lead scoring, email sequencing, and sales analytics. Kapp's CRM is functional, not a marketing suite.

## What we are honestly *not* yet

- **Live bank feeds are not fully wired on `main`.** The provider abstraction (Plaid/GoCardless), smart-matching engine, and rule engine exist in the codebase, but the routes, scheduler, and frontend are not yet connected end-to-end. Today the real, working reconciliation path is CSV import + the ±date/amount auto-matcher. Closing this gap is the next scheduled piece of work.
- **Payroll depth varies by country.** The tax packs compute withholding correctly, but full payroll run/payslip depth is not at parity with a dedicated national payroll bureau in every jurisdiction.
- **Manufacturing is the core make-to-stock loop, not closed-loop MRP.** BOMs, work orders, routings, finite-capacity *visibility*, and job cards are solid; full material requirements planning and subcontracting are not the focus.
- **Reporting and dashboards are functional, not a BI suite.** For deep ad-hoc analytics, a warehouse + BI tool still beats the built-in report builder.
- **Customer portal is intentionally narrow.** It covers the most common self-service use case — tickets — rather than a full B2B account center.

## Who should choose Kapp

Kapp is the right call for a **growing, multi-function SME** — a business that is simultaneously selling (CRM), billing (finance), holding stock or making product (inventory/manufacturing), and employing and training people (HR/recruitment/LMS) — and that is tired of paying for and reconciling five disconnected tools. The value is the *integration and isolation*, not out-featuring the category leader in any single box.

Kapp is **not** the right call if you need only world-class accounting (use Xero), only a best-in-class LMS (use Moodle/Docebo), full closed-loop MRP for a complex factory (use Odoo/ERPNext or SAP), or a dedicated marketing-automation and sales-analytics platform (use HubSpot/Salesforce). We would rather tell you that up front than lose your trust after onboarding.
