import { defineConfig } from "@playwright/test";

// Playwright config for the RTL flip regression test added in
// PR-6. Kept separate from playwright.config.ts (which drives
// the demo screenshot capture) so the two runs can target
// different specs without colliding on testMatch.
//
// `npm run test:e2e:rtl` boots Vite in demo mode and runs
// scripts/rtl-flip.spec.ts which asserts the LocaleProvider's
// <html dir>/<html lang> stamp + the sidebar's logical-border
// flip for Arabic.

export default defineConfig({
  testDir: "scripts",
  testMatch: /rtl-flip\.spec\.ts/,
  timeout: 60_000,
  retries: 0,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: "http://localhost:5173",
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 1,
    headless: true,
  },
  webServer: {
    command:
      "VITE_DEMO_MODE=true npm run dev --workspace=apps/web -- --host 127.0.0.1 --port 5173 --strictPort",
    url: "http://localhost:5173",
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
    env: { VITE_DEMO_MODE: "true" },
  },
});
