import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getKType = vi.fn();
const listRecords = vi.fn();
const listViews = vi.fn();
const createView = vi.fn();
const deleteView = vi.fn();
const bulkRecords = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    getKType: (...a: unknown[]) => getKType(...a),
    listRecords: (...a: unknown[]) => listRecords(...a),
    listViews: (...a: unknown[]) => listViews(...a),
    createView: (...a: unknown[]) => createView(...a),
    deleteView: (...a: unknown[]) => deleteView(...a),
    bulkRecords: (...a: unknown[]) => bulkRecords(...a),
    bulkExportRecords: vi.fn(),
    updateRecord: vi.fn(),
    runAction: vi.fn(),
  },
}));

import { RecordListPage } from "./RecordListPage";

const FIXTURE_KTYPE = {
  name: "crm.deal",
  schema: {
    fields: [
      { name: "title", type: "string" },
      { name: "stage", type: "string" },
      { name: "value", type: "number" },
    ],
    views: {
      list: { columns: ["title", "stage", "value"] },
    },
  },
};

function row(
  id: string,
  title: string,
  stage: string,
  value: number,
): { id: string; data: Record<string, unknown> } {
  return {
    id,
    data: { title, stage, value },
  };
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/records/crm.deal"]}>
        <Routes>
          <Route path="/records/:ktype" element={<RecordListPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RecordListPage", () => {
  beforeEach(() => {
    getKType.mockReset();
    listRecords.mockReset();
    listViews.mockReset();
    createView.mockReset();
    deleteView.mockReset();
    bulkRecords.mockReset();
  });

  it("renders every record when no saved view is active", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([
      row("r1", "Acme", "open", 100),
      row("r2", "Beta", "won", 250),
      row("r3", "Gamma", "lost", 75),
    ]);
    listViews.mockResolvedValue([]);
    renderPage();

    expect(await screen.findByText("Acme")).toBeInTheDocument();
    expect(screen.getByText("Beta")).toBeInTheDocument();
    expect(screen.getByText("Gamma")).toBeInTheDocument();
  });

  it("applies the default saved view's filter to hide non-matching rows", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([
      row("r1", "Acme", "open", 100),
      row("r2", "Beta", "won", 250),
      row("r3", "Gamma", "lost", 75),
    ]);
    listViews.mockResolvedValue([
      {
        id: "v-default",
        ktype: "crm.deal",
        name: "Open only",
        filters: { stage: "open" },
        sort: "",
        is_default: true,
        shared: false,
      },
    ]);
    renderPage();
    // The default view matches stage=open → Acme survives, Beta/
    // Gamma are filtered out. We wait for the dropdown to read
    // "Open only (default)" then assert the table contents.
    expect(await screen.findByText("Acme")).toBeInTheDocument();
    expect(screen.queryByText("Beta")).toBeNull();
    expect(screen.queryByText("Gamma")).toBeNull();
  });

  it("applies the saved view's sort spec to order the rendered rows", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([
      row("r1", "Charlie", "open", 100),
      row("r2", "Alpha", "open", 200),
      row("r3", "Bravo", "open", 150),
    ]);
    listViews.mockResolvedValue([
      {
        id: "v-sort",
        ktype: "crm.deal",
        name: "By title",
        filters: {},
        sort: "title",
        is_default: true,
        shared: false,
      },
    ]);
    renderPage();

    // Wait for rendering, then read the rendered <tr> order. The
    // sort spec is "title" ascending, so the rendered order should
    // be Alpha → Bravo → Charlie even though the API returned
    // Charlie → Alpha → Bravo.
    await screen.findByText("Alpha");
    // The KTypeList renders a leading checkbox column when bulk
    // select is wired, so the title cell is at index 1.
    const tbody = document.querySelector("tbody")!;
    const titles = within(tbody as HTMLElement)
      .getAllByRole("row")
      .map((tr) => tr.children[1]!.textContent);
    expect(titles).toEqual(["Alpha", "Bravo", "Charlie"]);
  });

  it("descending sort with `-value` reverses numeric ordering", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([
      row("r1", "Cheap", "open", 50),
      row("r2", "Pricey", "open", 999),
      row("r3", "Middle", "open", 300),
    ]);
    listViews.mockResolvedValue([
      {
        id: "v-desc",
        ktype: "crm.deal",
        name: "By value desc",
        filters: {},
        sort: "-value",
        is_default: true,
        shared: false,
      },
    ]);
    renderPage();
    await screen.findByText("Pricey");
    const tbody = document.querySelector("tbody")!;
    const titles = within(tbody as HTMLElement)
      .getAllByRole("row")
      .map((tr) => tr.children[1]!.textContent);
    expect(titles).toEqual(["Pricey", "Middle", "Cheap"]);
  });

  it("array-valued filters match when the record's field is in the array", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([
      row("r1", "Acme", "open", 100),
      row("r2", "Beta", "won", 250),
      row("r3", "Gamma", "lost", 75),
    ]);
    listViews.mockResolvedValue([
      {
        id: "v-multi",
        ktype: "crm.deal",
        name: "Open or won",
        // Multi-value filter: the record's stage must equal one
        // of the array members. Gamma (stage=lost) is filtered
        // out; Acme + Beta survive.
        filters: { stage: ["open", "won"] },
        sort: "",
        is_default: true,
        shared: false,
      },
    ]);
    renderPage();
    expect(await screen.findByText("Acme")).toBeInTheDocument();
    expect(screen.getByText("Beta")).toBeInTheDocument();
    expect(screen.queryByText("Gamma")).toBeNull();
  });

  it("renders the bulk-action toolbar after a row checkbox is ticked", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([
      row("r1", "Acme", "open", 100),
      row("r2", "Beta", "won", 250),
    ]);
    listViews.mockResolvedValue([]);
    renderPage();
    await screen.findByText("Acme");

    const user = userEvent.setup();
    await user.click(screen.getByLabelText("Select row r1"));
    // Toolbar is sticky with role="toolbar" and label "Bulk actions".
    const toolbar = await screen.findByRole("toolbar", { name: /Bulk actions/i });
    expect(within(toolbar).getByText(/1 selected/i)).toBeInTheDocument();
    expect(within(toolbar).getByRole("button", { name: /Change Status/i })).toBeInTheDocument();
    expect(within(toolbar).getByRole("button", { name: /^Delete$/i })).toBeInTheDocument();
    expect(within(toolbar).getByRole("button", { name: /Export CSV/i })).toBeInTheDocument();
  });

  it("Select-all toggles every visible row at once", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([
      row("r1", "Acme", "open", 100),
      row("r2", "Beta", "won", 250),
      row("r3", "Gamma", "lost", 75),
    ]);
    listViews.mockResolvedValue([]);
    renderPage();
    await screen.findByText("Acme");

    const user = userEvent.setup();
    await user.click(screen.getByLabelText(/Select all rows/i));
    await waitFor(() =>
      expect(screen.getByText(/3 selected/i)).toBeInTheDocument(),
    );
  });

  it("Save view prompts for a name and POSTs through api.createView", async () => {
    getKType.mockResolvedValue(FIXTURE_KTYPE);
    listRecords.mockResolvedValue([row("r1", "Acme", "open", 100)]);
    listViews.mockResolvedValue([]);
    createView.mockResolvedValue({
      id: "v-new",
      ktype: "crm.deal",
      name: "Mine",
      filters: {},
      sort: "",
      is_default: false,
      shared: false,
    });
    const promptSpy = vi.spyOn(window, "prompt").mockReturnValueOnce("Mine");
    renderPage();
    await screen.findByText("Acme");

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /Save view/i }));
    await waitFor(() => expect(createView).toHaveBeenCalledTimes(1));
    expect(createView).toHaveBeenCalledWith(
      expect.objectContaining({ ktype: "crm.deal", name: "Mine" }),
    );
    expect(promptSpy).toHaveBeenCalled();
    promptSpy.mockRestore();
  });
});
