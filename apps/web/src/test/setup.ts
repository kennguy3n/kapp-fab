// Vitest global setup. Loaded once per test file (see vitest.config.ts
// `test.setupFiles`). Four responsibilities:
//
//   1. Register @testing-library/jest-dom matchers (toBeInTheDocument,
//      toHaveAttribute, ...) on Vitest's expect. Without this every
//      RTL test would need to import the matchers locally.
//   2. Stand up the MSW (Mock Service Worker) node server so any
//      component / ApiClient call that reaches the real `fetch` is
//      answered by a deterministic handler (src/test/msw/handlers.ts)
//      instead of hitting the network. Handlers are reset between
//      tests so a per-test `server.use(...)` override never leaks.
//   3. Clear React Testing Library's mounted-component cache between
//      tests so a forgotten unmount in one test doesn't leak into
//      the next.
//   4. Polyfill DOM globals that jsdom 25 still ships incomplete:
//      window.matchMedia is referenced by Tailwind's `prefers-color-
//      scheme` queries and by some recharts utilities; ResizeObserver
//      is referenced by the chart container in recharts.
import "@testing-library/jest-dom/vitest";
import { afterAll, afterEach, beforeAll } from "vitest";
import { cleanup } from "@testing-library/react";
import { server } from "./msw/server";

// onUnhandledRequest "error" makes an un-mocked request fail the test
// rather than silently escaping to the network — the determinism
// guarantee the suite relies on. Tests that intentionally bypass MSW
// (vi.stubGlobal("fetch", ...) or vi.mock("../lib/api")) never reach
// this layer, so they are unaffected.
beforeAll(() => {
  server.listen({ onUnhandledRequest: "error" });
});

afterEach(() => {
  cleanup();
  // Drop any per-test handler overrides so each test starts from the
  // default handler set.
  server.resetHandlers();
});

afterAll(() => {
  server.close();
});

if (typeof window !== "undefined") {
  if (!window.matchMedia) {
    window.matchMedia = (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => undefined,
      removeListener: () => undefined,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      dispatchEvent: () => false,
    });
  }
  if (typeof window.ResizeObserver === "undefined") {
    class StubResizeObserver {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    }
    window.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
  }
  // recharts uses getBoundingClientRect on its container; jsdom
  // *does* define Element.prototype.getBoundingClientRect (inherited
  // by HTMLElement) but returns all-zero dimensions, which makes the
  // chart render 0×0 px and emit no SVG nodes. Unconditionally
  // override with a fixed non-zero size so any future recharts test
  // that doesn't mock the Charts component still gets a usable
  // surface. A guarded `if (!proto.method)` shape would never fire
  // here because the method is already inherited.
  HTMLElement.prototype.getBoundingClientRect = function (): DOMRect {
    return {
      x: 0,
      y: 0,
      width: 600,
      height: 400,
      top: 0,
      left: 0,
      right: 600,
      bottom: 400,
      toJSON() {
        return this;
      },
    } as DOMRect;
  };
}
