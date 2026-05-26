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
  },
});
