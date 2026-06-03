/*
 * Kapp service worker.
 *
 * Responsibilities:
 *   1. App-shell + static-asset caching so the SPA boots offline.
 *      - Navigations: network-first, falling back to the cached
 *        shell ("/") so a cold offline launch still renders the app.
 *      - Hashed static assets (scripts/styles/fonts/images):
 *        cache-first with a background revalidate (stale-while-
 *        revalidate) — instant loads, fresh on the next visit.
 *   2. Read API caching: GET /api/* is network-first with a cached
 *      fallback, and successful responses are stored so the same read
 *      works offline (stale-while-revalidate semantics).
 *   3. Offline mutation queue: a failed mutating /api/* request
 *      (POST/PUT/PATCH/DELETE) is persisted to the shared IndexedDB
 *      queue (same DB/store as src/lib/offlineQueue.ts) and a
 *      Background Sync is registered to replay it on reconnect. The
 *      page is notified so the OfflineIndicator's count updates.
 *
 * This file is served from /sw.js (apps/web/public/) so its scope is
 * the whole origin. It is intentionally dependency-free — service
 * workers run in a separate context and can't import the app bundle.
 */

const CACHE_VERSION = "v1";
const STATIC_CACHE = `kapp-static-${CACHE_VERSION}`;
const API_CACHE = `kapp-api-${CACHE_VERSION}`;
const APP_SHELL = ["/", "/manifest.json", "/icon.svg"];

const SYNC_TAG = "kapp-sync-mutations";
const QUEUE_CHANGED = "kapp:offline-queue-changed";

// --- IndexedDB queue (mirrors src/lib/offlineQueue.ts) ---------------
const DB_NAME = "kapp-offline";
const DB_VERSION = 1;
const STORE = "mutations";

function openDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) {
        const store = db.createObjectStore(STORE, { keyPath: "id" });
        store.createIndex("type", "type", { unique: false });
        store.createIndex("queuedAt", "queuedAt", { unique: false });
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function putMutation(mutation) {
  const db = await openDB();
  await new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    tx.objectStore(STORE).put(mutation);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

async function getAllMutations() {
  const db = await openDB();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readonly");
    const req = tx.objectStore(STORE).getAll();
    req.onsuccess = () => resolve(req.result || []);
    req.onerror = () => reject(req.error);
  });
}

async function deleteMutation(id) {
  const db = await openDB();
  await new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    tx.objectStore(STORE).delete(id);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

async function notifyClients() {
  const all = await self.clients.matchAll({ includeUncontrolled: true });
  for (const client of all) {
    client.postMessage({ type: QUEUE_CHANGED });
  }
}

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

  if (request.method !== "GET") {
    if (isApiRequest(url)) {
      event.respondWith(handleMutation(request));
    }
    return;
  }

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
// shell so an offline cold start still renders the SPA.
async function handleNavigation(request) {
  try {
    return await fetch(request);
  } catch {
    const cache = await caches.open(STATIC_CACHE);
    const shell = await cache.match("/");
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

// Try a mutating request; on network failure persist it and register a
// Background Sync so it replays on reconnect.
async function handleMutation(request) {
  try {
    return await fetch(request.clone());
  } catch {
    await queueRequest(request);
    if ("sync" in self.registration) {
      try {
        await self.registration.sync.register(SYNC_TAG);
      } catch {
        // Sync registration unsupported — the queue still drains on
        // the next page load / explicit drain.
      }
    }
    return new Response(
      JSON.stringify({ queued: true, message: "Request queued offline." }),
      { status: 202, headers: { "Content-Type": "application/json" } },
    );
  }
}

async function queueRequest(request) {
  const body = await request.clone().text();
  const headers = {};
  for (const [key, value] of request.headers.entries()) headers[key] = value;
  const id =
    request.headers.get("Idempotency-Key") ||
    (self.crypto && self.crypto.randomUUID
      ? self.crypto.randomUUID()
      : `${Date.now()}-${Math.random().toString(16).slice(2)}`);
  await putMutation({
    id,
    type: "sw.request",
    payload: { url: request.url, method: request.method, headers, body },
    queuedAt: new Date().toISOString(),
  });
  await notifyClients();
}

// --- Background Sync replay -----------------------------------------
self.addEventListener("sync", (event) => {
  if (event.tag === SYNC_TAG) {
    event.waitUntil(replayQueue());
  }
});

async function replayQueue() {
  const pending = (await getAllMutations()).filter(
    (m) => m.type === "sw.request",
  );
  let changed = false;
  for (const mutation of pending) {
    const { url, method, headers, body } = mutation.payload;
    try {
      const res = await fetch(url, {
        method,
        headers,
        body: body || undefined,
      });
      if (res && res.ok) {
        await deleteMutation(mutation.id);
        changed = true;
      }
    } catch {
      // Still offline — leave the entry for the next sync.
    }
  }
  if (changed) await notifyClients();
}

// Allow the page to trigger a drain explicitly (e.g. on reconnect when
// Background Sync isn't available).
self.addEventListener("message", (event) => {
  if (event.data && event.data.type === "kapp:drain-queue") {
    event.waitUntil(replayQueue());
  }
});
