import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import type { TenantFeaturesResponse } from "@kapp/client";

// AppShell gates the sidebar on api.listTenantFeatures and renders the
// "/" route (DashboardPage, lazy-loaded) which reads
// api.getDashboardSummary. Both go through ../lib/api, so a single
// module mock drives the whole shell. NotificationBell + the
// notifications poll go through raw fetch and are answered by MSW.
const listTenantFeatures = vi.fn();
const getDashboardSummary = vi.fn();
const searchRecords = vi.fn();

vi.mock("./lib/api", () => ({
  api: {
    listTenantFeatures: (...a: unknown[]) => listTenantFeatures(...a),
    getDashboardSummary: (...a: unknown[]) => getDashboardSummary(...a),
    searchRecords: (...a: unknown[]) => searchRecords(...a),
  },
}));

import { App } from "./App";

function features(map: Record<string, boolean>): TenantFeaturesResponse {
  return { features: map } as TenantFeaturesResponse;
}

function renderApp(initialEntry = "/") {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const EMPTY_SUMMARY = {
  base_currency: "USD",
  open_deals_count: 0,
  pipeline_value: 0,
  outstanding_ar: 0,
  outstanding_ap: 0,
  low_stock_items_count: 0,
  pending_approvals: 0,
  open_tickets_count: 0,
  overdue_tickets_count: 0,
  present_today: 0,
  pending_reviews: 0,
};

describe("App shell", () => {
  beforeEach(() => {
    listTenantFeatures.mockReset();
    getDashboardSummary.mockReset();
    searchRecords.mockReset();
    getDashboardSummary.mockResolvedValue(EMPTY_SUMMARY);
    searchRecords.mockResolvedValue({ results: [] });
    localStorage.clear();
    localStorage.setItem("kapp.tenant", "acme");
  });

  it("renders every nav section when the tenant has all features enabled", async () => {
    listTenantFeatures.mockResolvedValue(
      features({
        crm: true,
        finance: true,
        inventory: true,
        hr: true,
        insights: true,
      }),
    );
    renderApp();

    // Group headers are gated section titles; their presence proves
    // the feature filter let them through.
    expect(await screen.findByText("CRM")).toBeInTheDocument();
    expect(screen.getByText("Finance")).toBeInTheDocument();
    expect(screen.getByText("Inventory")).toBeInTheDocument();
    expect(screen.getByText("HR")).toBeInTheDocument();
    // Overview is ungated and always present.
    expect(screen.getByText("Overview")).toBeInTheDocument();
    // A representative link inside a section.
    expect(screen.getByRole("link", { name: "Leads" })).toBeInTheDocument();
  });

  it("hides sections whose gating feature is disabled", async () => {
    listTenantFeatures.mockResolvedValue(
      features({ crm: false, finance: false, inventory: true }),
    );
    renderApp();

    // Inventory stays (enabled) — wait for the shell to settle on it.
    expect(await screen.findByText("Inventory")).toBeInTheDocument();
    // CRM + Finance are disabled, so once the features query resolves
    // their group headers must drop out. waitFor bridges the brief
    // fail-open window before the query settles (data === undefined
    // shows every section).
    await waitFor(() =>
      expect(screen.queryByText("CRM")).not.toBeInTheDocument(),
    );
    expect(screen.queryByText("Finance")).not.toBeInTheDocument();
    // Overview (ungated) survives regardless.
    expect(screen.getByText("Overview")).toBeInTheDocument();
  });

  it("fails open and shows every section when the features query rejects", async () => {
    listTenantFeatures.mockRejectedValue(new Error("network down"));
    renderApp();

    // With no features data the gate is bypassed: even normally-gated
    // sections render so a transient API blip can't hide the whole app.
    expect(await screen.findByText("CRM")).toBeInTheDocument();
    expect(screen.getByText("Finance")).toBeInTheDocument();
    expect(screen.getByText("Inventory")).toBeInTheDocument();
  });

  it("applies per-link gating: Landed Costs needs finance on top of inventory", async () => {
    listTenantFeatures.mockResolvedValue(
      features({ inventory: true, finance: false }),
    );
    renderApp();

    // Section visible because inventory is enabled…
    expect(await screen.findByText("Inventory")).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "Stock Levels" }),
    ).toBeInTheDocument();
    // …but the Landed Costs link declares requires:["finance"], which
    // is disabled, so it must be filtered out once the query resolves.
    await waitFor(() =>
      expect(
        screen.queryByRole("link", { name: "Landed Costs" }),
      ).not.toBeInTheDocument(),
    );
  });

  it("renders the global search box and routes a query to /search", async () => {
    listTenantFeatures.mockResolvedValue(features({ crm: true }));
    const user = userEvent.setup();
    renderApp();

    // The shell search box now follows the ARIA combobox pattern: it
    // controls a popup listbox (recent searches / quick results) via
    // aria-expanded + aria-controls, so its role is combobox rather
    // than the plain searchbox it was before the G5 dropdown landed.
    const search = await screen.findByRole("combobox", {
      name: /global search/i,
    });
    await user.type(search, "acme corp{Enter}");

    // Submitting routes to /search?q=acme%20corp. SearchPage (lazy)
    // renders its "Search" heading and seeds its input from the URL's
    // ?q= param, so the echoed value proves the shell navigated with
    // the term intact — MemoryRouter never touches window.location, so
    // we assert on rendered route output instead.
    expect(
      await screen.findByRole("heading", { name: "Search" }),
    ).toBeInTheDocument();
    // SearchPage's own input (distinct from the shell search box) is
    // seeded from the ?q= param.
    expect(
      screen.getByPlaceholderText(/Search records by name/i),
    ).toHaveValue("acme corp");
    // And the debounced query fires against the navigated term.
    await waitFor(() =>
      expect(searchRecords).toHaveBeenCalledWith(
        expect.objectContaining({ q: "acme corp" }),
      ),
    );
  });

  it("highlights the first search option on the first ArrowDown after the panel was closed", async () => {
    // Regression: the cursor-reset effect used to key off both
    // `debounced` and `open`, so it fired on the same commit as the
    // ArrowDown handler (which opens the panel and advances the cursor
    // in one event) and reset activeIndex back to -1 — meaning the
    // first ArrowDown after a close reopened the panel but highlighted
    // nothing. Seed a recent search so the empty-query panel has an
    // option to navigate to.
    listTenantFeatures.mockResolvedValue(features({ crm: true }));
    localStorage.setItem(
      "kapp.recent_searches",
      JSON.stringify(["acme corp"]),
    );
    const user = userEvent.setup();
    renderApp();

    const search = await screen.findByRole("combobox", {
      name: /global search/i,
    });

    // Focus opens the panel (recent searches), then Escape closes it.
    await user.click(search);
    expect(
      await screen.findByRole("option", { name: /acme corp/i }),
    ).toHaveAttribute("aria-selected", "false");
    await user.keyboard("{Escape}");

    // A single ArrowDown must reopen and highlight the first option.
    await user.keyboard("{ArrowDown}");
    expect(
      await screen.findByRole("option", { name: /acme corp/i }),
    ).toHaveAttribute("aria-selected", "true");
  });

  it("renders the public login route without the tenant shell", async () => {
    renderApp("/login");

    // LoginPage exposes Tenant + Token fields.
    expect(await screen.findByLabelText(/^Tenant$/i)).toBeInTheDocument();
    // No authenticated sidebar chrome on the public route.
    expect(screen.queryByText("Overview")).not.toBeInTheDocument();
    expect(screen.queryByText("CRM")).not.toBeInTheDocument();
    // The features query must not even fire for an anonymous surface.
    expect(listTenantFeatures).not.toHaveBeenCalled();
  });
});
