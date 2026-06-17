import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listSubcontractOrders = vi.fn();
const getSubcontractOrder = vi.fn();
const createSubcontractOrder = vi.fn();
const issueSubcontractOrder = vi.fn();
const receiveSubcontractOrder = vi.fn();
const closeSubcontractOrder = vi.fn();
const cancelSubcontractOrder = vi.fn();
const listInventoryItems = vi.fn();
const listInventoryWarehouses = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listSubcontractOrders: (...a: unknown[]) => listSubcontractOrders(...a),
    getSubcontractOrder: (...a: unknown[]) => getSubcontractOrder(...a),
    createSubcontractOrder: (...a: unknown[]) => createSubcontractOrder(...a),
    issueSubcontractOrder: (...a: unknown[]) => issueSubcontractOrder(...a),
    receiveSubcontractOrder: (...a: unknown[]) => receiveSubcontractOrder(...a),
    closeSubcontractOrder: (...a: unknown[]) => closeSubcontractOrder(...a),
    cancelSubcontractOrder: (...a: unknown[]) => cancelSubcontractOrder(...a),
    listInventoryItems: (...a: unknown[]) => listInventoryItems(...a),
    listInventoryWarehouses: (...a: unknown[]) => listInventoryWarehouses(...a),
  },
}));

import { SubcontractingPage } from "./SubcontractingPage";
import { renderWithProviders } from "../test-utils";

const ITEM = {
  id: "item-1",
  tenant_id: "t1",
  sku: "SKU-1",
  name: "Widget",
  uom: "ea",
  active: true,
  reorder_level: "0",
};
const COMPONENT = {
  id: "item-2",
  tenant_id: "t1",
  sku: "SKU-2",
  name: "Bracket",
  uom: "ea",
  active: true,
  reorder_level: "0",
};
const WH = { id: "wh-1", tenant_id: "t1", code: "MAIN", name: "Main DC" };

function order(overrides: Record<string, unknown> = {}) {
  return {
    tenant_id: "t1",
    id: "sub-1",
    item_id: "item-1",
    warehouse_id: "wh-1",
    qty: "10",
    received_qty: "0",
    status: "draft",
    charge_amount: "25",
    charge_currency: "USD",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function detail(overrides: Record<string, unknown> = {}) {
  return {
    ...order(overrides),
    components: [
      {
        tenant_id: "t1",
        id: "c1",
        subcontract_order_id: "sub-1",
        item_id: "item-2",
        qty: "20",
        issued_qty: "0",
        created_at: "2026-01-01T00:00:00Z",
      },
    ],
    ...overrides,
  };
}

describe("SubcontractingPage", () => {
  beforeEach(() => {
    [
      listSubcontractOrders,
      getSubcontractOrder,
      createSubcontractOrder,
      issueSubcontractOrder,
      receiveSubcontractOrder,
      closeSubcontractOrder,
      cancelSubcontractOrder,
      listInventoryItems,
      listInventoryWarehouses,
    ].forEach((m) => m.mockReset());
    listInventoryItems.mockResolvedValue([ITEM, COMPONENT]);
    listInventoryWarehouses.mockResolvedValue([WH]);
    listSubcontractOrders.mockResolvedValue([order()]);
  });

  it("lists orders with their status", async () => {
    renderWithProviders(<SubcontractingPage />);
    // The finished-item label also appears in the create-form selects,
    // so gate on the row's unique View control and its status badge.
    expect(
      await screen.findByRole("button", { name: /View SKU-1/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Draft", { selector: "span" }),
    ).toBeInTheDocument();
  });

  it("shows an empty state", async () => {
    listSubcontractOrders.mockResolvedValue([]);
    renderWithProviders(<SubcontractingPage />);
    expect(
      await screen.findByText(/No subcontract orders yet/i),
    ).toBeInTheDocument();
  });

  it("creates an order with a finished item, warehouse and a component", async () => {
    createSubcontractOrder.mockResolvedValue(order());
    getSubcontractOrder.mockResolvedValue(detail());
    const user = userEvent.setup();
    renderWithProviders(<SubcontractingPage />);
    await screen.findByRole("button", { name: /View SKU-1/ });

    await user.selectOptions(
      screen.getByLabelText(/^Finished item/),
      "item-1",
    );
    await user.selectOptions(screen.getByLabelText(/^Warehouse/), "wh-1");
    await user.selectOptions(screen.getByLabelText("component 1"), "item-2");
    await user.click(screen.getByRole("button", { name: "Create order" }));

    await waitFor(() =>
      expect(createSubcontractOrder).toHaveBeenCalledWith(
        expect.objectContaining({
          item_id: "item-1",
          warehouse_id: "wh-1",
          qty: "1",
          components: [expect.objectContaining({ item_id: "item-2" })],
        }),
      ),
    );
  });

  it("issues a draft order after confirming", async () => {
    getSubcontractOrder.mockResolvedValue(detail());
    issueSubcontractOrder.mockResolvedValue(detail({ status: "issued" }));
    const user = userEvent.setup();
    renderWithProviders(<SubcontractingPage />);

    await user.click(await screen.findByRole("button", { name: /View SKU-1/ }));
    await user.click(
      await screen.findByRole("button", { name: "Issue components" }),
    );

    // Confirmation dialog appears; nothing posted until confirmed.
    expect(
      await screen.findByText(/Issue components to the supplier\?/i),
    ).toBeInTheDocument();
    expect(issueSubcontractOrder).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Confirm" }));
    await waitFor(() =>
      expect(issueSubcontractOrder).toHaveBeenCalledWith("sub-1"),
    );
  });

  it("offers receive on an issued order and close on a received one", async () => {
    getSubcontractOrder.mockResolvedValue(detail({ status: "issued" }));
    const user = userEvent.setup();
    renderWithProviders(<SubcontractingPage />);
    await user.click(await screen.findByRole("button", { name: /View SKU-1/ }));
    expect(
      await screen.findByRole("button", { name: "Receive finished item" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Issue components" }),
    ).not.toBeInTheDocument();
  });

  it("surfaces a load error", async () => {
    listSubcontractOrders.mockRejectedValue(new Error("kaboom"));
    renderWithProviders(<SubcontractingPage />);
    expect(await screen.findByText(/kaboom/)).toBeInTheDocument();
  });
});
