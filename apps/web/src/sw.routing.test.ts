// Behavioural tests for the service worker's fetch routing.
//
// sw.js lives in public/ and is never imported by the app bundle (it runs
// in a separate worker context), so we load its source and evaluate it
// with a controlled `self`/`caches`/`fetch` so we can dispatch synthetic
// fetch events and assert which requests it intercepts.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";

// Resolve public/sw.js robustly regardless of the vitest working dir
// (repo root vs. apps/web): walk up from cwd looking for the file.
function findSwSource(): string {
  const candidates = [
    path.resolve(process.cwd(), "apps/web/public/sw.js"),
    path.resolve(process.cwd(), "public/sw.js"),
    path.resolve(process.cwd(), "../public/sw.js"),
  ];
  const hit = candidates.find((p) => existsSync(p));
  if (!hit) throw new Error("could not locate public/sw.js for SW routing test");
  return readFileSync(hit, "utf8");
}

const swSource = findSwSource();

const ORIGIN = "https://app.example.com";

interface FetchEventLike {
  request: { url: string; method: string; mode: string };
  respondWith: ReturnType<typeof vi.fn>;
  waitUntil: ReturnType<typeof vi.fn>;
}

function loadSW() {
  const listeners: Record<string, (e: unknown) => void> = {};
  const self = {
    location: { origin: ORIGIN },
    addEventListener: (type: string, cb: (e: unknown) => void) => {
      listeners[type] = cb;
    },
    skipWaiting: vi.fn(),
    clients: { claim: vi.fn() },
  };
  const cacheObj = {
    match: vi.fn(async () => undefined),
    put: vi.fn(async () => undefined),
    addAll: vi.fn(async () => undefined),
  };
  const caches = {
    open: vi.fn(async () => cacheObj),
    keys: vi.fn(async () => []),
    delete: vi.fn(async () => true),
    match: vi.fn(async () => undefined),
  };
  const fetchMock = vi.fn(async () => new Response("ok", { status: 200 }));
  // sw.js references self/caches/fetch/URL/Response as free globals; pass
  // them in as params so they shadow the real globals during evaluation.
  const factory = new Function(
    "self",
    "caches",
    "fetch",
    "URL",
    "Response",
    swSource,
  );
  factory(self, caches, fetchMock, URL, Response);
  return { listeners, fetchMock };
}

function dispatchFetch(
  listeners: Record<string, (e: unknown) => void>,
  req: { path: string; method?: string; mode?: string },
): FetchEventLike {
  const event: FetchEventLike = {
    request: {
      url: `${ORIGIN}${req.path}`,
      method: req.method ?? "GET",
      mode: req.mode ?? "cors",
    },
    respondWith: vi.fn(),
    waitUntil: vi.fn(),
  };
  listeners.fetch(event);
  return event;
}

describe("service worker fetch routing", () => {
  let listeners: Record<string, (e: unknown) => void>;

  beforeEach(() => {
    ({ listeners } = loadSW());
  });

  // Regression: the SSO entry point is a real document navigation to an
  // API route (<a href="/api/v1/auth/kchat/start">) that 302s to an
  // external OAuth provider. If the SW intercepts it, the navigation-mode
  // fetch hits a cross-origin redirect, errors, and the cached shell is
  // served — breaking login. The SW must leave API navigations alone.
  it("does NOT intercept full-page navigations to /api/* (SSO redirect)", () => {
    const event = dispatchFetch(listeners, {
      path: "/api/v1/auth/kchat/start",
      mode: "navigate",
    });
    expect(event.respondWith).not.toHaveBeenCalled();
  });

  it("intercepts in-app document navigations (network-first shell)", () => {
    const event = dispatchFetch(listeners, {
      path: "/dashboard",
      mode: "navigate",
    });
    expect(event.respondWith).toHaveBeenCalledTimes(1);
  });

  it("intercepts GET /api/* reads issued by the app (not navigations)", () => {
    const event = dispatchFetch(listeners, {
      path: "/api/v1/records",
      mode: "cors",
    });
    expect(event.respondWith).toHaveBeenCalledTimes(1);
  });

  it("does NOT intercept API mutations (owned by the app-level queue)", () => {
    const event = dispatchFetch(listeners, {
      path: "/api/v1/records",
      method: "POST",
      mode: "cors",
    });
    expect(event.respondWith).not.toHaveBeenCalled();
  });

  it("intercepts static asset GETs (cache-first)", () => {
    const event = dispatchFetch(listeners, {
      path: "/assets/index-abc123.js",
      mode: "cors",
    });
    expect(event.respondWith).toHaveBeenCalledTimes(1);
  });
});
