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
 *   3. Per-identity isolation of the read cache: cached GET /api/*
 *      responses are keyed only by URL (Cache API default) and carry no
 *      auth context, so on a shared device (e.g. a POS terminal) the
 *      next user must not be served the previous user's cached reads.
 *      The app posts its current identity (a non-reversible hash of the
 *      active tenant + token) via postMessage; when it changes, the SW
 *      drops the API cache. There is no logout flow in the app today, so
 *      this identity-change signal — sent on every load — is what bounds
 *      one user's cached data to that user.
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
// Small bookkeeping cache (survives SW restarts) holding the last
// identity the API cache was populated for. Kept out of the activate
// cleanup below so the identity persists across deploys.
const META_CACHE = `kapp-meta-${CACHE_VERSION}`;
const IDENTITY_KEY = "/__kapp_identity__";
const APP_SHELL = ["/", "/manifest.json", "/icon.svg"];
const KNOWN_CACHES = [STATIC_CACHE, API_CACHE, META_CACHE];

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
            .filter((k) => !KNOWN_CACHES.includes(k))
            .map((k) => caches.delete(k)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

// --- Identity isolation ---------------------------------------------
// The app posts { type: "kapp:identity", id } whenever it loads. If the
// identity differs from the one the API cache was last populated for, we
// drop the API cache so a different user on a shared device can't read
// the previous user's cached responses. The identity is an opaque hash
// computed app-side (see src/main.tsx) — the SW never sees the raw
// token. The static/app-shell cache holds only public, non-tenant assets
// so it is intentionally left untouched.
async function applyIdentity(id) {
  const meta = await caches.open(META_CACHE);
  const prevRes = await meta.match(IDENTITY_KEY);
  const prev = prevRes ? await prevRes.text() : null;
  if (prev === id) return;
  await caches.delete(API_CACHE);
  await meta.put(IDENTITY_KEY, new Response(id));
}

self.addEventListener("message", (event) => {
  const data = event.data;
  if (data && data.type === "kapp:identity") {
    event.waitUntil(applyIdentity(String(data.id ?? "")));
  }
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
    // Full-page navigations to an API route must NOT be intercepted. The
    // primary SSO entry point is a real document navigation
    // (<a href="/api/v1/auth/kchat/start">) that the server answers with a
    // 302 to an external OAuth provider. A navigation-mode fetch() inside
    // the SW that meets a cross-origin redirect becomes a network error,
    // which would drop us into the catch below and serve the cached app
    // shell — silently breaking the OAuth hand-off. Returning without
    // calling respondWith() lets the browser perform the navigation (and
    // follow the redirect) exactly as it would with no SW installed.
    if (isApiRequest(url)) return;
    event.respondWith(handleNavigation(request));
    return;
  }

  if (isApiRequest(url)) {
    event.respondWith(handleApiRead(request));
    return;
  }

  event.respondWith(handleStatic(request, event));
});

// Write-through to the cache that never rejects into the caller. cache.put
// can fail when storage quota is exceeded; since the response is already
// being returned, a failed cache write is non-fatal and is swallowed here
// so it doesn't surface as an unhandled promise rejection in the SW.
function cachePut(cache, key, res) {
  return cache.put(key, res).catch(() => {});
}

// Network-first for document navigations; fall back to the cached app
// shell so an offline cold start still renders the SPA. A successful
// fetch refreshes the cached "/" shell so the offline fallback doesn't
// go stale after a deploy (the bundle's assets are content-hashed, so
// the refreshed shell always references URLs that resolve).
async function handleNavigation(request) {
  const cache = await caches.open(STATIC_CACHE);
  try {
    const res = await fetch(request);
    if (res && res.ok) cachePut(cache, "/", res.clone());
    return res;
  } catch {
    const shell = (await cache.match("/")) || (await cache.match(request));
    return shell || Response.error();
  }
}

// Stale-while-revalidate for static assets. When a cached copy exists we
// return it immediately and refresh in the background; the background
// fetch is handed to event.waitUntil() so the SW isn't terminated before
// the revalidation writes through (per the SW lifecycle spec).
async function handleStatic(request, event) {
  const cache = await caches.open(STATIC_CACHE);
  const cached = await cache.match(request);
  const network = fetch(request)
    .then((res) => {
      if (res && res.ok) cachePut(cache, request, res.clone());
      return res;
    })
    .catch(() => undefined);
  if (cached) {
    // Keep the worker alive until the background refresh settles.
    event.waitUntil(network);
    return cached;
  }
  return (await network) || Response.error();
}

// Network-first for read APIs, caching successes for offline reads.
async function handleApiRead(request) {
  const cache = await caches.open(API_CACHE);
  try {
    const res = await fetch(request);
    if (res && res.ok) cachePut(cache, request, res.clone());
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
