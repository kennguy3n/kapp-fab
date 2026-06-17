import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { KType } from "@kapp/client";
import { renderWithProviders } from "../test-utils";
import { KTypeForm } from "./KTypeForm";

// The relation control loads its options through the API client; stub it
// so the searchable picker can render without a real network round-trip.
vi.mock("../lib/api", () => ({
  api: { listRecords: vi.fn().mockResolvedValue([]) },
}));

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
    // The option labels are humanized to Title Case for non-experts,
    // while the underlying option values stay the raw schema tokens.
    expect(
      screen.getByRole("option", { name: "Open" }),
    ).toHaveValue("open");
    expect(
      screen.getByRole("option", { name: "Closed" }),
    ).toHaveValue("closed");
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

  it("maps a cleared numeric input to null (explicit clear), never NaN", async () => {
    // An empty number input yields `valueAsNumber === NaN`. KTypeForm maps
    // that to `null` rather than `undefined`: the edit flow PATCHes
    // `JSON.stringify({ data })`, where `undefined` keys are dropped (the
    // backend would read the omitted field as "leave unchanged" and the
    // value could never be cleared). `null` survives serialization and is
    // the explicit "clear this field" signal. NaN must never reach submit.
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
    const submitted = onSubmit.mock.calls[0][0] as { qty: number | null };
    expect(submitted).toHaveProperty("qty", null);
    expect(Number.isNaN(submitted.qty as unknown as number)).toBe(false);
    // Guard the PATCH contract directly: the cleared field must survive
    // JSON serialization as an explicit null, not be dropped.
    expect(JSON.stringify({ data: submitted })).toBe('{"data":{"qty":null}}');
  });

  it("clears the form only after Save & add another resolves", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const onAddAnother = vi.fn().mockResolvedValue(undefined);
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "title", type: "string" }])}
        onSubmit={onSubmit}
        onSubmitAndAddAnother={onAddAnother}
      />,
    );
    await user.type(screen.getByRole("textbox"), "First lead");
    await user.click(
      screen.getByRole("button", { name: "Save & add another" }),
    );
    expect(onAddAnother).toHaveBeenCalledWith({ title: "First lead" });
    // The reset is gated on the save resolving, so the next entry
    // starts from a clean form.
    await waitFor(() => expect(screen.getByRole("textbox")).toHaveValue(""));
  });

  it("preserves entered values when Save & add another fails (no silent data loss)", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    // A rejected save must not wipe the form: the operator keeps their
    // input and can retry without retyping.
    const onAddAnother = vi.fn().mockRejectedValue(new Error("save failed"));
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "title", type: "string" }])}
        onSubmit={onSubmit}
        onSubmitAndAddAnother={onAddAnother}
      />,
    );
    await user.type(screen.getByRole("textbox"), "Precious data");
    await user.click(
      screen.getByRole("button", { name: "Save & add another" }),
    );
    expect(onAddAnother).toHaveBeenCalledWith({ title: "Precious data" });
    await waitFor(() => expect(onAddAnother).toHaveBeenCalledTimes(1));
    // The form keeps the typed value because the save rejected.
    expect(screen.getByRole("textbox")).toHaveValue("Precious data");
  });

  it("keeps the unsaved-changes guard armed when a plain Save fails", async () => {
    const user = userEvent.setup();
    // A rejected save must not clear the dirty flag: the navigate-away
    // guard has to stay armed so the operator can't silently lose input.
    const onSubmit = vi.fn().mockRejectedValue(new Error("save failed"));
    const onCancel = vi.fn();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "title", type: "string" }])}
        onSubmit={onSubmit}
        onCancel={onCancel}
      />,
    );
    await user.type(screen.getByRole("textbox"), "Draft");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    // Still dirty: Cancel prompts to discard, and declining aborts.
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(confirmSpy).toHaveBeenCalledTimes(1);
    expect(onCancel).not.toHaveBeenCalled();
    confirmSpy.mockRestore();
  });

  it("clears the unsaved-changes guard after a successful plain Save", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    const onCancel = vi.fn();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([{ name: "title", type: "string" }])}
        onSubmit={onSubmit}
        onCancel={onCancel}
      />,
    );
    await user.type(screen.getByRole("textbox"), "Draft");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    // The save resolved, so the form is no longer dirty: Cancel navigates
    // straight away without a discard prompt.
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(onCancel).toHaveBeenCalledTimes(1));
    expect(confirmSpy).not.toHaveBeenCalled();
    confirmSpy.mockRestore();
  });

  it("does not submit the form when Enter is pressed in the relation search box", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    renderWithProviders(
      <KTypeForm
        ktype={ktypeWith([
          { name: "owner", type: "relation", ktype: "crm.contact" },
        ])}
        onSubmit={onSubmit}
      />,
    );
    // Open the relation combobox, then press Enter inside its search box.
    await user.click(screen.getByRole("combobox"));
    const searchBox = await screen.findByPlaceholderText(/^Search /i);
    await user.type(searchBox, "ac{Enter}");
    // Enter filters the option list; it must not submit the record form.
    expect(onSubmit).not.toHaveBeenCalled();
  });
});
