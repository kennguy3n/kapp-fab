/*
 * Kapp service worker.
 *
 * Scope: read-path offline support only. The SW makes the SPA boot and
 * read data offline; it intentionally does NOT touch mutating requests.
 *
 * Responsibilities:
 *   1. App-shell + static-asset caching so the SPA boots offline.
 *      - Navigations: network-first, falling back to the cached shell
 *        ("/") so a cold offline launch still renders the app. A
 *        successful navigation also refreshes the cached shell, so the
 *        offline fallback self-heals after a deploy.
 *      - Hashed static assets (scripts/styles/fonts/images): cache-first
 *        with a background revalidate (stale-while-revalidate) — instant
 *        loads, fresh on the next visit.
 *   2. Read API caching: GET /api/* is network-first with a cached
 *      fallback, and successful responses are stored so the same read
 *      works offline (stale-while-revalidate semantics).
 *
 * Why the SW deliberately leaves mutations alone:
 *   Transparently intercepting POST/PUT/PATCH/DELETE and synthesising a
 *   "queued" response is unsafe — the app's ApiClient treats any 2xx as
 *   success and parses the body as the typed result (e.g. a KRecord), so
 *   a fake 202 silently corrupts callers that read fields off the
 *   response (e.g. `created.id`). It also can't replay typed app
 *   mutations correctly (idempotency keys, dependent IDs). Offline writes
 *   are therefore owned by the app layer via src/lib/offlineQueue.ts,
 *   where each surface (e.g. POSPage) enqueues a typed, idempotent
 *   mutation and drains it on reconnect. Letting a failed mutation reject
 *   naturally is what triggers that app-level handling.
 *
 * This file is served from /sw.js (apps/web/public/) so its scope is the
 * whole origin. It is intentionally dependency-free — service workers run
 * in a separate context and can't import the app bundle.
 */

const CACHE_VERSION = "v1";
const STATIC_CACHE = `kapp-static-${CACHE_VERSION}`;
const API_CACHE = `kapp-api-${CACHE_VERSION}`;
const APP_SHELL = ["/", "/manifest.json", "/icon.svg"];

// --- Lifecycle -------------------------------------------------------
self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(STATIC_CACHE)
      .then((cache) => cache.addAll(APP_SHELL))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys
            .filter((k) => k !== STATIC_CACHE && k !== API_CACHE)
            .map((k) => caches.delete(k)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

// --- Fetch routing ---------------------------------------------------
function isApiRequest(url) {
  return url.pathname.startsWith("/api/");
}

self.addEventListener("fetch", (event) => {
  const { request } = event;
  const url = new URL(request.url);

  // Only same-origin requests are handled; cross-origin (CDNs, third
  // parties) pass straight through to the network.
  if (url.origin !== self.location.origin) return;

  // Mutations are owned by the app layer (see header). Don't intercept:
  // letting them hit the network — and reject naturally when offline —
  // is what lets the app's typed offline queue take over.
  if (request.method !== "GET") return;

  if (request.mode === "navigate") {
    event.respondWith(handleNavigation(request));
    return;
  }

  if (isApiRequest(url)) {
    event.respondWith(handleApiRead(request));
    return;
  }

  event.respondWith(handleStatic(request));
});

// Network-first for document navigations; fall back to the cached app
// shell so an offline cold start still renders the SPA. A successful
// fetch refreshes the cached "/" shell so the offline fallback doesn't
// go stale after a deploy (the bundle's assets are content-hashed, so
// the refreshed shell always references URLs that resolve).
async function handleNavigation(request) {
  const cache = await caches.open(STATIC_CACHE);
  try {
    const res = await fetch(request);
    if (res && res.ok) cache.put("/", res.clone());
    return res;
  } catch {
    const shell = (await cache.match("/")) || (await cache.match(request));
    return shell || Response.error();
  }
}

// Stale-while-revalidate for static assets.
async function handleStatic(request) {
  const cache = await caches.open(STATIC_CACHE);
  const cached = await cache.match(request);
  const network = fetch(request)
    .then((res) => {
      if (res && res.ok) cache.put(request, res.clone());
      return res;
    })
    .catch(() => undefined);
  return cached || (await network) || Response.error();
}

// Network-first for read APIs, caching successes for offline reads.
async function handleApiRead(request) {
  const cache = await caches.open(API_CACHE);
  try {
    const res = await fetch(request);
    if (res && res.ok) cache.put(request, res.clone());
    return res;
  } catch {
    const cached = await cache.match(request);
    if (cached) return cached;
    return new Response(
      JSON.stringify({ error: "offline", message: "No cached response." }),
      { status: 503, headers: { "Content-Type": "application/json" } },
    );
  }
}
