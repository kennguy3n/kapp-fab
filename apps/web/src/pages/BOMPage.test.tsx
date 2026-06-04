import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listBOMs = vi.fn();
const listInventoryItems = vi.fn();
const setBOMStatus = vi.fn();
const createBOM = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listBOMs: (...a: unknown[]) => listBOMs(...a),
    listInventoryItems: (...a: unknown[]) => listInventoryItems(...a),
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
    [listBOMs, listInventoryItems, setBOMStatus, createBOM].forEach((m) => m.mockReset());
    listInventoryItems.mockResolvedValue(ITEMS);
    listBOMs.mockResolvedValue([bom()]);
  });

  it("lists BOMs with resolved item labels and a draft action", async () => {
    renderWithProviders(<BOMPage />);
    // The label also appears as a <select> option, so scope to the cell.
    expect(await screen.findByRole("cell", { name: "FG-1 — Finished Good" })).toBeInTheDocument();
    expect(screen.getByText("draft")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Activate" })).toBeInTheDocument();
  });

  it("filters the list by status", async () => {
    const user = userEvent.setup();
    renderWithProviders(<BOMPage />);
    await screen.findByRole("cell", { name: "FG-1 — Finished Good" });
    await user.selectOptions(screen.getByLabelText(/Status:/), "active");
    await waitFor(() => expect(listBOMs).toHaveBeenCalledWith("active"));
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

  it("manages component rows and creates a BOM", async () => {
    createBOM.mockResolvedValue(bom());
    const user = userEvent.setup();
    renderWithProviders(<BOMPage />);
    await screen.findByRole("cell", { name: "FG-1 — Finished Good" });

    // Pick the finished good, then choose its single component.
    await user.selectOptions(screen.getByLabelText("Finished good"), "item-fg");

    // A second component row can be added then removed again.
    await user.click(screen.getByRole("button", { name: "+ Add component" }));
    const removeButtons = screen.getAllByRole("button", { name: "remove component" });
    expect(removeButtons).toHaveLength(2);
    await user.click(removeButtons[1]);
    expect(screen.getAllByRole("button", { name: "remove component" })).toHaveLength(1);

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
