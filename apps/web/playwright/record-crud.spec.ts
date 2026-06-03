import { test, expect } from "@playwright/test";
import { installApiMock, seedSession } from "./mock-api";

// Record CRUD: list existing crm.deal records, create a new one
// through the form, and verify it appears in the refreshed list. The
// mock-api keeps created records in memory so the post-create list
// reflects the write.

test.describe("record CRUD", () => {
  test.beforeEach(async ({ page }) => {
    await installApiMock(page);
    await seedSession(page);
  });

  test("lists the seeded records", async ({ page }) => {
    await page.goto("/records/crm.deal");
    await expect(page.getByRole("heading", { name: "crm.deal" })).toBeVisible();
    await expect(page.getByText("Acme renewal")).toBeVisible();
    await expect(page.getByText("Globex expansion")).toBeVisible();
  });

  test("creates a new record through the form and shows it in the list", async ({
    page,
  }) => {
    await page.goto("/records/crm.deal");
    await expect(page.getByRole("heading", { name: "crm.deal" })).toBeVisible();

    await page.getByRole("button", { name: "New" }).click();
    await expect(page.getByRole("heading", { name: "New crm.deal" })).toBeVisible();

    // Scope to the record form (the one with the Save button) — the
    // app shell also renders a header global-search <form>, so an
    // unscoped "form input" would grab the search box instead.
    const recordForm = page.locator("form:has(button)");
    // First field in the KType form is the required "title" string.
    await recordForm.locator("input").first().fill("Initech rollout");
    await page.getByRole("button", { name: "Save" }).click();

    // createRecord -> navigate back to the list; the new record is now
    // returned by the (stateful) mock list endpoint.
    await expect(page.getByRole("heading", { name: "crm.deal" })).toBeVisible();
    await expect(page.getByText("Initech rollout")).toBeVisible();
  });
});
