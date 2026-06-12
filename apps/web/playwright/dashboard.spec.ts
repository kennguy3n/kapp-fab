import { test, expect } from "@playwright/test";
import { installApiMock, seedSession } from "./mock-api";

// Dashboard: the KPI tiles render from the mocked
// /api/v1/dashboard/summary payload.

test.describe("dashboard", () => {
  test.beforeEach(async ({ page }) => {
    await installApiMock(page);
    await seedSession(page);
  });

  test("renders KPI tiles backed by the mock summary", async ({ page }) => {
    await page.goto("/");

    await expect(
      page.getByRole("heading", { name: /Good (morning|afternoon|evening)/ }),
    ).toBeVisible();
    // Tile labels from DashboardPage.
    await expect(page.getByText("Open deals")).toBeVisible();
    await expect(page.getByText("Outstanding AR")).toBeVisible();
    await expect(page.getByText("Pending approvals")).toBeVisible();
    // The "Open deals" count from the mock payload (7).
    await expect(page.getByText("7", { exact: true })).toBeVisible();
  });

  test("each tile links to its underlying worklist", async ({ page }) => {
    await page.goto("/");
    await expect(
      page.getByRole("heading", { name: /Good (morning|afternoon|evening)/ }),
    ).toBeVisible();

    // The "Open deals" tile deep-links to the crm.deal list.
    const dealLink = page.locator('a[href="/records/crm.deal"]');
    await expect(dealLink.first()).toBeVisible();
    await expect(page.locator('a[href="/approvals"]').first()).toBeVisible();
  });
});
