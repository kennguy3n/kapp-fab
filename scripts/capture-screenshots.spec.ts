import { test, expect } from "@playwright/test";
import path from "node:path";

// Each entry maps `[route, output filename]`. The route is appended
// to the dev server `baseURL` from playwright.config.ts. The
// filename is written under docs/screenshots/.
const ROUTES: Array<[string, string]> = [
  ["/login", "00-login.png"],
  ["/setup/00000000-0000-4000-8000-000000001001", "00-setup-wizard.png"],
  ["/", "01-overview-dashboard.png"],
  ["/records/crm.lead", "02-crm-leads-list.png"],
  ["/records/crm.contact", "02-crm-contacts-list.png"],
  ["/records/crm.deal", "02-crm-deals-kanban.png"],
  ["/approvals", "03-work-approvals.png"],
  ["/projects/gantt", "04-projects-gantt.png"],
  ["/finance/accounts", "05-finance-chart-of-accounts.png"],
  ["/finance/journal", "05-finance-journal-entries.png"],
  ["/finance/reports/trial-balance", "05-finance-trial-balance.png"],
  ["/finance/reports/income-statement", "05-finance-income-statement.png"],
  ["/finance/ar-subledger", "05-finance-ar-subledger.png"],
  ["/finance/bank-reconciliation", "05-finance-bank-reconciliation.png"],
  ["/finance/exchange-rates", "05-finance-exchange-rates.png"],
  ["/finance/cost-centers", "05-finance-cost-centers.png"],
  ["/reports", "05-finance-report-builder.png"],
  ["/helpdesk", "06-helpdesk-sla-triage.png"],
  ["/sales/orders", "07-sales-orders.png"],
  ["/sales/price-lists", "07-sales-price-lists.png"],
  ["/procurement/purchase-orders", "07-sales-purchase-orders.png"],
  ["/pos", "08-pos-register.png"],
  ["/inventory/stock-levels", "09-inventory-stock-levels.png"],
  ["/inventory/reports/valuation", "09-inventory-valuation.png"],
  ["/records/hr.employee", "10-hr-employees-list.png"],
  ["/hr/org-chart", "10-hr-org-chart.png"],
  ["/hr/payroll", "10-hr-payroll.png"],
  ["/hr/shifts", "10-hr-shift-calendar.png"],
  ["/records/lms.course", "11-lms-courses.png"],
  ["/lms/progress", "11-lms-learner-progress.png"],
  ["/insights/queries", "12-insights-query-builder.png"],
  ["/insights/dashboards", "12-insights-dashboard.png"],
  ["/admin/tenants", "13-admin-tenants.png"],
  ["/admin/features", "13-admin-features.png"],
  ["/admin/audit", "13-admin-audit-log.png"],
  ["/admin/usage", "13-admin-usage.png"],
  ["/admin/webhooks", "13-admin-webhooks.png"],
  ["/admin/retention", "13-admin-retention.png"],
  ["/portal/acme", "14-portal-login.png"],
  ["/portal/acme/tickets", "14-portal-ticket-list.png"],
  ["/search?q=invoice", "15-search-results.png"],
];

const OUT_DIR = path.resolve(__dirname, "..", "docs", "screenshots");

const DEMO_TENANT_ID = "00000000-0000-0000-0000-000000000001";
const DEMO_TOKEN =
  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZW1vQGFjbWUuZXhhbXBsZSIsInRlbmFudF9pZCI6Ijk5OTk5OTk5LTk5OTktOTk5OS05OTk5LTk5OTk5OTk5OTk5OSJ9.demo-signature";

test.describe("capture demo screenshots", () => {
  test.beforeEach(async ({ context }) => {
    // Pre-seed demo localStorage on every page in the context so the
    // app shell skips the login redirect on first paint.
    await context.addInitScript(
      ({ tenantId, token }) => {
        try {
          localStorage.setItem("kapp.tenant", tenantId);
          localStorage.setItem("kapp.token", token);
        } catch {
          // localStorage may not be available on every page (e.g. about:blank).
        }
      },
      { tenantId: DEMO_TENANT_ID, token: DEMO_TOKEN }
    );
  });

  for (const [route, filename] of ROUTES) {
    test(`route ${route} → ${filename}`, async ({ page }, testInfo) => {
      // Use `domcontentloaded` rather than `networkidle` because the
      // mock layer keeps a small delay running on every API call;
      // waiting for "idle" can race in dev mode.
      await page.goto(route, { waitUntil: "domcontentloaded" });

      // Give React Query queries time to resolve and the loading
      // text to clear. The app uses literal "Loading…" / "Loading"
      // strings in many places; we wait until none are visible.
      await page
        .waitForFunction(
          () => {
            const text = document.body?.innerText ?? "";
            return !/Loading[…\.]/.test(text);
          },
          undefined,
          { timeout: 8_000 }
        )
        .catch(() => {
          // Don't fail the screenshot if the loading text never
          // disappears — capture what's on screen so we at least
          // see the populated state.
        });

      // Per-route adjustments: a couple of pages need a click before
      // their populated state is visible (e.g. selecting a saved
      // dashboard / webhook from a sidebar). Best-effort — the page
      // is still captured even if the click doesn't find the target.
      if (route === "/insights/dashboards") {
        await page
          .getByRole("button", { name: /Executive Overview/i })
          .first()
          .click({ timeout: 2_000 })
          .catch(() => undefined);
        await page.waitForTimeout(1500);
      } else if (route === "/admin/webhooks") {
        // Scroll to top so the registration form + table both land
        // in the viewport on the first paint.
        await page.evaluate(() => window.scrollTo(0, 0));
      }

      // Small visual settle — recharts mount in the next frame and
      // the org-chart tree expands lazily.
      await page.waitForTimeout(900);

      const out = path.join(OUT_DIR, filename);
      await page.screenshot({ path: out, fullPage: true });
      await testInfo.attach(filename, { path: out, contentType: "image/png" });
      expect(true).toBe(true);
    });
  }
});
