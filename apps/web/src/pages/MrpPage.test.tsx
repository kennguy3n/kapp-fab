import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listMRPRuns = vi.fn();
const getMRPRun = vi.fn();
const runMRP = vi.fn();
const listInventoryItems = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listMRPRuns: (...a: unknown[]) => listMRPRuns(...a),
    getMRPRun: (...a: unknown[]) => getMRPRun(...a),
    runMRP: (...a: unknown[]) => runMRP(...a),
    listInventoryItems: (...a: unknown[]) => listInventoryItems(...a),
  },
}));

import { MrpPage } from "./MrpPage";
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

function run(overrides: Record<string, unknown> = {}) {
  return {
    tenant_id: "t1",
    id: "run-1",
    status: "completed",
    horizon_start: "2026-01-01",
    horizon_end: "2026-01-31",
    include_min_stock: false,
    buy_lead_time_days: 7,
    demand_line_count: 1,
    planned_order_count: 2,
    make_order_count: 1,
    buy_order_count: 1,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("MrpPage", () => {
  beforeEach(() => {
    [listMRPRuns, getMRPRun, runMRP, listInventoryItems].forEach((m) =>
      m.mockReset(),
    );
    listInventoryItems.mockResolvedValue([ITEM]);
    listMRPRuns.mockResolvedValue([run()]);
  });

  it("lists past runs with their make/buy counts", async () => {
    renderWithProviders(<MrpPage />);
    expect(
      await screen.findByText("Jan 1, 2026 → Jan 31, 2026"),
    ).toBeInTheDocument();
    expect(screen.getByText("Completed")).toBeInTheDocument();
    // make / buy column
    expect(screen.getByText("1 / 1")).toBeInTheDocument();
  });

  it("shows an empty state when there are no runs", async () => {
    listMRPRuns.mockResolvedValue([]);
    renderWithProviders(<MrpPage />);
    expect(
      await screen.findByText(/No MRP runs yet/i),
    ).toBeInTheDocument();
  });

  it("requires demand or reorder top-up before running", async () => {
    const user = userEvent.setup();
    renderWithProviders(<MrpPage />);
    await screen.findByText("Jan 1, 2026 → Jan 31, 2026");

    // Run button is disabled until there is demand or min-stock top-up.
    const submit = screen.getByRole("button", { name: "Run MRP" });
    expect(submit).toBeDisabled();

    // An empty demand row (no item picked) must not arm the submit.
    await user.click(screen.getByRole("button", { name: "Add demand line" }));
    expect(submit).toBeDisabled();
    await user.selectOptions(screen.getByLabelText("Item 1"), "item-1");
    expect(submit).toBeEnabled();

    // Removing the only valid row disarms it again; min-stock re-arms.
    await user.click(screen.getByRole("button", { name: "Remove" }));
    expect(submit).toBeDisabled();
    await user.click(screen.getByLabelText(/Top up items below reorder level/i));
    expect(submit).toBeEnabled();
  });

  it("runs MRP with an explicit demand line and opens the new run", async () => {
    const created = run({ id: "run-2", planned_order_count: 0 });
    runMRP.mockResolvedValue(created);
    getMRPRun.mockResolvedValue({
      ...created,
      demand_lines: [],
      planned_orders: [],
    });
    const user = userEvent.setup();
    renderWithProviders(<MrpPage />);
    await screen.findByText("Jan 1, 2026 → Jan 31, 2026");

    await user.click(screen.getByRole("button", { name: "Add demand line" }));
    await user.selectOptions(screen.getByLabelText("Item 1"), "item-1");
    await user.click(screen.getByRole("button", { name: "Run MRP" }));

    await waitFor(() =>
      expect(runMRP).toHaveBeenCalledWith(
        expect.objectContaining({
          include_min_stock: false,
          demand: [
            expect.objectContaining({ item_id: "item-1", source: "manual" }),
          ],
        }),
      ),
    );
    // Detail panel loads the newly-created run.
    await waitFor(() => expect(getMRPRun).toHaveBeenCalledWith("run-2"));
  });

  it("drills into a run to show planned orders", async () => {
    getMRPRun.mockResolvedValue({
      ...run(),
      demand_lines: [
        {
          tenant_id: "t1",
          id: "d1",
          run_id: "run-1",
          item_id: "item-1",
          qty: "5",
          due_date: "2026-01-20",
          source: "manual",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
      planned_orders: [
        {
          tenant_id: "t1",
          id: "p1",
          run_id: "run-1",
          item_id: "item-1",
          order_type: "make",
          qty: "5",
          due_date: "2026-01-20",
          suggested_start_date: "2026-01-13",
          explosion_level: 0,
          lead_time_days: 7,
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    const user = userEvent.setup();
    renderWithProviders(<MrpPage />);
    await user.click(
      await screen.findByRole("button", { name: /View Jan 1, 2026/ }),
    );

    expect(await screen.findByText("Planned orders")).toBeInTheDocument();
    expect(screen.getByText("Make")).toBeInTheDocument();
    expect(screen.getByText("Jan 13, 2026")).toBeInTheDocument();
    expect(screen.getByText("7d")).toBeInTheDocument();
  });

  it("surfaces a runs load error", async () => {
    listMRPRuns.mockRejectedValue(new Error("kaboom"));
    renderWithProviders(<MrpPage />);
    expect(await screen.findByText(/kaboom/)).toBeInTheDocument();
  });
});
