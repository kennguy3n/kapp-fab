import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
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
});
