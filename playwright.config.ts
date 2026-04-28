import { defineConfig } from "@playwright/test";

// Playwright config for the demo screenshot capture workflow.
//
// `npm run screenshots` boots Vite with VITE_DEMO_MODE=true and runs
// `scripts/capture-screenshots.spec.ts`, which navigates each app
// route and writes a full-page PNG into docs/screenshots/.
//
// The webServer command starts the workspace dev server on a fixed
// port. `reuseExistingServer` lets a developer keep their `npm run
// dev` running while iterating on the spec.

export default defineConfig({
  testDir: "scripts",
  testMatch: /capture-screenshots\.spec\.ts/,
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
