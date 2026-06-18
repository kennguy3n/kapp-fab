import { describe, it, expect } from "vitest";
import { useState } from "react";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../../test-utils";
import { LineItemsEditor } from "./LineItemsEditor";
import { DOCUMENT_CONFIGS } from "./configs";
import type { ItemOption, LineItem } from "./types";

const ITEMS: ItemOption[] = [
  { value: "i1", label: "Widget", price: 10, uom: "ea" },
  { value: "i2", label: "Gadget", price: 4.5, uom: "box" },
];

function Harness({ initial = [] as LineItem[] }: { initial?: LineItem[] }) {
  const [lines, setLines] = useState<LineItem[]>(initial);
  return (
    <LineItemsEditor
      lines={lines}
      onChange={setLines}
      itemOptions={ITEMS}
      columns={DOCUMENT_CONFIGS.sales_order.columns}
      currency="USD"
    />
  );
}

describe("LineItemsEditor", () => {
  it("shows a teaching empty row when there are no lines", () => {
    renderWithProviders(<Harness />);
    expect(screen.getByText(/No items yet/i)).toBeInTheDocument();
  });

  it("adds and removes line rows", async () => {
    const user = userEvent.setup();
    renderWithProviders(<Harness />);
    await user.click(screen.getByRole("button", { name: "Add line" }));
    expect(screen.getByLabelText("Item for line 1")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove line 1" }));
    expect(screen.queryByLabelText("Item for line 1")).not.toBeInTheDocument();
    expect(screen.getByText(/No items yet/i)).toBeInTheDocument();
  });

  it("prefills unit price from the selected item and computes the line amount", async () => {
    const user = userEvent.setup();
    renderWithProviders(<Harness />);
    await user.click(screen.getByRole("button", { name: "Add line" }));
    await user.selectOptions(screen.getByLabelText("Item for line 1"), "i1");

    // qty defaults to 1, price prefilled to 10 → amount USD 10.00
    const priceInput = screen.getByLabelText("Unit price for line 1") as HTMLInputElement;
    expect(priceInput.value).toBe("10");

    const row = priceInput.closest("tr") as HTMLTableRowElement;
    expect(within(row).getByText(/USD\s*10\.00/)).toBeInTheDocument();

    const qty = screen.getByLabelText("Quantity for line 1");
    await user.clear(qty);
    await user.type(qty, "3");
    expect(within(row).getByText(/USD\s*30\.00/)).toBeInTheDocument();
  });

  it("clamps a discount typed above the line gross down to the gross", async () => {
    const user = userEvent.setup();
    renderWithProviders(<Harness />);
    await user.click(screen.getByRole("button", { name: "Add line" }));
    await user.selectOptions(screen.getByLabelText("Item for line 1"), "i1");

    // qty 1 × price 10 → gross 10, so the discount can't exceed 10.
    const discount = screen.getByLabelText("Discount for line 1") as HTMLInputElement;
    await user.type(discount, "50");
    expect(discount.value).toBe("10");

    const row = discount.closest("tr") as HTMLTableRowElement;
    expect(within(row).getByText(/USD\s*0\.00/)).toBeInTheDocument();
  });
});
