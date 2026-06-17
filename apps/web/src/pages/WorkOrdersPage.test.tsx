import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listWorkOrders = vi.fn();
const listInventoryItems = vi.fn();
const listInventoryWarehouses = vi.fn();
const createWorkOrder = vi.fn();
const releaseWorkOrder = vi.fn();
const startWorkOrder = vi.fn();
const completeWorkOrder = vi.fn();
const cancelWorkOrder = vi.fn();
const closeWorkOrder = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listWorkOrders: (...a: unknown[]) => listWorkOrders(...a),
    listInventoryItems: (...a: unknown[]) => listInventoryItems(...a),
    listInventoryWarehouses: (...a: unknown[]) => listInventoryWarehouses(...a),
    createWorkOrder: (...a: unknown[]) => createWorkOrder(...a),
    releaseWorkOrder: (...a: unknown[]) => releaseWorkOrder(...a),
    startWorkOrder: (...a: unknown[]) => startWorkOrder(...a),
    completeWorkOrder: (...a: unknown[]) => completeWorkOrder(...a),
    cancelWorkOrder: (...a: unknown[]) => cancelWorkOrder(...a),
    closeWorkOrder: (...a: unknown[]) => closeWorkOrder(...a),
  },
}));

import { WorkOrdersPage } from "./WorkOrdersPage";
import { renderWithProviders } from "../test-utils";

const ITEM = { id: "item-1", sku: "SKU-1", name: "Widget" };
const WH = { id: "wh-1", code: "MAIN", name: "Main DC" };

function wo(overrides: Record<string, unknown> = {}) {
  return {
    id: "wo-1",
    item_id: "item-1",
    warehouse_id: "wh-1",
    status: "draft",
    planned_qty: "5",
    actual_qty: "",
    ...overrides,
  };
}

describe("WorkOrdersPage", () => {
  beforeEach(() => {
    [
      listWorkOrders, listInventoryItems, listInventoryWarehouses,
      createWorkOrder, releaseWorkOrder, startWorkOrder, completeWorkOrder,
      cancelWorkOrder, closeWorkOrder,
    ].forEach((m) => m.mockReset());
    listInventoryItems.mockResolvedValue([ITEM]);
    listInventoryWarehouses.mockResolvedValue([WH]);
    listWorkOrders.mockResolvedValue([wo()]);
  });

  it("renders the kanban lanes and a draft card's transitions", async () => {
    renderWithProviders(<WorkOrdersPage />);
    // "SKU-1 — Widget" also appears as a <select> option, so gate on the
    // lane count which is unique to the board.
    expect(await screen.findByText("Draft (1)")).toBeInTheDocument();
    expect(screen.getByText("Released (0)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Release" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("creates a work order from the header form", async () => {
    createWorkOrder.mockResolvedValue(wo());
    const user = userEvent.setup();
    renderWithProviders(<WorkOrdersPage />);
    await screen.findByText("Draft (1)");

    await user.selectOptions(screen.getByLabelText(/^Item/), "item-1");
    await user.selectOptions(screen.getByLabelText(/^Warehouse/), "wh-1");
    await user.click(screen.getByRole("button", { name: "Create work order" }));

    await waitFor(() =>
      expect(createWorkOrder).toHaveBeenCalledWith(
        expect.objectContaining({ item_id: "item-1", warehouse_id: "wh-1", planned_qty: "1" }),
      ),
    );
  });

  it("releases a draft work order", async () => {
    releaseWorkOrder.mockResolvedValue({});
    const user = userEvent.setup();
    renderWithProviders(<WorkOrdersPage />);
    await user.click(await screen.findByRole("button", { name: "Release" }));
    await waitFor(() => expect(releaseWorkOrder).toHaveBeenCalledWith("wo-1"));
  });

  it("completes an in-progress work order with the actual quantity", async () => {
    listWorkOrders.mockResolvedValue([wo({ status: "in_progress" })]);
    completeWorkOrder.mockResolvedValue({});
    const user = userEvent.setup();
    renderWithProviders(<WorkOrdersPage />);

    const actual = await screen.findByLabelText("actual qty");
    await user.clear(actual);
    await user.type(actual, "7");
    await user.click(screen.getByRole("button", { name: "Complete" }));
    await waitFor(() => expect(completeWorkOrder).toHaveBeenCalledWith("wo-1", "7"));
  });

  it("toggles the closed lane on demand", async () => {
    listWorkOrders.mockResolvedValue([wo({ id: "wo-c", status: "closed" })]);
    const user = userEvent.setup();
    renderWithProviders(<WorkOrdersPage />);

    const toggle = await screen.findByRole("button", { name: /Show closed \(1\)/ });
    expect(screen.queryByText("Closed (1)")).not.toBeInTheDocument();
    await user.click(toggle);
    expect(screen.getByText("Closed (1)")).toBeInTheDocument();
  });

  it("surfaces a load error", async () => {
    listWorkOrders.mockRejectedValue(new Error("kaboom"));
    renderWithProviders(<WorkOrdersPage />);
    expect(await screen.findByText(/kaboom/)).toBeInTheDocument();
  });
});
