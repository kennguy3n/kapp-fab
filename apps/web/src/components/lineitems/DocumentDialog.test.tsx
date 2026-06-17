import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../../test-utils";
import { DocumentDialog, type DocumentSubmitPayload } from "./DocumentDialog";
import { DOCUMENT_CONFIGS } from "./configs";
import type { ItemOption, RecordOption } from "./types";

const ITEMS: ItemOption[] = [{ value: "i1", label: "Widget", price: 10, uom: "ea" }];
const CUSTOMERS: RecordOption[] = [
  { value: "c1", label: "Acme Corporation" },
  { value: "c2", label: "Globex" },
];

function renderDialog(onSubmit: (p: DocumentSubmitPayload) => void) {
  return renderWithProviders(
    <DocumentDialog
      open
      onClose={() => {}}
      mode="create"
      config={DOCUMENT_CONFIGS.sales_order}
      title="New sales order"
      initialHeader={{}}
      initialLines={[]}
      itemOptions={ITEMS}
      selectOptions={{ customer_id: CUSTOMERS }}
      onSubmit={onSubmit}
    />,
  );
}

describe("DocumentDialog", () => {
  it("blocks submission and shows inline errors when required fields and lines are missing", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderDialog(onSubmit);

    await user.click(screen.getByRole("button", { name: "Create order" }));

    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByText("Add at least one line item with a quantity.")).toBeInTheDocument();
    expect(screen.getAllByText("Required").length).toBeGreaterThanOrEqual(2);
  });

  it("submits a normalised payload once the header and a line are valid", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderDialog(onSubmit);

    await user.selectOptions(screen.getByLabelText(/Customer/), "c1");
    fireEvent.change(screen.getByLabelText(/Order date/), { target: { value: "2024-02-01" } });

    await user.click(screen.getByRole("button", { name: "Add line" }));
    await user.selectOptions(screen.getByLabelText("Item for line 1"), "i1");

    await user.click(screen.getByRole("button", { name: "Create order" }));

    expect(onSubmit).toHaveBeenCalledTimes(1);
    const payload = onSubmit.mock.calls[0][0] as DocumentSubmitPayload;
    expect(payload.lines).toHaveLength(1);
    expect(payload.data).toMatchObject({
      customer_id: "c1",
      order_date: "2024-02-01",
      subtotal: 10,
      total: 10,
    });
  });
});
