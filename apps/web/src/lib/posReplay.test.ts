// fake-indexeddb/auto installs an in-memory IndexedDB on the global so
// the offline queue (read lazily by posReplay) has a real store to drain.
import "fake-indexeddb/auto";
import { describe, it, expect, beforeEach, vi } from "vitest";

const finalizePOSInvoice = vi.fn();

vi.mock("./api", () => ({
  api: {
    finalizePOSInvoice: (...args: unknown[]) => finalizePOSInvoice(...args),
  },
}));

import { clearQueue, drainAll, enqueue } from "./offlineQueue";
import {
  POS_MUTATION_TYPE,
  posFinalizeReplay,
  registerPOSReplay,
} from "./posReplay";

describe("posReplay", () => {
  beforeEach(async () => {
    await clearQueue();
    finalizePOSInvoice.mockReset();
    finalizePOSInvoice.mockResolvedValue({ id: "inv-1" });
  });

  it("replays a queued finalize reusing the entry id as the idempotency key", async () => {
    await posFinalizeReplay({
      id: "idem-123",
      type: POS_MUTATION_TYPE,
      payload: { posInvoiceId: "inv-9", total: 50 },
      queuedAt: "2024-01-01T00:00:00.000Z",
    });
    expect(finalizePOSInvoice).toHaveBeenCalledWith("inv-9", "idem-123");
  });

  // The key regression: once registered at startup, the shell's global
  // drainAll() must replay POS finalizes WITHOUT POSPage ever mounting.
  it("registerPOSReplay lets drainAll replay POS entries with POSPage unmounted", async () => {
    registerPOSReplay();
    await enqueue({
      id: "idem-A",
      type: POS_MUTATION_TYPE,
      payload: { posInvoiceId: "inv-A", total: 10 },
      queuedAt: "2024-01-01T00:00:00.000Z",
    });

    await drainAll();

    expect(finalizePOSInvoice).toHaveBeenCalledTimes(1);
    expect(finalizePOSInvoice).toHaveBeenCalledWith("inv-A", "idem-A");
  });

  it("registerPOSReplay is idempotent (no double-replay on repeat calls)", async () => {
    registerPOSReplay();
    registerPOSReplay();
    await enqueue({
      id: "idem-B",
      type: POS_MUTATION_TYPE,
      payload: { posInvoiceId: "inv-B", total: 20 },
      queuedAt: "2024-01-02T00:00:00.000Z",
    });

    await drainAll();

    expect(finalizePOSInvoice).toHaveBeenCalledTimes(1);
  });
});
