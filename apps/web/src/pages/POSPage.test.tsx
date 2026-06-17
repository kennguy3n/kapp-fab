import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listRecords = vi.fn();
const createRecord = vi.fn();
const finalizePOSInvoice = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...a: unknown[]) => listRecords(...a),
    createRecord: (...a: unknown[]) => createRecord(...a),
    finalizePOSInvoice: (...a: unknown[]) => finalizePOSInvoice(...a),
  },
}));

import { POSPage } from "./POSPage";
import { renderWithProviders, makeKRecord } from "../test-utils";

const QUEUE_KEY = "kapp.pos.offline-queue";

const PROFILE = makeKRecord({
  id: "prof-1",
  ktype: "sales.pos_profile",
  data: { name: "Main Register", warehouse_id: "wh-1", currency: "USD", default_customer_id: "cust-1" },
});
const ITEM_A = makeKRecord({
  id: "item-a",
  ktype: "inventory.item",
  data: { name: "Coffee", sku: "COF", barcode: "111", default_price: 3.5 },
});
const ITEM_B = makeKRecord({
  id: "item-b",
  ktype: "inventory.item",
  data: { name: "Tea", sku: "TEA", barcode: "222", default_price: 2 },
});

function routeListRecords(profiles = [PROFILE], items = [ITEM_A, ITEM_B]) {
  listRecords.mockImplementation((ktype: string) => {
    if (ktype === "sales.pos_profile") return Promise.resolve(profiles);
    if (ktype === "inventory.item") return Promise.resolve(items);
    return Promise.resolve([]);
  });
}

describe("POSPage", () => {
  beforeEach(() => {
    listRecords.mockReset();
    createRecord.mockReset();
    finalizePOSInvoice.mockReset();
    localStorage.clear();
    routeListRecords();
  });

  it("renders the item grid and an empty cart", async () => {
    renderWithProviders(<POSPage />);
    expect(screen.getByRole("heading", { name: "Point of Sale" })).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: /Coffee/ })).toBeInTheDocument();
    expect(screen.getByText("Your cart is empty")).toBeInTheDocument();
    expect(screen.getByText(/Total: USD 0\.00/)).toBeInTheDocument();
  });

  it("adds an item to the cart from the grid and accumulates quantity", async () => {
    const user = userEvent.setup();
    renderWithProviders(<POSPage />);
    const tile = await screen.findByRole("button", { name: /Coffee/ });
    await user.click(tile);
    await user.click(tile);
    // Two clicks on the same item collapse into qty 2 at 3.50 each.
    expect(screen.getByText(/Total: USD 7\.00/)).toBeInTheDocument();
  });

  it("rings an item by barcode and reports an unknown code", async () => {
    const user = userEvent.setup();
    renderWithProviders(<POSPage />);
    await screen.findByRole("button", { name: /Coffee/ });
    const input = screen.getByPlaceholderText(/Scan or type barcode/i);

    await user.type(input, "222{Enter}");
    expect(screen.getByText(/Total: USD 2\.00/)).toBeInTheDocument();

    await user.type(input, "999");
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(screen.getByText(/No item matching "999"/)).toBeInTheDocument();
  });

  it("guards finalize when the cart is empty", async () => {
    const user = userEvent.setup();
    renderWithProviders(<POSPage />);
    await screen.findByRole("button", { name: /Coffee/ });
    await user.click(screen.getByRole("button", { name: "Finalize sale" }));
    expect(screen.getByText("Cart is empty")).toBeInTheDocument();
    expect(createRecord).not.toHaveBeenCalled();
  });

  it("creates and finalizes a draft invoice on success", async () => {
    createRecord.mockResolvedValue(makeKRecord({ id: "inv-1", ktype: "sales.pos_invoice" }));
    finalizePOSInvoice.mockResolvedValue(makeKRecord({ id: "inv-1" }));
    const user = userEvent.setup();
    renderWithProviders(<POSPage />);
    await user.click(await screen.findByRole("button", { name: /Coffee/ }));
    await user.click(screen.getByRole("button", { name: "Finalize sale" }));

    await waitFor(() => expect(createRecord).toHaveBeenCalledWith("sales.pos_invoice", expect.any(Object)));
    expect(finalizePOSInvoice).toHaveBeenCalledWith("inv-1", expect.any(String));
    expect(await screen.findByText("Finalized inv-1")).toBeInTheDocument();
    // Cart resets after a successful sale.
    expect(screen.getByText("Your cart is empty")).toBeInTheDocument();
  });

  it("queues the sale offline when finalize fails", async () => {
    createRecord.mockResolvedValue(makeKRecord({ id: "inv-2", ktype: "sales.pos_invoice" }));
    finalizePOSInvoice.mockRejectedValue(new Error("network down"));
    const user = userEvent.setup();
    renderWithProviders(<POSPage />);
    await user.click(await screen.findByRole("button", { name: /Coffee/ }));
    await user.click(screen.getByRole("button", { name: "Finalize sale" }));

    expect(await screen.findByText(/Queued offline: network down/)).toBeInTheDocument();
    expect(screen.getByText(/1 pending/)).toBeInTheDocument();
    // The queue is persisted so a reload (or reconnect) can replay it.
    expect(JSON.parse(localStorage.getItem(QUEUE_KEY) ?? "[]")).toHaveLength(1);
  });

  it("drains a persisted offline queue on mount", async () => {
    localStorage.setItem(
      QUEUE_KEY,
      JSON.stringify([
        { idempotencyKey: "key-1", posInvoiceId: "inv-old", total: 5, queuedAt: "2024-01-01T00:00:00Z" },
      ]),
    );
    finalizePOSInvoice.mockResolvedValue(makeKRecord({ id: "inv-old" }));
    renderWithProviders(<POSPage />);

    await waitFor(() => expect(finalizePOSInvoice).toHaveBeenCalledWith("inv-old", "key-1"));
    // Once replayed, the pending banner disappears and storage is empty.
    await waitFor(() => expect(screen.queryByText(/pending/)).not.toBeInTheDocument());
    expect(JSON.parse(localStorage.getItem(QUEUE_KEY) ?? "[]")).toHaveLength(0);
  });
});
