import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

// Vitest config for apps/web — pinned to jsdom (so React components
// can mount without a real browser), the same @kapp/{ui,client} path
// aliases the production Vite build uses, and a single setup file
// that wires @testing-library/jest-dom matchers into Vitest's
// expect().
//
// Tests live alongside their subjects under apps/web/src/**/*.test.tsx
// so a developer renaming a page also moves its test in the same
// commit. CI runs `npm run test -w @kapp/web` (a thin wrapper around
// `vitest run`) from .github/workflows/ci.yml; the same command is
// the entry point developers run locally.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      // Mirror vite.config.ts so a test file `import "@kapp/ui"`
      // resolves identically in jsdom and the production bundle.
      "@kapp/ui": path.resolve(__dirname, "../../packages/ui/src"),
      "@kapp/client": path.resolve(__dirname, "../../packages/client/src"),
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
    // Restrict the discovery glob so we don't accidentally pick up
    // spec.ts files from scripts/ (Playwright suites at the
    // monorepo root use the *.spec.ts convention).
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
    // Each test file is isolated by default; we don't need parallel
    // file workers given the small unit-test surface, and "forks"
    // gives more stable behaviour for code that uses dynamic import.
    pool: "forks",
    coverage: {
      // v8 (native) coverage — no instrumentation transform, so it
      // can't drift from what actually executed. Enabled unconditionally
      // (not just behind the --coverage flag) so the existing CI
      // `web-tests` job — which runs `npm run test -w @kapp/web`
      // (a plain `vitest run`) — enforces the thresholds below without
      // needing a separate coverage invocation.
      enabled: true,
      provider: "v8",
      reporter: ["text-summary", "html", "lcov"],
      reportsDirectory: "./coverage",
      // Measure only first-party source the unit suite is responsible
      // for. Test files, the test harness itself, type-only barrels,
      // and the demo-mode fixtures (mock-api/mock-data, exercised by
      // the Playwright E2E suite rather than Vitest) are excluded so
      // the percentages reflect real component/page coverage.
      include: ["src/**/*.{ts,tsx}"],
      exclude: [
        // `test.include` matches both *.test.* and *.spec.*, so exclude
        // both from coverage — otherwise a future src/**/*.spec.ts would
        // run as a test and count its own lines toward the metrics.
        "src/**/*.test.{ts,tsx}",
        "src/**/*.spec.{ts,tsx}",
        "src/test/**",
        "src/main.tsx",
        "src/vite-env.d.ts",
        "src/lib/mock-api.ts",
        "src/lib/mock-data.ts",
      ],
      // Global floors, set a few points below the suite's current
      // measured coverage (statements/lines ~29.7%, branches ~71.7%,
      // functions ~52.2%) so an unrelated edit that drops a covered
      // line trips the gate, without making the threshold so tight
      // that day-to-day churn fails CI. Statement/line coverage is
      // modest because the unit suite deliberately prioritises breadth
      // of meaningful page/component behaviour over blanketing every
      // large page module; the Playwright E2E suite covers the
      // remaining end-to-end flows. Raise these as coverage grows;
      // never lower them to make a red build pass.
      thresholds: {
        statements: 28,
        branches: 68,
        functions: 50,
        lines: 28,
      },
    },
  },
});
