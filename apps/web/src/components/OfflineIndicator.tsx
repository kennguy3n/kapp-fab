import { useEffect, useState } from "react";
import { countQueue, drainAll, subscribeQueue } from "../lib/offlineQueue";

/**
 * OfflineIndicator is a shell-level banner that surfaces two related
 * states the user otherwise can't see:
 *
 *   1. Connectivity — when `navigator.onLine` flips to false, the app
 *      still works (read paths are served from the service-worker
 *      cache) but the user should know writes are being deferred.
 *   2. Pending offline mutations — writes that failed while offline
 *      are persisted to the shared IndexedDB queue (offlineQueue.ts)
 *      by the app layer (e.g. POS finalize).
 *
 * Because it's always mounted in the shell, this component also OWNS
 * the global drain: on reconnect (and on mount) it calls `drainAll()`,
 * which replays every registered mutation type via its handler. That's
 * what makes the "syncing" message truthful regardless of which page is
 * open — previously a queued write only drained while POSPage happened
 * to be mounted.
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
    // Drive the global drain on mount and on reconnect, then re-count.
    // Re-count also fires whenever the queue changes so the banner
    // shrinks as entries replay.
    const drainThenRefresh = () => {
      drainAll()
        .catch(() => {
          // Drain errors are handled per-entry inside the queue.
        })
        .finally(refresh);
    };
    refresh();
    drainThenRefresh();
    const unsubscribe = subscribeQueue(refresh);
    window.addEventListener("online", drainThenRefresh);
    return () => {
      cancelled = true;
      unsubscribe();
      window.removeEventListener("online", drainThenRefresh);
    };
  }, []);

  if (online && pending === 0) return null;

  const changeCount = `${pending} change${pending === 1 ? "" : "s"}`;

  const message = !online
    ? pending > 0
      ? `You're offline — ${changeCount} queued, will sync when reconnected.`
      : "You're offline — changes will sync when you reconnect."
    : `Back online — syncing ${changeCount}…`;

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
