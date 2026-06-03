import { defineConfig, devices } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Repo root, regardless of where `playwright test` is invoked from —
// the webServer command runs npm workspace scripts that only resolve
// from the monorepo root. (This config is loaded as an ES module, so
// __dirname isn't available — derive it from import.meta.url.)
const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, "../../..");

// E2E config for the apps/web SPA. Kept separate from the repo-root
// playwright.config.ts (which drives the demo-mode screenshot capture)
// and playwright.rtl.config.ts (the RTL regression) so the three
// Playwright runs target disjoint specs without colliding on
// testDir/testMatch. CI runs this via the dedicated `web-e2e` job.
//
// The web server is Vite's dev server WITHOUT VITE_DEMO_MODE — the
// specs want the real ApiClient (plain fetch to /api/v1) so they can
// deterministically intercept those requests via page.route (see
// mock-api.ts). Demo mode would swap in the in-memory shim and defeat
// the interception.
const PORT = 4173;
const BASE_URL = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.spec\.ts/,
  // Fail fast on an accidentally committed `.only`.
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  // `list` for readable console output; `html` so a CI failure ships a
  // browsable report (with the on-first-retry trace) as an artifact.
  // Both output paths are pinned explicitly below so they resolve
  // deterministically relative to THIS config's directory
  // (apps/web/playwright/) regardless of the Playwright version's
  // default-resolution heuristics — the CI `upload report` step
  // references these exact paths.
  reporter: [["list"], ["html", { open: "never", outputFolder: "playwright-report" }]],
  // Per-test artifacts (traces, screenshots). Pinned for the same
  // reason as the report folder above.
  outputDir: "test-results",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: BASE_URL,
    headless: true,
    viewport: { width: 1280, height: 800 },
    trace: "on-first-retry",
  },
  // Chromium-only matrix: the suite asserts framework-agnostic DOM
  // behaviour, so a single engine gives deterministic, fast signal.
  // Add firefox/webkit projects here if cross-engine coverage is
  // needed (the CI job would then need those browsers installed).
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
  webServer: {
    command:
      "npm run dev --workspace=apps/web -- --host 127.0.0.1 --port 4173 --strictPort",
    url: BASE_URL,
    cwd: REPO_ROOT,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
