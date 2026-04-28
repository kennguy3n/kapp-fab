# Module Screenshots

This folder contains full-page screenshots of every kapp-fab module
page rendered against an in-memory mock dataset (no live backend). The
fixtures simulate a fictional company **"Acme Corp"** (tenant slug
`acme`) and are wired in via `VITE_DEMO_MODE=true`.

The mock data layer lives in:

- `apps/web/src/lib/mock-data.ts` — fixture records and KType metadata
- `apps/web/src/lib/mock-api.ts` — `ApiClient`-shaped shim used when
  demo mode is on
- `apps/web/src/lib/api.ts` — selects between the real and mock API
  client based on `import.meta.env.VITE_DEMO_MODE`

## Naming convention

Files are named `NN-module-page.png`, where `NN` is a two-digit module
prefix and `module-page` is a kebab-cased descriptor of the screen:

```
00-login.png
00-setup-wizard.png
01-overview-dashboard.png
02-crm-leads-list.png
02-crm-contacts-list.png
02-crm-deals-kanban.png
03-work-approvals.png
04-projects-gantt.png
05-finance-...png
06-helpdesk-sla-triage.png
07-sales-...png
08-pos-register.png
09-inventory-...png
10-hr-...png
11-lms-...png
12-insights-...png
13-admin-...png
14-portal-...png
15-search-results.png
```

## Regenerating

The full set is captured headlessly by Playwright. From the repo root:

```bash
# install browser binaries (first time only)
npx playwright install chromium

# launches Vite with VITE_DEMO_MODE=true and writes the PNGs
npm run screenshots
```

Behind the scenes this runs `playwright test
scripts/capture-screenshots.spec.ts` against a Vite dev server booted
with `VITE_DEMO_MODE=true`. Each test:

1. Pre-seeds `localStorage.kapp.tenant` and `localStorage.kapp.token`
   so the app shell doesn't redirect to `/login`.
2. Navigates to a route at the standard `1440x900` viewport.
3. Waits for any `Loading…` indicator to disappear (best-effort).
4. Takes a `fullPage: true` screenshot and writes it to this folder.

To capture a single route, pass the test title fragment to Playwright:

```bash
VITE_DEMO_MODE=true npx playwright test scripts/capture-screenshots.spec.ts -g "deals-kanban"
```

## Determinism

Mock UUIDs are derived from a stable hash so identifiers don't churn
between runs. Date-driven UI such as the Gantt chart uses fixed
2026-Q2 dates; everything else (today's attendance, recent activity)
is relative to the current day.
