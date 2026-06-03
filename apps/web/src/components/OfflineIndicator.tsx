import { useEffect, useState } from "react";
import { countQueue, subscribeQueue } from "../lib/offlineQueue";

/**
 * OfflineIndicator is a shell-level banner that surfaces two related
 * states the user otherwise can't see:
 *
 *   1. Connectivity — when `navigator.onLine` flips to false, the app
 *      still works (read paths are served from the service-worker
 *      cache) but the user should know writes are being deferred.
 *   2. Pending offline mutations — writes that failed while offline
 *      are persisted to the shared IndexedDB queue (offlineQueue.ts)
 *      by both the app (e.g. POS finalize) and the service worker
 *      (background-sync queue). The count reflects both sources.
 *
 * The banner is hidden entirely when the app is online with an empty
 * queue, so it costs nothing in the common case.
 */
export function OfflineIndicator() {
  const [online, setOnline] = useState(() =>
    typeof navigator === "undefined" ? true : navigator.onLine,
  );
  const [pending, setPending] = useState(0);

  useEffect(() => {
    const goOnline = () => setOnline(true);
    const goOffline = () => setOnline(false);
    window.addEventListener("online", goOnline);
    window.addEventListener("offline", goOffline);
    return () => {
      window.removeEventListener("online", goOnline);
      window.removeEventListener("offline", goOffline);
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    const refresh = () => {
      countQueue()
        .then((n) => {
          if (!cancelled) setPending(n);
        })
        .catch(() => {
          // IndexedDB unavailable — treat as an empty queue.
        });
    };
    refresh();
    // Re-count whenever the queue changes (in-app or service-worker
    // writes) and whenever connectivity flips, since a reconnect
    // triggers a drain that shrinks the queue.
    const unsubscribe = subscribeQueue(refresh);
    window.addEventListener("online", refresh);
    return () => {
      cancelled = true;
      unsubscribe();
      window.removeEventListener("online", refresh);
    };
  }, []);

  if (online && pending === 0) return null;

  const queuedLabel =
    pending > 0
      ? `${pending} change${pending === 1 ? "" : "s"} queued`
      : null;

  const message = !online
    ? queuedLabel
      ? `You're offline — ${queuedLabel}, will sync when reconnected.`
      : "You're offline — changes will sync when you reconnect."
    : `Back online — syncing ${queuedLabel}…`;

  return (
    <div
      role="status"
      aria-live="polite"
      data-online={online}
      className={
        "flex items-center gap-2 px-4 py-2 text-sm " +
        (online
          ? "bg-accent/10 text-accent"
          : "bg-amber-100 text-amber-900")
      }
    >
      <span
        aria-hidden="true"
        className={
          "inline-block h-2 w-2 rounded-full " +
          (online ? "bg-accent" : "bg-amber-500")
        }
      />
      <span>{message}</span>
    </div>
  );
}
