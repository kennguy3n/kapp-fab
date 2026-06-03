// fake-indexeddb/auto installs a spec-compliant in-memory IndexedDB on
// the global object — jsdom 25 ships none. Imported first so the queue
// module (which reads the `indexedDB` global lazily) sees it.
import "fake-indexeddb/auto";
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  clearQueue,
  countQueue,
  drainAll,
  drainQueue,
  enqueue,
  listQueue,
  registerReplayHandler,
  removeFromQueue,
  subscribeQueue,
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

  it("drainAll replays only registered types and removes succeeded entries", async () => {
    await enqueue(mutation("p1", "pos.finalize"));
    await enqueue(mutation("p2", "pos.finalize"));
    await enqueue(mutation("o1", "other.kind"));

    const handled: string[] = [];
    const unregister = registerReplayHandler("pos.finalize", async (m) => {
      handled.push(m.id);
    });

    await drainAll();

    // Both pos.finalize entries replayed and removed; the unregistered
    // "other.kind" entry is left untouched.
    expect(handled.sort()).toEqual(["p1", "p2"]);
    expect((await listQueue("pos.finalize")).length).toBe(0);
    expect((await listQueue("other.kind")).map((m) => m.id)).toEqual(["o1"]);

    unregister();
  });

  it("drainAll leaves failed entries queued for a later pass", async () => {
    await enqueue(mutation("p1", "pos.finalize"));
    const unregister = registerReplayHandler("pos.finalize", async () => {
      throw new Error("still offline");
    });

    await drainAll();

    expect((await listQueue("pos.finalize")).map((m) => m.id)).toEqual(["p1"]);
    unregister();
  });

  it("drainAll coalesces concurrent calls into one in-flight pass", async () => {
    await enqueue(mutation("p1", "pos.finalize"));
    let calls = 0;
    const unregister = registerReplayHandler("pos.finalize", async (m) => {
      calls += 1;
      void m;
    });

    await Promise.all([drainAll(), drainAll()]);

    // The entry is replayed exactly once despite two concurrent drains.
    expect(calls).toBe(1);
    unregister();
  });

  // Regression: the shell (OfflineIndicator) mounts before a page can
  // register its replay handler, so its drainAll() runs against an empty
  // handler map and the coalescing `drainInFlight` would otherwise make
  // a page's subsequent drainAll() join that handler-less pass — skipping
  // replay. A page must therefore drain its OWN type via drainQueue,
  // which is independent of drainAll coalescing.
  it("direct drainQueue replays even while a handler-less drainAll is in flight", async () => {
    await enqueue(mutation("p1", "pos.finalize"));

    // Shell drains first with nothing registered (the real mount order).
    const shellPass = drainAll();

    // Page registers its handler and drains its own type directly.
    const handled: string[] = [];
    const replay = async (m: QueuedMutation) => {
      handled.push(m.id);
    };
    const unregister = registerReplayHandler("pos.finalize", replay);
    await drainQueue(replay, "pos.finalize");
    await shellPass;

    // The entry was replayed by the direct drain, not stranded by the
    // coalesced handler-less drainAll pass.
    expect(handled).toEqual(["p1"]);
    expect((await listQueue("pos.finalize")).length).toBe(0);
    unregister();
  });
});
