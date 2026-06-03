// fake-indexeddb/auto installs a spec-compliant in-memory IndexedDB on
// the global object — jsdom 25 ships none. Imported first so the queue
// module (which reads the `indexedDB` global lazily) sees it.
import "fake-indexeddb/auto";
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  clearQueue,
  countQueue,
  drainQueue,
  enqueue,
  listQueue,
  removeFromQueue,
  subscribeQueue,
  QUEUE_CHANGED_EVENT,
  type QueuedMutation,
} from "./offlineQueue";

function mutation(
  id: string,
  type = "test.mutation",
  queuedAt = "2024-01-01T00:00:00.000Z",
): QueuedMutation {
  return { id, type, payload: { n: id }, queuedAt };
}

describe("offlineQueue", () => {
  beforeEach(async () => {
    await clearQueue();
  });

  it("enqueues and lists mutations in FIFO (queuedAt) order", async () => {
    await enqueue(mutation("b", "test.mutation", "2024-01-02T00:00:00.000Z"));
    await enqueue(mutation("a", "test.mutation", "2024-01-01T00:00:00.000Z"));

    const items = await listQueue();
    expect(items.map((m) => m.id)).toEqual(["a", "b"]);
  });

  it("filters list/count by mutation type", async () => {
    await enqueue(mutation("p1", "pos.finalize"));
    await enqueue(mutation("p2", "pos.finalize"));
    await enqueue(mutation("s1", "sw.request"));

    expect(await countQueue()).toBe(3);
    expect(await countQueue("pos.finalize")).toBe(2);
    expect((await listQueue("sw.request")).map((m) => m.id)).toEqual(["s1"]);
  });

  it("deduplicates by id (put replaces same key)", async () => {
    await enqueue(mutation("dup"));
    await enqueue({ ...mutation("dup"), payload: { n: "updated" } });

    const items = await listQueue();
    expect(items).toHaveLength(1);
    expect((items[0].payload as { n: string }).n).toBe("updated");
  });

  it("removes a single mutation by id", async () => {
    await enqueue(mutation("keep"));
    await enqueue(mutation("drop"));

    await removeFromQueue("drop");

    expect((await listQueue()).map((m) => m.id)).toEqual(["keep"]);
  });

  it("clears the entire queue", async () => {
    await enqueue(mutation("x"));
    await enqueue(mutation("y"));

    await clearQueue();

    expect(await countQueue()).toBe(0);
  });

  it("drains successful mutations and keeps failures queued", async () => {
    await enqueue(mutation("ok-1", "pos.finalize", "2024-01-01T00:00:01.000Z"));
    await enqueue(mutation("bad", "pos.finalize", "2024-01-01T00:00:02.000Z"));
    await enqueue(mutation("ok-2", "pos.finalize", "2024-01-01T00:00:03.000Z"));

    const handler = vi.fn(async (m: QueuedMutation) => {
      if (m.id === "bad") throw new Error("network down");
    });

    const result = await drainQueue(handler, "pos.finalize");

    expect(handler).toHaveBeenCalledTimes(3);
    expect(result.succeeded).toEqual(["ok-1", "ok-2"]);
    expect(result.failed).toEqual(["bad"]);
    // Only the failed entry survives for a later retry.
    expect((await listQueue()).map((m) => m.id)).toEqual(["bad"]);
  });

  it("only drains the requested type", async () => {
    await enqueue(mutation("pos", "pos.finalize"));
    await enqueue(mutation("sw", "sw.request"));
    const handler = vi.fn(async () => undefined);

    await drainQueue(handler, "pos.finalize");

    expect(handler).toHaveBeenCalledTimes(1);
    // The sw.request entry is untouched.
    expect((await listQueue("sw.request")).map((m) => m.id)).toEqual(["sw"]);
  });

  it("notifies subscribers on queue changes and stops after unsubscribe", async () => {
    const listener = vi.fn();
    const unsubscribe = subscribeQueue(listener);

    await enqueue(mutation("n1"));
    expect(listener).toHaveBeenCalledTimes(1);

    await removeFromQueue("n1");
    expect(listener).toHaveBeenCalledTimes(2);

    unsubscribe();
    await enqueue(mutation("n2"));
    expect(listener).toHaveBeenCalledTimes(2);
  });

  it("bridges service-worker postMessage events to subscribers", () => {
    // jsdom has no navigator.serviceWorker; stand in a minimal
    // EventTarget so the SW-message bridge in subscribeQueue is exercised.
    const swTarget = new EventTarget();
    const original = Object.getOwnPropertyDescriptor(navigator, "serviceWorker");
    Object.defineProperty(navigator, "serviceWorker", {
      value: swTarget,
      configurable: true,
    });

    const listener = vi.fn();
    const unsubscribe = subscribeQueue(listener);

    // The service worker posts this shape to its clients on a queue write.
    swTarget.dispatchEvent(
      new MessageEvent("message", { data: { type: QUEUE_CHANGED_EVENT } }),
    );
    expect(listener).toHaveBeenCalledTimes(1);

    // Unrelated messages are ignored.
    swTarget.dispatchEvent(
      new MessageEvent("message", { data: { type: "something-else" } }),
    );
    expect(listener).toHaveBeenCalledTimes(1);

    unsubscribe();
    swTarget.dispatchEvent(
      new MessageEvent("message", { data: { type: QUEUE_CHANGED_EVENT } }),
    );
    expect(listener).toHaveBeenCalledTimes(1);

    if (original) Object.defineProperty(navigator, "serviceWorker", original);
    else
      Object.defineProperty(navigator, "serviceWorker", {
        value: undefined,
        configurable: true,
      });
  });
});
