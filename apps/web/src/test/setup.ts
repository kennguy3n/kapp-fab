// Vitest global setup. Loaded once per test file (see vitest.config.ts
// `test.setupFiles`). Three responsibilities:
//
//   1. Register @testing-library/jest-dom matchers (toBeInTheDocument,
//      toHaveAttribute, ...) on Vitest's expect. Without this every
//      RTL test would need to import the matchers locally.
//   2. Clear React Testing Library's mounted-component cache between
//      tests so a forgotten unmount in one test doesn't leak into
//      the next.
//   3. Polyfill DOM globals that jsdom 25 still ships incomplete:
//      window.matchMedia is referenced by Tailwind's `prefers-color-
//      scheme` queries and by some recharts utilities; ResizeObserver
//      is referenced by the chart container in recharts.
import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
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
  // returns zeros for every dimension which makes the chart render
  // 0×0 px. Stub a non-zero size so the SVG nodes are created.
  if (!HTMLElement.prototype.getBoundingClientRect) {
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
}
