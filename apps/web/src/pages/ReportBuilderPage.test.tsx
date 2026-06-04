import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listReports = vi.fn();
const runAdhocReport = vi.fn();
const createReport = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listReports: (...a: unknown[]) => listReports(...a),
    runAdhocReport: (...a: unknown[]) => runAdhocReport(...a),
    createReport: (...a: unknown[]) => createReport(...a),
  },
}));

import { ReportBuilderPage } from "./ReportBuilderPage";
import { renderWithProviders } from "../test-utils";

const SAVED = {
  id: "rep-1",
  name: "Pipeline by stage",
  description: "deals grouped by stage",
  definition: { source: "ktype:crm.deal", columns: ["stage", "value"], filters: [], group_by: ["stage"], aggregations: [], sort: [], limit: 50 },
};

describe("ReportBuilderPage", () => {
  beforeEach(() => {
    listReports.mockReset();
    runAdhocReport.mockReset();
    createReport.mockReset();
    listReports.mockResolvedValue({ reports: [] });
  });

  it("renders the blank definition and empty saved list", async () => {
    renderWithProviders(<ReportBuilderPage />);
    expect(screen.getByRole("heading", { name: "Report Builder" })).toBeInTheDocument();
    expect(await screen.findByText("No saved reports yet.")).toBeInTheDocument();
    // The editor seeds the blank crm.deal definition.
    expect(screen.getByDisplayValue(/ktype:crm\.deal/)).toBeInTheDocument();
  });

  it("loads a saved report into the editor when clicked", async () => {
    listReports.mockResolvedValue({ reports: [SAVED] });
    const user = userEvent.setup();
    renderWithProviders(<ReportBuilderPage />);
    await user.click(await screen.findByRole("button", { name: "Pipeline by stage" }));

    expect(screen.getByDisplayValue("Pipeline by stage")).toBeInTheDocument();
    expect(screen.getByDisplayValue("deals grouped by stage")).toBeInTheDocument();
  });

  it("runs the report and renders the result grid", async () => {
    runAdhocReport.mockResolvedValue({
      columns: ["stage", "value", "meta"],
      rows: [
        { stage: "open", value: 1000, meta: { tag: "a" } },
        { stage: "won", value: 2000, meta: null },
      ],
    });
    const user = userEvent.setup();
    renderWithProviders(<ReportBuilderPage />);
    await user.click(screen.getByRole("button", { name: "Run" }));

    expect(await screen.findByText("Result (2 rows)")).toBeInTheDocument();
    // Object cells are JSON-stringified by formatCell; null renders empty.
    expect(screen.getByText('{"tag":"a"}')).toBeInTheDocument();
    expect(runAdhocReport).toHaveBeenCalledWith(
      expect.objectContaining({ source: "ktype:crm.deal" }),
    );
  });

  it("reports invalid JSON without calling the server", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ReportBuilderPage />);
    const editor = screen.getByDisplayValue(/ktype:crm\.deal/);
    await user.clear(editor);
    await user.type(editor, "not valid json");
    await user.click(screen.getByRole("button", { name: "Run" }));

    expect(await screen.findByText(/Invalid JSON:/i)).toBeInTheDocument();
    expect(runAdhocReport).not.toHaveBeenCalled();
  });

  it("surfaces a server error from the runner", async () => {
    runAdhocReport.mockRejectedValue(new Error("400 unknown column"));
    const user = userEvent.setup();
    renderWithProviders(<ReportBuilderPage />);
    await user.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByText(/400 unknown column/)).toBeInTheDocument();
  });

  it("saves a named report definition", async () => {
    createReport.mockResolvedValue(SAVED);
    const user = userEvent.setup();
    renderWithProviders(<ReportBuilderPage />);

    // Save is disabled until the report has a name.
    const save = screen.getByRole("button", { name: "Save report" });
    expect(save).toBeDisabled();
    await user.type(screen.getByPlaceholderText("report name"), "My report");
    expect(save).toBeEnabled();
    await user.click(save);

    await waitFor(() =>
      expect(createReport).toHaveBeenCalledWith(
        expect.objectContaining({ name: "My report", definition: expect.any(Object) }),
      ),
    );
  });
});
