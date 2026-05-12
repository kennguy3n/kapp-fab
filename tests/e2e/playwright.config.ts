/**
 * Playwright config for E2E email tests (Mailosaur integration).
 *
 * Run with:
 *   npx playwright test --config tests/e2e/playwright.config.ts
 *
 * Required env vars:
 *   MAILOSAUR_API_KEY    – Mailosaur API key
 *   MAILOSAUR_SERVER_ID  – Mailosaur server (sandbox) ID
 *
 * Optional env vars:
 *   APP_BASE_URL         – web app URL (default: http://localhost:5173)
 *   API_BASE_URL         – backend API URL (default: APP_BASE_URL/api)
 *   AUTH_TOKEN            – bearer token to skip UI login
 *   ADMIN_EMAIL           – admin email (used if AUTH_TOKEN is not set)
 *   ADMIN_PASSWORD        – admin password
 */

import { defineConfig, devices } from "@playwright/test";

const APP_BASE_URL = process.env.APP_BASE_URL ?? "http://localhost:5173";

export default defineConfig({
  testDir: "specs",
  timeout: 120_000,
  expect: { timeout: 10_000 },
  retries: 1,
  workers: 1,
  fullyParallel: false,
  reporter: [
    ["list"],
    ["html", { outputFolder: "../../playwright-report-email", open: "never" }],
  ],
  use: {
    baseURL: APP_BASE_URL,
    viewport: { width: 1440, height: 900 },
    headless: true,
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
