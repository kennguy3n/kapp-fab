import { test, expect } from "@playwright/test";
import { installApiMock, seedSession } from "./mock-api";

// Insights: build a query in the visual builder, save it, run it, and
// confirm the result renders (default viz is the table, so the run
// result's columns/rows appear).

test.describe("insights query builder", () => {
  test.beforeEach(async ({ page }) => {
    await installApiMock(page);
    await seedSession(page);
  });

  test("saves and runs a query, rendering the result table", async ({ page }) => {
    await page.goto("/insights/queries");
    await expect(
      page.getByRole("heading", { name: "Query Builder" }),
    ).toBeVisible();

    // Name + save (visual mode only requires a name).
    await page.getByPlaceholder("Query name").fill("Deals by stage");
    await page.getByRole("button", { name: "Save", exact: true }).click();

    // After a successful create the Run action becomes available.
    const runButton = page.getByRole("button", { name: "Run", exact: true });
    await expect(runButton).toBeVisible();
    await runButton.click();

    // The mock run result is a stage/count table. The result table
    // humanizes column keys for display (stage -> "Stage",
    // count -> "Count"), so assert the humanized columnheaders.
    // Scope to columnheader roles since these substrings also appear
    // in sidebar nav + the source dropdown.
    await expect(page.getByRole("table")).toBeVisible();
    await expect(
      page.getByRole("columnheader", { name: "Stage", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("columnheader", { name: "Count", exact: true }),
    ).toBeVisible();
  });

  test("blocks running until the query has been saved", async ({ page }) => {
    await page.goto("/insights/queries");
    await expect(
      page.getByRole("heading", { name: "Query Builder" }),
    ).toBeVisible();

    // Before saving, the Run button isn't rendered (it appears only
    // once a query id exists). Saving without a name surfaces the
    // validation error instead.
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.getByText("query name required")).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Run", exact: true }),
    ).toHaveCount(0);
  });
});
