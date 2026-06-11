# Kapp in the Real World: One Platform, Five Businesses, Five Countries

This is a business-facing blog series. Instead of listing features, we provisioned
five real tenants — different industries, different countries, different currencies
and statutory rules — bootstrapped them with realistic data, and then drove the
actual product end-to-end. Every screenshot below is the live application running
against a real Postgres database with multi-tenant row-level security, not a mockup.

The goal is simple: show how a single SME platform handles the *jobs to be done*
that a coffee roaster in Singapore, a consultancy in London, a distributor in Dubai,
an outdoor retailer in the US, and a metal-fab shop in Mexico each care about — and
to be honest about where we lead and where established competitors are still ahead.

## The five businesses

| Tenant | Country | Currency | Industry | Persona & job-to-be-done |
|---|---|---|---|---|
| **Lion City Coffee** | 🇸🇬 Singapore | SGD | Coffee & Hospitality | Finance lead closing the books, tracing AR to the GL, watching the deal pipeline |
| **Thistle & Oak** | 🇬🇧 United Kingdom | GBP | Professional Services | HR/People lead running recruitment and onboarding training |
| **Falcon Trading** | 🇦🇪 UAE | AED | Wholesale & Distribution | Operations lead valuing stock across two warehouses |
| **Cascade Outfitters** | 🇺🇸 United States | USD | Retail / E-commerce | Sales lead managing a deal pipeline and cash position |
| **Talleres del Bajío** | 🇲🇽 Mexico | MXN | Manufacturing | Plant manager planning production capacity, SAT-compliant books |

All five run on the **same binary and the same database**. The only differences are
the tenant's feature flags, chart-of-accounts template, currency, locale, and the
statutory tax pack selected at setup — all isolated per `tenant_id` by Postgres RLS.

## The posts

1. [Finance for a Singapore coffee roaster](./01-finance-singapore.md) — invoicing, the trial balance, and tracing AR to the general ledger.
2. [Recruitment for a UK consultancy](./02-recruitment-uk.md) — a hiring pipeline from application to offer, on a drag-to-advance kanban.
3. [Training & LMS for a UK consultancy](./03-lms-uk.md) — learning paths, instructor analytics, and completion tracking.
4. [Inventory for a Dubai distributor](./04-inventory-uae.md) — live stock levels and valuation across two warehouses, in AED.
5. [CRM for a US outdoor retailer](./05-crm-usa.md) — a deal pipeline and at-a-glance cash KPIs in USD.
6. [Manufacturing for a Mexican fab shop](./06-manufacturing-mexico.md) — BOMs, work orders, finite-capacity planning, and SAT-compliant statutory books in MXN.
7. [Honest competitive assessment](./07-competitive-analysis.md) — Kapp vs Xero, QuickBooks, ERPNext, Odoo, SAP Business One, NetSuite, TalentLMS, and Moodle. Where we win, where we don't.

## How the evidence was produced

- Five tenants were provisioned through the normal setup wizard, each with its own
  country, currency, locale, and CoA template.
- Demo data (customers, suppliers, GL postings, deals, inventory ledgers, employees,
  recruitment pipelines, LMS enrolments, BOMs and work orders) was loaded the same way
  a customer would — through the public API with a tenant-scoped JWT.
- Each journey was then driven in the browser as the tenant owner. Screenshots are
  full application views with real numbers.

> A note on honesty: this series is meant to be credible to a buyer who will check
> our claims. Where a competitor is genuinely better for a given job, post 7 says so.
