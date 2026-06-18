import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listBOMs = vi.fn();
const getBOM = vi.fn();
const listInventoryItems = vi.fn();
const getInventoryValuation = vi.fn();
const setBOMStatus = vi.fn();
const createBOM = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listBOMs: (...a: unknown[]) => listBOMs(...a),
    getBOM: (...a: unknown[]) => getBOM(...a),
    listInventoryItems: (...a: unknown[]) => listInventoryItems(...a),
    getInventoryValuation: (...a: unknown[]) => getInventoryValuation(...a),
    setBOMStatus: (...a: unknown[]) => setBOMStatus(...a),
    createBOM: (...a: unknown[]) => createBOM(...a),
  },
}));

import { BOMPage } from "./BOMPage";
import { renderWithProviders } from "../test-utils";

const ITEMS = [
  { id: "item-fg", sku: "FG-1", name: "Finished Good" },
  { id: "item-comp", sku: "CMP-1", name: "Component" },
];

function bom(overrides: Record<string, unknown> = {}) {
  return {
    id: "bom-1",
    item_id: "item-fg",
    version: "v1",
    status: "draft",
    output_qty: "1",
    uom: "each",
    ...overrides,
  };
}

describe("BOMPage", () => {
  beforeEach(() => {
    [
      listBOMs,
      getBOM,
      listInventoryItems,
      getInventoryValuation,
      setBOMStatus,
      createBOM,
    ].forEach((m) => m.mockReset());
    listInventoryItems.mockResolvedValue(ITEMS);
    listBOMs.mockResolvedValue([bom()]);
    getInventoryValuation.mockResolvedValue({
      as_of: "2026-01-01T00:00:00Z",
      total_value: "0",
      rows: [],
    });
    getBOM.mockResolvedValue(bom());
  });

  it("lists BOMs with resolved item labels and a draft action", async () => {
    renderWithProviders(<BOMPage />);
    // The label also appears as a <select> option, so scope to the cell.
    expect(await screen.findByRole("cell", { name: "FG-1 — Finished Good" })).toBeInTheDocument();
    // Humanized status is rendered as a Badge (<span>), distinct from the
    // filter's <option> of the same text.
    expect(screen.getByText("Draft", { selector: "span" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Activate" })).toBeInTheDocument();
  });

  it("filters the list by status", async () => {
    const user = userEvent.setup();
    renderWithProviders(<BOMPage />);
    await screen.findByRole("cell", { name: "FG-1 — Finished Good" });
    // "obsolete" (not "active") so this asserts the filter triggered the
    // call — the page also fetches active BOMs on mount to cost sub-assemblies.
    await user.selectOptions(screen.getByLabelText(/^Status/), "obsolete");
    await waitFor(() => expect(listBOMs).toHaveBeenCalledWith("obsolete"));
  });

  it("activates a draft BOM", async () => {
    setBOMStatus.mockResolvedValue({});
    const user = userEvent.setup();
    renderWithProviders(<BOMPage />);
    await user.click(await screen.findByRole("button", { name: "Activate" }));
    await waitFor(() => expect(setBOMStatus).toHaveBeenCalledWith("bom-1", "active"));
  });

  it("obsoletes an active BOM", async () => {
    listBOMs.mockResolvedValue([bom({ status: "active" })]);
    setBOMStatus.mockResolvedValue({});
    const user = userEvent.setup();
    renderWithProviders(<BOMPage />);
    await user.click(await screen.findByRole("button", { name: "Obsolete" }));
    await waitFor(() => expect(setBOMStatus).toHaveBeenCalledWith("bom-1", "obsolete"));
  });

  it("surfaces a list load error", async () => {
    listBOMs.mockRejectedValue(new Error("nope"));
    renderWithProviders(<BOMPage />);
    expect(await screen.findByText(/nope/)).toBeInTheDocument();
  });

  it("opens a recipe and rolls component costs up into a make cost", async () => {
    // bom-1 makes 1 "each" from 2 units of item-comp; item-comp's on-hand
    // unit cost is 50 / 10 = 5.00, so the rolled make cost is 2 × 5 = 10.00.
    getBOM.mockResolvedValue(
      bom({
        components: [
          {
            bom_id: "bom-1",
            component_item_id: "item-comp",
            qty: "2",
            uom: "each",
            scrap_percent: null,
            sort_order: 1,
          },
        ],
      }),
    );
    getInventoryValuation.mockResolvedValue({
      as_of: "2026-01-01T00:00:00Z",
      total_value: "50",
      rows: [
        { item_id: "item-comp", sku: "CMP-1", name: "Component", qty: "10", value_cost: "50" },
      ],
    });
    const user = userEvent.setup();
    renderWithProviders(<BOMPage />);
    await user.click(await screen.findByRole("cell", { name: "FG-1 — Finished Good" }));

    await waitFor(() => expect(getBOM).toHaveBeenCalledWith("bom-1"));
    // The exploded component appears in the detail tree.
    expect(
      await screen.findByText("CMP-1 — Component", { selector: "span" }),
    ).toBeInTheDocument();
    // Rolled-up make cost is surfaced (2 × 5.00 = 10.00).
    await waitFor(() =>
      expect(screen.getAllByText("10.00").length).toBeGreaterThan(0),
    );
  });

  it("manages component rows and creates a BOM", async () => {
    createBOM.mockResolvedValue(bom());
    const user = userEvent.setup();
    renderWithProviders(<BOMPage />);
    await screen.findByRole("cell", { name: "FG-1 — Finished Good" });

    // Pick the finished good, then choose its single component.
    await user.selectOptions(screen.getByLabelText(/^Finished good/), "item-fg");

    // A second component row can be added then removed again.
    await user.click(screen.getByRole("button", { name: /Add component/ }));
    const removeButtons = screen.getAllByRole("button", { name: /Remove component/i });
    expect(removeButtons).toHaveLength(2);
    await user.click(removeButtons[1]);
    expect(screen.getAllByRole("button", { name: /Remove component/i })).toHaveLength(1);

    // The component dropdown excludes the finished good itself. The
    // second table on the page is the authoring form's components grid
    // (the first is the BOM list).
    const componentsTable = screen.getAllByRole("table")[1];
    const componentSelect = within(componentsTable).getByRole("combobox");
    await user.selectOptions(componentSelect, "item-comp");

    await user.click(screen.getByRole("button", { name: "Create BOM" }));
    await waitFor(() =>
      expect(createBOM).toHaveBeenCalledWith(
        expect.objectContaining({
          item_id: "item-fg",
          components: [expect.objectContaining({ component_item_id: "item-comp" })],
        }),
      ),
    );
  });
});
