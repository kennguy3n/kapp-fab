/**
 * Shared Playwright fixtures for authenticated E2E email tests.
 *
 * Extends the base `test` object with:
 *   - `adminPage`  – a Page already logged in as an admin user
 *   - `apiContext` – an APIRequestContext pre-configured with auth headers
 *
 * The fixture reads credentials from env vars so CI can inject them
 * without hard-coding secrets:
 *
 *   APP_BASE_URL   – e.g. https://staging.kinshield.example.com
 *   ADMIN_EMAIL    – admin account email
 *   ADMIN_PASSWORD – admin account password
 *   API_BASE_URL   – backend API root (defaults to APP_BASE_URL + "/api")
 *   AUTH_TOKEN      – optional pre-generated bearer token (skips login flow)
 */

import { test as base, type Page, type APIRequestContext } from "@playwright/test";

const APP_BASE_URL = process.env.APP_BASE_URL ?? "http://localhost:5173";
const API_BASE_URL = process.env.API_BASE_URL ?? `${APP_BASE_URL}/api`;
const ADMIN_EMAIL = process.env.ADMIN_EMAIL ?? "";
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD ?? "";
const AUTH_TOKEN = process.env.AUTH_TOKEN ?? "";

interface EmailTestFixtures {
  /** A browser page already authenticated as admin. */
  adminPage: Page;
  /** An API request context with auth headers. */
  apiContext: APIRequestContext;
}

export const test = base.extend<EmailTestFixtures>({
  adminPage: async ({ page }, use) => {
    if (AUTH_TOKEN) {
      // Inject token into localStorage so the app treats us as logged in.
      await page.goto(APP_BASE_URL);
      await page.evaluate(
        ({ token }) => {
          localStorage.setItem("kapp.token", token);
        },
        { token: AUTH_TOKEN }
      );
      await page.reload();
    } else if (ADMIN_EMAIL && ADMIN_PASSWORD) {
      // Perform UI login.
      await page.goto(`${APP_BASE_URL}/login`);
      await page.getByLabel(/email/i).fill(ADMIN_EMAIL);
      await page.getByLabel(/password/i).fill(ADMIN_PASSWORD);
      await page.getByRole("button", { name: /sign in|log in/i }).click();
      await page.waitForURL("**/", { timeout: 15_000 });
    } else {
      throw new Error(
        "Either AUTH_TOKEN or ADMIN_EMAIL + ADMIN_PASSWORD must be set."
      );
    }

    await use(page);
  },

  apiContext: async ({ playwright }, use) => {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
    };

    if (AUTH_TOKEN) {
      headers["Authorization"] = `Bearer ${AUTH_TOKEN}`;
    } else if (ADMIN_EMAIL && ADMIN_PASSWORD) {
      // Obtain a bearer token via the login API so every apiContext
      // call is authenticated, even when AUTH_TOKEN is not set.
      const tempCtx = await playwright.request.newContext({
        baseURL: API_BASE_URL,
      });
      const loginResp = await tempCtx.post("/v1/login", {
        data: { email: ADMIN_EMAIL, password: ADMIN_PASSWORD },
      });
      if (loginResp.ok()) {
        const body = await loginResp.json();
        const token = body.token ?? body.access_token ?? "";
        if (token) {
          headers["Authorization"] = `Bearer ${token}`;
        }
      }
      await tempCtx.dispose();
    }

    const ctx = await playwright.request.newContext({
      baseURL: API_BASE_URL,
      extraHTTPHeaders: headers,
    });

    await use(ctx);
    await ctx.dispose();
  },
});

export { expect } from "@playwright/test";
export { APP_BASE_URL, API_BASE_URL };
