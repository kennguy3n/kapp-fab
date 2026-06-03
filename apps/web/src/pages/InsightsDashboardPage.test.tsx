import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type {
  InsightsDashboardBundle,
  InsightsWidget,
} from "@kapp/client";
import { makeInsightsDashboard, makeInsightsQuery } from "../test/factories";

const listInsightsDashboards = vi.fn();
const listInsightsQueries = vi.fn();
const getInsightsDashboard = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listInsightsDashboards: (...a: unknown[]) => listInsightsDashboards(...a),
    listInsightsQueries: (...a: unknown[]) => listInsightsQueries(...a),
    getInsightsDashboard: (...a: unknown[]) => getInsightsDashboard(...a),
    createInsightsDashboard: vi.fn(),
    updateInsightsDashboard: vi.fn(),
    deleteInsightsDashboard: vi.fn(),
    upsertInsightsWidget: vi.fn(),
    deleteInsightsWidget: vi.fn(),
    runInsightsQuery: vi.fn(),
  },
}));

// recharts is heavy and irrelevant to the dashboard plumbing under
// test — stub Viz to a marker and ShareModal to nothing, mirroring the
// QueryBuilder test's approach.
vi.mock("../components/insights/Charts", () => ({
  Viz: ({ vizType }: { vizType: string }) => (
    <div data-testid="viz">{vizType}</div>
  ),
}));
vi.mock("../components/insights/ShareModal", () => ({
  ShareModal: () => null,
}));

import { InsightsDashboardPage } from "./InsightsDashboardPage";

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <InsightsDashboardPage />
    </QueryClientProvider>,
  );
}

function widget(over: Partial<InsightsWidget> = {}): InsightsWidget {
  return {
    id: "w-1",
    tenant_id: "t-1",
    dashboard_id: "dash-1",
    query_id: "q-1",
    viz_type: "bar",
    position: { x: 0, y: 0, w: 6, h: 4 },
    config: { title: "Deals by stage" },
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-01T00:00:00Z",
    ...over,
  };
}

describe("InsightsDashboardPage", () => {
  beforeEach(() => {
    listInsightsDashboards.mockReset();
    listInsightsQueries.mockReset();
    getInsightsDashboard.mockReset();
  });

  it("prompts the user to pick a dashboard when none is selected", async () => {
    listInsightsDashboards.mockResolvedValue({ dashboards: [] });
    listInsightsQueries.mockResolvedValue({ queries: [] });
    renderPage();

    expect(
      await screen.findByText(/Select or create a dashboard to start/i),
    ).toBeInTheDocument();
    // Nothing should have been fetched for a (non-existent) selection.
    expect(getInsightsDashboard).not.toHaveBeenCalled();
  });

  it("lists the tenant's dashboards in the sidebar", async () => {
    listInsightsDashboards.mockResolvedValue({
      dashboards: [
        makeInsightsDashboard({ id: "dash-1", name: "Sales overview" }),
        makeInsightsDashboard({ id: "dash-2", name: "Ops health" }),
      ],
    });
    listInsightsQueries.mockResolvedValue({ queries: [] });
    renderPage();

    expect(
      await screen.findByRole("button", { name: "Sales overview" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Ops health" }),
    ).toBeInTheDocument();
  });

  it("loads the selected dashboard bundle and renders its widget via Viz", async () => {
    const dash = makeInsightsDashboard({
      id: "dash-1",
      name: "Sales overview",
      widgets: [widget()],
    });
    listInsightsDashboards.mockResolvedValue({ dashboards: [dash] });
    listInsightsQueries.mockResolvedValue({
      queries: [makeInsightsQuery({ id: "q-1", name: "Deals" })],
    });
    const bundle: InsightsDashboardBundle = {
      dashboard: dash,
      widget_results: {
        "w-1": {
          result: { columns: ["stage", "count"], rows: [{ stage: "open", count: 3 }] },
          cache_hit: false,
          query_hash: "h",
          filter_hash: "f",
        },
      },
    };
    getInsightsDashboard.mockResolvedValue(bundle);

    const user = userEvent.setup();
    renderPage();
    await user.click(
      await screen.findByRole("button", { name: "Sales overview" }),
    );

    await waitFor(() =>
      expect(getInsightsDashboard).toHaveBeenCalledWith("dash-1"),
    );
    expect(await screen.findByText("Deals by stage")).toBeInTheDocument();
    // The stubbed Viz renders the resolved viz_type.
    expect(screen.getByTestId("viz")).toHaveTextContent("bar");
  });

  it("shows the bind prompt for a widget with no query attached", async () => {
    const dash = makeInsightsDashboard({
      id: "dash-1",
      name: "Empty board",
      widgets: [widget({ id: "w-9", query_id: null, config: { title: "Blank" } })],
    });
    listInsightsDashboards.mockResolvedValue({ dashboards: [dash] });
    listInsightsQueries.mockResolvedValue({ queries: [] });
    getInsightsDashboard.mockResolvedValue({
      dashboard: dash,
      widget_results: {},
    });

    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole("button", { name: "Empty board" }));

    expect(
      await screen.findByText(/Bind this widget to a saved query\./i),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("viz")).not.toBeInTheDocument();
  });
});
