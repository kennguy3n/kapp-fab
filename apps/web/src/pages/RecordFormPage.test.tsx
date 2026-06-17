import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { makeKType, makeKRecord } from "../test/factories";

const getKType = vi.fn();
const getRecord = vi.fn();
const createRecord = vi.fn();
const updateRecord = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    getKType: (...a: unknown[]) => getKType(...a),
    getRecord: (...a: unknown[]) => getRecord(...a),
    createRecord: (...a: unknown[]) => createRecord(...a),
    updateRecord: (...a: unknown[]) => updateRecord(...a),
  },
}));

import { RecordFormPage } from "./RecordFormPage";

const KTYPE = makeKType({ name: "crm.deal" });

// Renders the page at /records/:ktype (new) or /records/:ktype/:id
// (edit) and exposes the resolved location so navigation-on-success is
// observable.
function renderForm(path: string) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/records/:ktype" element={<RecordFormPage />} />
          <Route path="/records/:ktype/:id" element={<RecordFormPage />} />
          <Route
            path="/records/:ktype/list"
            element={<div>record list landing</div>}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RecordFormPage", () => {
  beforeEach(() => {
    getKType.mockReset();
    getRecord.mockReset();
    createRecord.mockReset();
    updateRecord.mockReset();
  });

  it("renders the create form heading and the KType's fields", async () => {
    getKType.mockResolvedValue(KTYPE);
    renderForm("/records/crm.deal");

    // Heading uses the humanized, singularized display label — never the
    // raw ktype id.
    expect(await screen.findByText(/New Deal/i)).toBeInTheDocument();
    expect(screen.queryByText(/crm\.deal/i)).not.toBeInTheDocument();
    // Field labels come from the schema, humanized to Title Case. The
    // required field carries an asterisk marker rendered as a sibling
    // element, so assert on the label's full text content.
    expect(screen.getByText("title", { exact: false }).textContent).toContain(
      "*",
    );
    expect(screen.getByText(/stage/i)).toBeInTheDocument();
    expect(screen.getByText(/value/i)).toBeInTheDocument();
    // Print buttons are edit-only — absent in create mode.
    expect(
      screen.queryByRole("button", { name: /Download PDF/i }),
    ).not.toBeInTheDocument();
  });

  it("submits a new record through api.createRecord", async () => {
    getKType.mockResolvedValue(KTYPE);
    createRecord.mockResolvedValue(makeKRecord());
    const user = userEvent.setup();
    renderForm("/records/crm.deal");

    await screen.findByText(/New Deal/i);
    const title = screen.getAllByRole("textbox")[0];
    await user.type(title, "Acme renewal");
    await user.click(screen.getByRole("button", { name: /^Save$/i }));

    await waitFor(() => expect(createRecord).toHaveBeenCalledTimes(1));
    const [ktypeArg, dataArg] = createRecord.mock.calls[0]!;
    expect(ktypeArg).toBe("crm.deal");
    expect((dataArg as Record<string, unknown>).title).toBe("Acme renewal");
    expect(updateRecord).not.toHaveBeenCalled();
  });

  it("loads an existing record in edit mode and saves via api.updateRecord", async () => {
    getKType.mockResolvedValue(KTYPE);
    getRecord.mockResolvedValue(
      makeKRecord({ id: "rec-1", data: { title: "Existing deal", stage: "open" } }),
    );
    updateRecord.mockResolvedValue(makeKRecord({ id: "rec-1" }));
    const user = userEvent.setup();
    renderForm("/records/crm.deal/rec-1");

    expect(await screen.findByText(/Edit Deal/i)).toBeInTheDocument();
    // The form is seeded from the loaded record.
    expect(screen.getByDisplayValue("Existing deal")).toBeInTheDocument();
    // Edit mode exposes the print actions.
    expect(
      screen.getByRole("button", { name: /Download PDF/i }),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /^Save$/i }));
    await waitFor(() => expect(updateRecord).toHaveBeenCalledTimes(1));
    const [ktypeArg, idArg] = updateRecord.mock.calls[0]!;
    expect(ktypeArg).toBe("crm.deal");
    expect(idArg).toBe("rec-1");
    expect(createRecord).not.toHaveBeenCalled();
  });

  it("shows a not-found message when the KType does not exist", async () => {
    getKType.mockResolvedValue(undefined);
    renderForm("/records/crm.deal");
    expect(
      await screen.findByText(/couldn't find that record type/i),
    ).toBeInTheDocument();
  });

  it("shows a not-found message when the record id resolves to nothing", async () => {
    getKType.mockResolvedValue(KTYPE);
    getRecord.mockResolvedValue(undefined);
    renderForm("/records/crm.deal/missing");
    expect(
      await screen.findByText(/couldn't find that record$/i),
    ).toBeInTheDocument();
  });
});
