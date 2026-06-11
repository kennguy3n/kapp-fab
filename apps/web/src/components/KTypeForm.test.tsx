import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { KType } from "@kapp/client";
import { renderWithProviders } from "../test-utils";
import { KTypeForm } from "./KTypeForm";

// KTypeForm is the shared metadata-driven form renderer: every page
// that edits a KRecord builds its inputs from a KType's field specs
// through this component, so its type→input mapping and submit
// payload shape are pinned here.

function ktypeWith(fields: KType["schema"]["fields"]): KType {
  return {
    name: "ticket",
    version: 1,
    schema: { name: "ticket", version: 1, fields },
  };
}

describe("KTypeForm", () => {
  it("renders one labelled input per field and marks required ones", () => {
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([
          { name: "title", type: "string", required: true },
          { name: "notes", type: "text" },
        ])}
        onSubmit={() => {}}
      />,
    );
    expect(screen.getByText("title", { exact: false }).textContent).toContain(
      "*",
    );
    // Optional field has no required marker.
    expect(screen.getByText("notes", { exact: false }).textContent).not.toContain(
      "*",
    );
  });

  it("submits the accumulated field values as the record payload", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "title", type: "string" }])}
        onSubmit={onSubmit}
      />,
    );
    await user.type(screen.getByRole("textbox"), "Broken printer");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(onSubmit).toHaveBeenCalledWith({ title: "Broken printer" });
  });

  it("seeds inputs from initialData and preserves untouched values on submit", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([
          { name: "title", type: "string" },
          { name: "priority", type: "enum", values: ["low", "high"] },
        ])}
        initialData={{ title: "Seeded", priority: "low" }}
        onSubmit={onSubmit}
      />,
    );
    const titleInput = screen.getByDisplayValue("Seeded");
    expect(titleInput).toBeInTheDocument();
    // Change the enum, leave the title untouched.
    await user.selectOptions(screen.getByRole("combobox"), "high");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledWith({ title: "Seeded", priority: "high" });
  });

  it("renders a checkbox for boolean fields and submits the toggled value", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "billable", type: "boolean" }])}
        onSubmit={onSubmit}
      />,
    );
    const checkbox = screen.getByRole("checkbox");
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledWith({ billable: true });
  });

  it("renders enum fields as a select seeded with the schema's values", () => {
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([
          { name: "status", type: "enum", values: ["open", "closed"] },
        ])}
        onSubmit={() => {}}
      />,
    );
    expect(
      screen.getByRole("option", { name: "open" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("option", { name: "closed" }),
    ).toBeInTheDocument();
  });

  it("coerces numeric field input to a JS number (not a string) on submit", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "qty", type: "number" }])}
        onSubmit={onSubmit}
      />,
    );
    // type="number" inputs expose the ARIA "spinbutton" role.
    await user.type(screen.getByRole("spinbutton"), "42");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledWith({ qty: 42 });
    // Guard against a string regression (e.target.value vs valueAsNumber).
    expect(typeof onSubmit.mock.calls[0][0].qty).toBe("number");
  });

  it("routes integer/float/decimal through the same numeric input", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([
          { name: "count", type: "integer" },
          { name: "price", type: "decimal" },
        ])}
        onSubmit={onSubmit}
      />,
    );
    const [count, price] = screen.getAllByRole("spinbutton");
    await user.type(count, "7");
    await user.type(price, "3.50");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledWith({ count: 7, price: 3.5 });
  });

  it("passes date / datetime field values through as the raw control string", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { container } = renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([
          { name: "due", type: "date" },
          { name: "at", type: "datetime" },
        ])}
        onSubmit={onSubmit}
      />,
    );
    // Date/datetime controls have no stable ARIA role in jsdom, so select
    // by input type. fireEvent.change drives the native value directly,
    // which is more reliable than typing into a date picker under jsdom.
    const dateInput = container.querySelector('input[type="date"]');
    const dtInput = container.querySelector('input[type="datetime-local"]');
    fireEvent.change(dateInput as Element, { target: { value: "2024-03-09" } });
    fireEvent.change(dtInput as Element, {
      target: { value: "2024-03-09T13:05" },
    });
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledWith({
      due: "2024-03-09",
      at: "2024-03-09T13:05",
    });
  });

  it("documents that clearing a numeric input stores NaN (latent KTypeForm issue)", async () => {
    // KTypeForm.tsx:68 stores `e.target.valueAsNumber`, which is NaN for an
    // empty number input. This characterization test pins the CURRENT
    // (suboptimal) behavior so a future fix in the form-owning workstream —
    // e.g. mapping empty → undefined/null — is a deliberate, visible change
    // that updates this assertion rather than silently altering payloads.
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "qty", type: "number" }])}
        initialData={{ qty: 5 }}
        onSubmit={onSubmit}
      />,
    );
    await user.clear(screen.getByRole("spinbutton"));
    await user.click(screen.getByRole("button", { name: "Save" }));
    const submitted = onSubmit.mock.calls[0][0] as { qty: number };
    expect(Number.isNaN(submitted.qty)).toBe(true);
  });
});
