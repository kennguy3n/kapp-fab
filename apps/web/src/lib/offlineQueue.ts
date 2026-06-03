// Shared IndexedDB-backed offline mutation queue.
//
// This generalises the offline-replay pattern that previously lived
// inline in POSPage (a localStorage queue of pending POS finalize
// calls). Offline writes are owned entirely by the app layer (the
// service worker deliberately does NOT intercept mutations — see the
// header of public/sw.js for why a synthesised "queued" response would
// corrupt the typed ApiClient). The flow is:
//
//   1. A surface (POSPage, future mutation surfaces) enqueues a
//      `QueuedMutation` when a write fails because the device is offline.
//   2. The surface registers a typed replay handler for its mutation
//      `type` via `registerReplayHandler`.
//   3. The always-mounted shell calls `drainAll()` on reconnect, which
//      replays every registered kind with its idempotency key so retries
//      collapse to a single server-side outcome.
//
// IndexedDB (not localStorage) is used because it stores structured
// values without manual JSON (de)serialisation and does not block the
// main thread.

const DB_NAME = "kapp-offline";
const DB_VERSION = 1;
const STORE = "mutations";

/**
 * Fired on `window` whenever the queue changes (enqueue / remove /
 * clear). Components such as OfflineIndicator subscribe via
 * `subscribeQueue` to refresh their pending count.
 */
export const QUEUE_CHANGED_EVENT = "kapp:offline-queue-changed";

export interface QueuedMutation<T = unknown> {
  /** Stable, client-generated key. For replayable writes this is the
   *  Idempotency-Key sent to the server so retries collapse to one
   *  server-side outcome. Doubles as the IndexedDB primary key. */
  id: string;
  /** Discriminator for the mutation kind, e.g. "pos.finalize". Lets a
   *  consumer drain only the entries it knows how to replay. */
  type: string;
  /** Arbitrary structured-clonable body for the replay. */
  payload: T;
  /** ISO-8601 timestamp the entry was queued, for FIFO ordering. */
  queuedAt: string;
}

function hasIndexedDB(): boolean {
  return typeof indexedDB !== "undefined" && indexedDB !== null;
}

// A single shared open-DB promise so concurrent callers reuse one
// connection instead of racing multiple `open` requests.
let dbPromise: Promise<IDBDatabase> | null = null;

function openDB(): Promise<IDBDatabase> {
  if (!hasIndexedDB()) {
    return Promise.reject(new Error("IndexedDB is not available"));
  }
  if (dbPromise) return dbPromise;
  dbPromise = new Promise<IDBDatabase>((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) {
        const store = db.createObjectStore(STORE, { keyPath: "id" });
        store.createIndex("type", "type", { unique: false });
        store.createIndex("queuedAt", "queuedAt", { unique: false });
      }
    };
    req.onsuccess = () => {
      const db = req.result;
      // If another tab opens the DB with a higher version, close this
      // connection so we don't block its upgrade (and so that tab's
      // open doesn't hang on `onblocked`). The matching `onclose` below
      // drops the cached promise so the next call here reopens.
      db.onversionchange = () => db.close();
      // If the connection is later force-closed (e.g. the version change
      // above, or eviction), drop the cached promise so the next call
      // reopens instead of handing out a dead connection.
      db.onclose = () => {
        dbPromise = null;
      };
      resolve(db);
    };
    // A version upgrade is blocked by another tab's still-open
    // connection. Rather than leave `dbPromise` as a never-resolving
    // promise (which would wedge the queue for the whole session),
    // reject so the caller can retry once the other tab releases.
    req.onblocked = () => {
      dbPromise = null;
      reject(req.error ?? new Error("open blocked by another connection"));
    };
    req.onerror = () => {
      // Don't cache the rejection: a transient open failure (storage
      // pressure, a blocked upgrade) would otherwise permanently wedge
      // the queue for the rest of the session. Clear it so the next
      // call retries.
      dbPromise = null;
      reject(req.error ?? new Error("open failed"));
    };
  }).catch((err) => {
    dbPromise = null;
    throw err;
  });
  return dbPromise;
}

// Wrap an IDBRequest in a promise.
function promisifyRequest<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error("request failed"));
  });
}

function emitChanged(): void {
  if (typeof window !== "undefined" && typeof window.dispatchEvent === "function") {
    window.dispatchEvent(new Event(QUEUE_CHANGED_EVENT));
  }
}

/** Append a mutation to the queue. Resolves once it is durably stored. */
export async function enqueue(mutation: QueuedMutation): Promise<void> {
  const db = await openDB();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    tx.objectStore(STORE).put(mutation);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("enqueue failed"));
    tx.onabort = () => reject(tx.error ?? new Error("enqueue aborted"));
  });
  emitChanged();
}

/**
 * List queued mutations in FIFO (queuedAt) order. Pass `type` to
 * restrict the result to one mutation kind.
 */
export async function listQueue(type?: string): Promise<QueuedMutation[]> {
  const db = await openDB();
  const tx = db.transaction(STORE, "readonly");
  const all = await promisifyRequest(tx.objectStore(STORE).getAll());
  const items = all as QueuedMutation[];
  const filtered = type ? items.filter((m) => m.type === type) : items;
  return filtered.sort((a, b) => a.queuedAt.localeCompare(b.queuedAt));
}

/** Number of queued mutations (optionally of a single `type`). */
export async function countQueue(type?: string): Promise<number> {
  // Counting via listQueue keeps the type filter in one place; the
  // queue is expected to be small (pending offline writes), so the
  // extra getAll is negligible versus maintaining a per-type cursor.
  const items = await listQueue(type);
  return items.length;
}

/** Remove a single mutation by id (e.g. after a successful replay). */
export async function removeFromQueue(id: string): Promise<void> {
  const db = await openDB();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    tx.objectStore(STORE).delete(id);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("remove failed"));
    tx.onabort = () => reject(tx.error ?? new Error("remove aborted"));
  });
  emitChanged();
}

/** Remove every queued mutation. */
export async function clearQueue(): Promise<void> {
  const db = await openDB();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    tx.objectStore(STORE).clear();
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("clear failed"));
    tx.onabort = () => reject(tx.error ?? new Error("clear aborted"));
  });
  emitChanged();
}

export interface DrainResult {
  /** Mutations whose handler resolved (and were removed from the queue). */
  succeeded: string[];
  /** Mutations whose handler threw (left in the queue for a later retry). */
  failed: string[];
}

/**
 * Drain queued mutations sequentially, invoking `handler` for each.
 * A handler that resolves marks the entry done and removes it; a
 * handler that throws leaves the entry queued so a later drain
 * retries it. Pass `type` to drain only one mutation kind — entries
 * of other kinds are left untouched (so POS and a generic mutation
 * consumer can drain independently without stepping on each other).
 *
 * Draining is sequential by design: replays often have ordering or
 * rate constraints. Every queued entry is attempted on each pass —
 * a handler failure is recorded in `failed` and the entry is left in
 * the queue for the next drain, but it does NOT abort the remaining
 * entries (POS finalizes are independent, so one bad entry shouldn't
 * strand the rest). When genuinely offline `fetch` rejects almost
 * immediately, so this is N fast failures rather than N slow timeouts.
 */
export async function drainQueue(
  handler: (mutation: QueuedMutation) => Promise<void>,
  type?: string,
): Promise<DrainResult> {
  const pending = await listQueue(type);
  const succeeded: string[] = [];
  const failed: string[] = [];
  for (const mutation of pending) {
    try {
      await handler(mutation);
      await removeFromQueue(mutation.id);
      succeeded.push(mutation.id);
    } catch {
      failed.push(mutation.id);
    }
  }
  return { succeeded, failed };
}

// --- Shell-level global drain ----------------------------------------
//
// A mutation can only be replayed by code that knows its typed shape
// (idempotency key, endpoint, dependent IDs). So each surface that
// enqueues a mutation kind registers a replay handler for its `type`;
// the always-mounted shell (OfflineIndicator) then calls drainAll() on
// reconnect to replay every registered kind — the queue no longer drains
// only while the originating page happens to be mounted.

type ReplayHandler = (mutation: QueuedMutation) => Promise<void>;
const replayHandlers = new Map<string, ReplayHandler>();

/**
 * Register a replay handler for a mutation `type`. The shell-level
 * `drainAll()` uses these to replay queued mutations regardless of which
 * page is currently mounted. Returns an unregister function (safe to call
 * even if a newer handler has since replaced this one).
 */
export function registerReplayHandler(
  type: string,
  handler: ReplayHandler,
): () => void {
  replayHandlers.set(type, handler);
  return () => {
    if (replayHandlers.get(type) === handler) replayHandlers.delete(type);
  };
}

let drainInFlight: Promise<void> | null = null;

/**
 * Replay every registered mutation type once. Concurrent callers share a
 * single in-flight pass, so a page mount plus an `online` event don't
 * replay the same entries twice. Types with no registered handler are
 * left untouched (their owning surface hasn't mounted yet this session).
 */
export function drainAll(): Promise<void> {
  if (drainInFlight) return drainInFlight;
  drainInFlight = (async () => {
    for (const [type, handler] of replayHandlers) {
      try {
        await drainQueue(handler, type);
      } catch {
        // A throw here means the queue itself was unreadable (IndexedDB
        // unavailable) — per-entry handler failures are swallowed inside
        // drainQueue. Move on to the next type.
      }
    }
  })().finally(() => {
    drainInFlight = null;
  });
  return drainInFlight;
}

/**
 * Subscribe to queue-change notifications. Returns an unsubscribe
 * function. Fires for every in-page queue write (enqueue / remove /
 * clear). All writers are in-page — the service worker deliberately
 * never touches the queue — so a single window event is sufficient.
 */
export function subscribeQueue(listener: () => void): () => void {
  if (typeof window === "undefined") return () => undefined;
  const onEvent = () => listener();
  window.addEventListener(QUEUE_CHANGED_EVENT, onEvent);
  return () => {
    window.removeEventListener(QUEUE_CHANGED_EVENT, onEvent);
  };
}

/** Test-only: drop the cached connection so a fresh DB can be opened. */
export function __resetForTests(): void {
  dbPromise = null;
}
