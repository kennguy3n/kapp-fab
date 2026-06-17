import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

const getDashboardSummary = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    getDashboardSummary: (...args: unknown[]) => getDashboardSummary(...args),
  },
}));

// Replace useFormatter with a deterministic stub so the test doesn't
// depend on the host machine's Intl ICU data. The stub formats USD
// like the production en-US output the dashboard target was tuned
// for: $1,234 (no decimals) for currency, 1,234 for bare numbers.
vi.mock("../lib/i18n", () => ({
  useFormatter: () => ({
    currency: (n: number, currency: string) =>
      new Intl.NumberFormat("en-US", {
        style: "currency",
        currency,
        maximumFractionDigits: 0,
      }).format(n),
    number: (n: number) =>
      new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 }).format(n),
  }),
}));

import { DashboardPage } from "./DashboardPage";

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <DashboardPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// The KPI tile and the Action center can legitimately surface the same
// raw number (e.g. the pending-approvals count appears both as a KPI
// value and as an action row), so value assertions are scoped to the
// owning tile rather than matched globally. `tile(label)` returns the
// <a> that wraps a KPI card so `within` can assert on just that card.
function tile(label: string): HTMLElement {
  const anchor = screen.getByText(label).closest("a");
  if (!anchor) throw new Error(`No tile anchor for "${label}"`);
  return anchor as HTMLElement;
}

describe("DashboardPage", () => {
  beforeEach(() => {
    getDashboardSummary.mockReset();
  });

  it("renders every KPI tile with the formatted summary values", async () => {
    getDashboardSummary.mockResolvedValueOnce({
      base_currency: "USD",
      open_deals_count: 12,
      pipeline_value: 145_000,
      outstanding_ar: 23_500,
      outstanding_ap: 8_100,
      low_stock_items_count: 4,
      pending_approvals: 3,
      open_tickets_count: 7,
      overdue_tickets_count: 2,
      present_today: 18,
      pending_reviews: 5,
    });
    renderPage();

    // The page leads with a time-of-day greeting header (the tenant key
    // stands in for the user) instead of a literal "Dashboard" title;
    // match the greeting so the assertion is stable across the hour the
    // suite runs.
    expect(
      await screen.findByRole("heading", {
        name: /Good (morning|afternoon|evening)/i,
      }),
    ).toBeInTheDocument();

    // Wait on a data-dependent node so the remaining assertions see the
    // loaded tiles. The pipeline value sits inside the "Pipeline
    // $145,000" subtitle string on the Open deals tile.
    expect(await screen.findByText(/Pipeline \$145,000/)).toBeInTheDocument();

    // KPI values — scoped to their tile so the Action center counts
    // (which mirror some of the same numbers) don't make the lookup
    // ambiguous.
    expect(within(tile("Open deals")).getByText("12")).toBeInTheDocument();
    expect(within(tile("Outstanding AR")).getByText("$23,500")).toBeInTheDocument();
    expect(within(tile("Outstanding AP")).getByText("$8,100")).toBeInTheDocument();
    expect(within(tile("Low-stock items")).getByText("4")).toBeInTheDocument();
    expect(within(tile("Pending approvals")).getByText("3")).toBeInTheDocument();
    expect(within(tile("Open tickets")).getByText("7")).toBeInTheDocument();
    expect(within(tile("Present today")).getByText("18")).toBeInTheDocument();
    expect(within(tile("Pending reviews")).getByText("5")).toBeInTheDocument();

    // Overdue subtitle pulls a count of 2 from the payload.
    expect(within(tile("Open tickets")).getByText(/2 overdue/i)).toBeInTheDocument();
    // The hr.attendance ktype id must never leak as a subtitle.
    expect(screen.queryByText(/hr\.attendance/)).toBeNull();
    // Outstanding AR / AP tiles include the "in USD" subline.
    expect(screen.getAllByText(/in USD/).length).toBeGreaterThanOrEqual(2);
  });

  it("surfaces an Action center of the items that need attention", async () => {
    getDashboardSummary.mockResolvedValueOnce({
      base_currency: "USD",
      open_deals_count: 12,
      pipeline_value: 145_000,
      outstanding_ar: 23_500,
      outstanding_ap: 8_100,
      low_stock_items_count: 4,
      pending_approvals: 3,
      open_tickets_count: 7,
      overdue_tickets_count: 2,
      present_today: 18,
      pending_reviews: 5,
    });
    renderPage();

    const approvals = (
      await screen.findByText("Approvals to review")
    ).closest("a");
    expect(approvals).toHaveAttribute("href", "/approvals");
    expect(screen.getByText("Tickets overdue").closest("a")).toHaveAttribute(
      "href",
      "/helpdesk",
    );
    expect(screen.getByText("Items to reorder").closest("a")).toHaveAttribute(
      "href",
      "/inventory/stock-levels",
    );
  });

  it("shows an all-caught-up message when nothing needs attention", async () => {
    getDashboardSummary.mockResolvedValueOnce({
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
    });
    renderPage();
    expect(
      await screen.findByText(/You're all caught up/i),
    ).toBeInTheDocument();
  });

  it("falls back to plain-number formatting when the API omits a currency", async () => {
    getDashboardSummary.mockResolvedValueOnce({
      base_currency: "",
      open_deals_count: 0,
      pipeline_value: 999_999,
      outstanding_ar: 0,
      outstanding_ap: 0,
      low_stock_items_count: 0,
      pending_approvals: 0,
      open_tickets_count: 0,
      overdue_tickets_count: 0,
      present_today: 0,
      pending_reviews: 0,
    });
    renderPage();
    // currency="" hits the empty-string branch in formatAmount and
    // falls back to fmt.number, producing "999,999" (no leading $).
    expect(await screen.findByText(/Pipeline 999,999/)).toBeInTheDocument();
    expect(screen.queryByText(/Pipeline \$999,999/)).toBeNull();
  });

  it("renders the inline error banner when the summary query fails", async () => {
    getDashboardSummary.mockRejectedValueOnce(new Error("boom"));
    renderPage();
    expect(
      await screen.findByText(/Failed to load dashboard: boom/i),
    ).toBeInTheDocument();
  });

  it("links every tile to a deep route in the records app", async () => {
    getDashboardSummary.mockResolvedValueOnce({
      base_currency: "USD",
      open_deals_count: 1,
      pipeline_value: 100,
      outstanding_ar: 100,
      outstanding_ap: 100,
      low_stock_items_count: 1,
      pending_approvals: 1,
      open_tickets_count: 1,
      overdue_tickets_count: 0,
      present_today: 1,
      pending_reviews: 1,
    });
    renderPage();
    await screen.findByText(/Pipeline/);

    // The tile is a react-router <Link>, so the rendered DOM has a real
    // anchor; verify it points at the expected deep view.
    expect(tile("Outstanding AR")).toHaveAttribute(
      "href",
      "/records/finance.ar_invoice",
    );
    expect(tile("Outstanding AP")).toHaveAttribute(
      "href",
      "/records/finance.ap_bill",
    );
    expect(tile("Open tickets")).toHaveAttribute("href", "/helpdesk");
    expect(tile("Present today")).toHaveAttribute(
      "href",
      "/records/hr.attendance",
    );
  });
});
