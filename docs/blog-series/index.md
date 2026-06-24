# Kapp in Action: A Live Walkthrough of the Seeded Demo

This is a product-facing showcase series. Every screenshot below is a full-page capture of the real Kapp UI running in the browser against the built-in demo tenant. There is no Photoshop, no canned Figma mock, and no staging backend required: the same `npm run screenshots` command that generated these images launches Vite in `VITE_DEMO_MODE=true`, seeds the fictional **Acme Corp** tenant, and drives the application through every route.

The demo tenant is a single USD business with a deliberately broad footprint — CRM, finance, sales, procurement, inventory, manufacturing, HR, LMS, projects, helpdesk, approvals, insights, and admin. That breadth lets us show how a single platform, a single sign-in, and a single underlying data model serve many different jobs-to-be-done without the integration seams that normally force SMEs to stitch five tools together.

## What you are looking at

- The app shell, navigation, and every page are the current React frontend from `apps/web`.
- Numbers, lists, and charts come from the deterministic mock fixtures in `apps/web/src/lib/mock-data.ts`.
- Image files are stored in `docs/screenshots/` and are regenerated automatically by `scripts/capture-screenshots.spec.ts`.

## The posts

1. [Getting Started: Login, Setup, and the Overview Dashboard](./01-getting-started.md) — sign-in, the tenant setup wizard, the live dashboard, and universal search.
2. [CRM & Sales: Leads, Contacts, Deals, and Approvals](./02-crm-sales-approvals.md) — the record surfaces, the deal pipeline, sales orders, price lists, purchase orders, and the POS register.
3. [Finance & Reporting: Chart of Accounts to Trial Balance](./03-finance-reporting.md) — the general ledger, journal entries, AR subledger, bank reconciliation, exchange rates, cost centers, and the report builder.
4. [Operations & Manufacturing: Helpdesk, Projects, Inventory, and the Shop Floor](./04-operations-manufacturing.md) — SLA triage, project Gantt, live stock levels, inventory valuation, BOMs, work orders, routings, capacity planning, and job cards.
5. [People, Recruitment & Learning: HR, Org Chart, Payroll, Recruitment, and LMS](./05-people-learning.md) — the employee directory, org chart, payroll, shift calendar, hiring pipeline, courses, and learner progress.
6. [Insights, Administration, and Customer Portal](./06-insights-admin-portal.md) — the visual query builder, dashboards, tenant management, audit log, webhooks, retention, and the customer portal.
7. [Honest Competitive Assessment](./07-competitive-analysis.md) — where Kapp wins, where Xero, QuickBooks, ERPNext, Odoo, SAP Business One, NetSuite, TalentLMS, and Moodle still lead, and which buyer should choose which path.

## A note on honesty

The demo is designed to look like a real business, but it is still a demo. This series does not pretend Kapp has deeper payroll than a national bureau, a more polished bank feed than Xero, or more mature MRP than Odoo. Where a competitor is genuinely better for a given job, the last post says so.
