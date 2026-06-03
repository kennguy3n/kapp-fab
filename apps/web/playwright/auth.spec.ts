import { test, expect } from "@playwright/test";
import { installApiMock, seedSession, TENANT_ID } from "./mock-api";

// Auth flow: login via the KChat SSO mock, session persistence across
// a reload, and "logout". The app ships no logout control, so logout
// is modelled as clearing the persisted session keys (what a logout
// button would do) and verifying the login surface is reachable again.

test.describe("authentication", () => {
  test("logs in via the mocked KChat SSO exchange and lands on the dashboard", async ({
    page,
  }) => {
    await installApiMock(page);
    await page.goto("/login");

    await page.getByRole("heading", { name: "Sign in" }).waitFor();
    await page.locator("label", { hasText: "KChat auth code" }).locator("input").fill("kchat-code");
    await page.getByRole("button", { name: "Continue" }).click();

    // Redirects to the dashboard shell.
    await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
    // Tokens from the SSO response are persisted for session continuity.
    expect(await page.evaluate(() => localStorage.getItem("kapp.token"))).toBe(
      "e2e-access-token",
    );
    expect(await page.evaluate(() => localStorage.getItem("kapp.tenant"))).toBe(
      TENANT_ID,
    );
  });

  test("persists the session across a full page reload", async ({ page }) => {
    await installApiMock(page);
    await seedSession(page);

    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();

    await page.reload();
    // Still authenticated (no bounce to /login) after the reload.
    await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
    expect(page.url()).not.toContain("/login");
  });

  test("clearing the session (logout) returns the user to the login form", async ({
    page,
  }) => {
    await installApiMock(page);
    // Authenticate through the real SSO flow rather than seedSession()
    // here: seedSession installs a persistent init script that re-seeds
    // the token on every navigation, which would mask the logout.
    await page.goto("/login");
    await page.locator("label", { hasText: "KChat auth code" }).locator("input").fill("kchat-code");
    await page.getByRole("button", { name: "Continue" }).click();
    await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();

    // Simulate logout: drop the persisted credentials, as a logout
    // control would, then navigate to the login surface.
    await page.evaluate(() => {
      localStorage.removeItem("kapp.token");
      localStorage.removeItem("kapp.refresh");
      localStorage.removeItem("kapp.expires_at");
    });
    await page.goto("/login");

    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    expect(await page.evaluate(() => localStorage.getItem("kapp.token"))).toBeNull();
  });

  test("shows an error and stays on the form when the SSO exchange fails", async ({
    page,
  }) => {
    await installApiMock(page);
    // Override the SSO route with a 401 for this test only.
    await page.route("**/api/v1/auth/sso", (route) =>
      route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "bad code" }),
      }),
    );

    await page.goto("/login");
    await page.locator("label", { hasText: "KChat auth code" }).locator("input").fill("bad");
    await page.getByRole("button", { name: "Continue" }).click();

    await expect(page.getByText(/SSO failed \(401\)/)).toBeVisible();
    expect(page.url()).toContain("/login");
  });
});
