import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listWorkCenters = vi.fn();
const listRoutings = vi.fn();
const listInventoryItems = vi.fn();
const setRoutingStatus = vi.fn();
const setWorkCenterStatus = vi.fn();
const createWorkCenter = vi.fn();
const createRouting = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listWorkCenters: (...a: unknown[]) => listWorkCenters(...a),
    listRoutings: (...a: unknown[]) => listRoutings(...a),
    listInventoryItems: (...a: unknown[]) => listInventoryItems(...a),
    setRoutingStatus: (...a: unknown[]) => setRoutingStatus(...a),
    setWorkCenterStatus: (...a: unknown[]) => setWorkCenterStatus(...a),
    createWorkCenter: (...a: unknown[]) => createWorkCenter(...a),
    createRouting: (...a: unknown[]) => createRouting(...a),
  },
}));

import { RoutingPage } from "./RoutingPage";
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
const WC = {
  tenant_id: "t1",
  id: "wc-1",
  name: "CNC mill",
  capacity_per_hour: "10",
  operating_hours_per_day: "8",
  efficiency_percent: "100",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};
const ROUTING = {
  tenant_id: "t1",
  id: "r-1",
  item_id: "item-1",
  version: "v1",
  status: "draft",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

describe("RoutingPage", () => {
  beforeEach(() => {
    [
      listWorkCenters,
      listRoutings,
      listInventoryItems,
      setRoutingStatus,
      setWorkCenterStatus,
      createWorkCenter,
      createRouting,
    ].forEach((m) => m.mockReset());
    listWorkCenters.mockResolvedValue([WC]);
    listRoutings.mockResolvedValue([ROUTING]);
    listInventoryItems.mockResolvedValue([ITEM]);
  });

  it("lists work centers and routings with humanized status", async () => {
    renderWithProviders(<RoutingPage />);
    expect(
      await screen.findByRole("cell", { name: "CNC mill" }),
    ).toBeInTheDocument();
    // Work-center status rendered as a Badge, not a raw token.
    expect(
      screen.getByText("Active", { selector: "span" }),
    ).toBeInTheDocument();
    // Routing row shows the item label and a draft action.
    expect(
      await screen.findByRole("cell", { name: /SKU-1 — Widget/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Activate" }),
    ).toBeInTheDocument();
  });

  it("shows teaching empty states when there is nothing yet", async () => {
    listWorkCenters.mockResolvedValue([]);
    listRoutings.mockResolvedValue([]);
    renderWithProviders(<RoutingPage />);
    expect(await screen.findByText(/No work centers yet/i)).toBeInTheDocument();
    expect(await screen.findByText(/No routings yet/i)).toBeInTheDocument();
  });

  it("surfaces a load error with retry", async () => {
    listWorkCenters.mockRejectedValue(new Error("kaboom"));
    renderWithProviders(<RoutingPage />);
    expect(await screen.findByText(/kaboom/)).toBeInTheDocument();
  });

  it("creates a work center", async () => {
    createWorkCenter.mockResolvedValue(WC);
    const user = userEvent.setup();
    renderWithProviders(<RoutingPage />);
    await screen.findByRole("cell", { name: "CNC mill" });

    await user.type(screen.getByLabelText(/^Name/), "Lathe");
    await user.click(
      screen.getByRole("button", { name: "Create work center" }),
    );

    await waitFor(() =>
      expect(createWorkCenter).toHaveBeenCalledWith(
        expect.objectContaining({ name: "Lathe" }),
      ),
    );
  });

  it("activates a draft routing", async () => {
    setRoutingStatus.mockResolvedValue({ ...ROUTING, status: "active" });
    const user = userEvent.setup();
    renderWithProviders(<RoutingPage />);
    await user.click(await screen.findByRole("button", { name: "Activate" }));
    await waitFor(() =>
      expect(setRoutingStatus).toHaveBeenCalledWith("r-1", "active"),
    );
  });
});
