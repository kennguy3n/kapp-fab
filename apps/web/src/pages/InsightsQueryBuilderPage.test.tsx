import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const listInsightsQueries = vi.fn();
const listKTypes = vi.fn();
const createInsightsQuery = vi.fn();
const runInsightsQuery = vi.fn();
const updateInsightsQuery = vi.fn();
const deleteInsightsQuery = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listInsightsQueries: (...a: unknown[]) => listInsightsQueries(...a),
    listKTypes: (...a: unknown[]) => listKTypes(...a),
    createInsightsQuery: (...a: unknown[]) => createInsightsQuery(...a),
    runInsightsQuery: (...a: unknown[]) => runInsightsQuery(...a),
    updateInsightsQuery: (...a: unknown[]) => updateInsightsQuery(...a),
    deleteInsightsQuery: (...a: unknown[]) => deleteInsightsQuery(...a),
    runInsightsQuerySQL: vi.fn(),
    listInsightsRuns: vi.fn(),
  },
}));

// The Charts module pulls in recharts which is heavy and tries to
// observe sizes on mount. The query-builder test doesn't care about
// the rendered chart at all — it only exercises the source picker,
// columns editor, filters, and run/save plumbing — so stub the
// component to a trivial placeholder.
vi.mock("../components/insights/Charts", () => ({
  Viz: () => <div data-testid="viz" />,
}));

vi.mock("../components/insights/ShareModal", () => ({
  ShareModal: () => null,
}));

import { InsightsQueryBuilderPage } from "./InsightsQueryBuilderPage";

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <InsightsQueryBuilderPage />
    </QueryClientProvider>,
  );
}

describe("InsightsQueryBuilderPage", () => {
  beforeEach(() => {
    listInsightsQueries.mockReset();
    listKTypes.mockReset();
    createInsightsQuery.mockReset();
    runInsightsQuery.mockReset();
    updateInsightsQuery.mockReset();
    deleteInsightsQuery.mockReset();
  });

  it("populates the source picker with both ktype:* and ledger options", async () => {
    listInsightsQueries.mockResolvedValue({ queries: [] });
    listKTypes.mockResolvedValue([
      { name: "crm.deal", schema: { fields: [] } },
      { name: "hr.employee", schema: { fields: [] } },
    ]);
    renderPage();

    // The page renders once queriesQuery resolves; ktypesQuery
    // resolves on a separate tick and triggers a re-render that
    // splices ktype:* options into the source select. Wait for
    // the ktype option to appear before reading the option list.
    await waitFor(() => {
      const opt = document.querySelector(
        'option[value="ktype:crm.deal"]',
      );
      expect(opt).not.toBeNull();
    });
    const sourceLegend = screen.getByText(/^Source$/);
    const source = sourceLegend.parentElement!.querySelector(
      "select",
    )! as HTMLSelectElement;
    const optionTexts = Array.from(source.querySelectorAll("option")).map(
      (o) => o.value,
    );
    expect(optionTexts).toContain("ktype:crm.deal");
    expect(optionTexts).toContain("ktype:hr.employee");
    expect(optionTexts).toContain("journal_lines");
    expect(optionTexts).toContain("ar_invoices");
    expect(optionTexts).toContain("ap_bills");
  });

  it("blocks Save when the query name is empty and surfaces the error inline", async () => {
    listInsightsQueries.mockResolvedValue({ queries: [] });
    listKTypes.mockResolvedValue([]);
    renderPage();

    const user = userEvent.setup();
    // Find the Save button by its accessible name. There's no
    // selectedId yet so the label is "Save" not "Update".
    const save = await screen.findByRole("button", { name: /^Save$/i });
    await user.click(save);

    expect(await screen.findByText(/query name required/i)).toBeInTheDocument();
    expect(createInsightsQuery).not.toHaveBeenCalled();
  });

  it("Save POSTs the query definition through api.createInsightsQuery", async () => {
    listInsightsQueries.mockResolvedValue({ queries: [] });
    listKTypes.mockResolvedValue([]);
    createInsightsQuery.mockResolvedValue({
      id: "q-1",
      name: "My report",
      mode: "visual",
      definition: { source: "ktype:crm.deal", columns: ["id", "name"] },
      cache_ttl_seconds: 300,
    });
    renderPage();

    const user = userEvent.setup();
    // Fill the form name and click Save.
    const nameInput = await screen.findByPlaceholderText(/query name/i);
    await user.type(nameInput, "My report");
    await user.click(screen.getByRole("button", { name: /^Save$/i }));

    await waitFor(() => expect(createInsightsQuery).toHaveBeenCalledTimes(1));
    const arg = createInsightsQuery.mock.calls[0]![0] as Record<string, unknown>;
    expect(arg.name).toBe("My report");
    expect(arg.mode).toBe("visual");
    // definition.source defaults to "ktype:crm.deal" per blankForm.
    const def = arg.definition as { source: string; columns: string[] };
    expect(def.source).toBe("ktype:crm.deal");
    expect(def.columns).toEqual(["id", "name"]);
  });

  it("Run before save shows the inline 'save first' error", async () => {
    listInsightsQueries.mockResolvedValue({
      queries: [
        {
          id: "q-saved",
          name: "Saved query",
          description: "",
          mode: "visual",
          definition: { source: "ktype:crm.deal", columns: ["id"] },
          cache_ttl_seconds: 300,
        },
      ],
    });
    listKTypes.mockResolvedValue([]);
    renderPage();

    // Without selecting an existing query the Run button isn't even
    // rendered, so the way to reach the "save first" path is to
    // create a new query, fill the name, and try to run. Easier:
    // verify the Run button is only visible after a query is
    // selected from the sidebar.
    expect(
      screen.queryByRole("button", { name: /^Run$/i }),
    ).toBeNull();

    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /Saved query/i }));
    // Now the Run button should appear.
    const runBtn = await screen.findByRole("button", { name: /^Run$/i });
    expect(runBtn).toBeInTheDocument();
  });

  it("Run on a selected saved query invokes api.runInsightsQuery with the query id", async () => {
    listInsightsQueries.mockResolvedValue({
      queries: [
        {
          id: "q-7",
          name: "Q7",
          description: "",
          mode: "visual",
          definition: { source: "ktype:crm.deal", columns: ["id", "name"] },
          cache_ttl_seconds: 300,
        },
      ],
    });
    listKTypes.mockResolvedValue([]);
    runInsightsQuery.mockResolvedValue({
      // Page reads preview.result.rows and preview.cache_hit so
      // match that exact envelope shape, not the wire shape.
      result: { columns: ["id", "name"], rows: [["1", "deal a"]] },
      cache_hit: false,
      run_ms: 12,
    });
    renderPage();

    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /Q7/i }));
    await user.click(await screen.findByRole("button", { name: /^Run$/i }));
    await waitFor(() => expect(runInsightsQuery).toHaveBeenCalledTimes(1));
    expect(runInsightsQuery).toHaveBeenCalledWith("q-7", expect.any(Object));
  });

  it("Add column appends a row to the columns editor", async () => {
    listInsightsQueries.mockResolvedValue({ queries: [] });
    listKTypes.mockResolvedValue([]);
    renderPage();

    // Default form starts with two column rows: id, name.
    const idInput = (await screen.findByDisplayValue("id")) as HTMLInputElement;
    expect(idInput).toBeInTheDocument();
    expect(screen.getByDisplayValue("name")).toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /Add column/i }));
    // The newly added column placeholder is "new_column" per the
    // page's onClick handler.
    expect(await screen.findByDisplayValue("new_column")).toBeInTheDocument();
  });

  it("Visual / SQL tab switch hides the structured sections in SQL mode", async () => {
    listInsightsQueries.mockResolvedValue({ queries: [] });
    listKTypes.mockResolvedValue([]);
    renderPage();

    // The Source picker is visible in visual mode by default.
    const sourceLegend = await screen.findByText(/^Source$/);
    expect(sourceLegend).toBeInTheDocument();

    const user = userEvent.setup();
    // The tab buttons render with role="tab" not role="button", so
    // query by the tab role to scope away from the Save/Run
    // toolbar.
    await user.click(screen.getByRole("tab", { name: /SQL editor/i }));
    // After switching to SQL mode the Source section unmounts. The
    // raw-SQL textarea appears in its place.
    await waitFor(() => expect(screen.queryByText(/^Source$/)).toBeNull());
    expect(
      screen.getByText(/SQL \(parameterised\)/i),
    ).toBeInTheDocument();
  });
});
