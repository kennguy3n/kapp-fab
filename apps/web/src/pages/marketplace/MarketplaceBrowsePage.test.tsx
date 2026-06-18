import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

const listMarketplaceExtensions = vi.fn();

vi.mock("../../lib/api", () => ({
  api: {
    listMarketplaceExtensions: (...a: unknown[]) =>
      listMarketplaceExtensions(...a),
  },
}));

import { MarketplaceBrowsePage } from "./MarketplaceBrowsePage";

const FIXTURE = {
  items: [
    {
      id: "ext-1",
      name: "acme.inventory-sync",
      publisher: "acme",
      slug: "inventory-sync",
      display_name: "Inventory Sync",
      description: "Syncs stock levels with external WMS feeds.",
      author: "Acme Corp",
      license: "MIT",
      status: "listed" as const,
      listed_version: "1.2.0",
      category: "inventory" as const,
      screenshots: [],
      rating_average: 4.5,
      rating_count: 12,
      created_at: "2025-01-01T00:00:00Z",
      updated_at: "2025-02-01T00:00:00Z",
    },
    {
      id: "ext-2",
      name: "acme.billing",
      publisher: "acme",
      slug: "billing",
      display_name: "Billing Connector",
      description: "Push invoices to QuickBooks.",
      author: "Acme Corp",
      license: "Apache-2.0",
      status: "deprecated" as const,
      category: "finance" as const,
      screenshots: [],
      rating_average: 0,
      rating_count: 0,
      created_at: "2024-06-01T00:00:00Z",
      updated_at: "2024-09-01T00:00:00Z",
    },
  ],
};

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/marketplace"]}>
        <MarketplaceBrowsePage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("MarketplaceBrowsePage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders catalogue entries returned by listMarketplaceExtensions", async () => {
    listMarketplaceExtensions.mockResolvedValueOnce(FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(screen.getByText("Inventory Sync")).toBeInTheDocument(),
    );
    expect(screen.getByText("Billing Connector")).toBeInTheDocument();
    // Listed badge for the active extension, Deprecated for the other.
    expect(screen.getByText("Listed")).toBeInTheDocument();
    expect(screen.getByText("Deprecated")).toBeInTheDocument();
  });

  it("debounces the search input and forwards q into the query opts", async () => {
    listMarketplaceExtensions.mockResolvedValue(FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(listMarketplaceExtensions).toHaveBeenLastCalledWith({
        q: undefined,
        publisher: undefined,
      }),
    );
    const search = screen.getByPlaceholderText(/search extensions/i);
    await userEvent.type(search, "billing");
    await waitFor(
      () => {
        expect(listMarketplaceExtensions).toHaveBeenLastCalledWith({
          q: "billing",
          publisher: undefined,
        });
      },
      { timeout: 1000 },
    );
  });

  it("forwards the selected category into the query opts", async () => {
    listMarketplaceExtensions.mockResolvedValue(FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(screen.getByText("Inventory Sync")).toBeInTheDocument(),
    );
    await userEvent.selectOptions(
      screen.getByLabelText(/filter by category/i),
      "inventory",
    );
    await waitFor(() =>
      expect(listMarketplaceExtensions).toHaveBeenLastCalledWith({
        q: undefined,
        publisher: undefined,
        category: "inventory",
      }),
    );
  });

  it("shows the rating summary on cards and teaches the unrated case", async () => {
    listMarketplaceExtensions.mockResolvedValueOnce(FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(screen.getByText("Inventory Sync")).toBeInTheDocument(),
    );
    // Rated listing shows the average + humanised count.
    expect(
      screen.getByLabelText("Rated 4.5 out of 5 from 12 ratings"),
    ).toBeInTheDocument();
    // Unrated listing reads as an invitation, never a bare 0.
    expect(screen.getAllByText(/no ratings yet/i).length).toBeGreaterThan(0);
  });

  it("renders the humanised category label, never the raw token", async () => {
    listMarketplaceExtensions.mockResolvedValueOnce(FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(screen.getByText("Inventory Sync")).toBeInTheDocument(),
    );
    // "Inventory"/"Finance" appear both as card badges and as
    // category-filter options, so assert presence (not uniqueness).
    expect(screen.getAllByText("Inventory").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Finance").length).toBeGreaterThan(0);
    // The raw snake_case token must never leak to the UI.
    expect(screen.queryByText("inventory")).not.toBeInTheDocument();
    expect(screen.queryByText("developer_tools")).not.toBeInTheDocument();
  });

  it("shows an empty-state message when the API returns no items", async () => {
    listMarketplaceExtensions.mockResolvedValueOnce({ items: [] });
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/no extensions/i)).toBeInTheDocument(),
    );
  });

  it("renders an error banner when the request fails", async () => {
    listMarketplaceExtensions.mockRejectedValueOnce(new Error("503 boom"));
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/503 boom/)).toBeInTheDocument(),
    );
  });
});
