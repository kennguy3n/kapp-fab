import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
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

    expect(await screen.findByRole("heading", { name: /Dashboard/i })).toBeInTheDocument();

    // The pipeline value sits inside the "Pipeline $145,000"
    // subtitle string, so match with a regex; the AR/AP tiles
    // render the formatted amount as the sole value text so an
    // exact match is fine there.
    expect(screen.getByText(/Pipeline \$145,000/)).toBeInTheDocument();
    expect(screen.getByText("$23,500")).toBeInTheDocument();
    expect(screen.getByText("$8,100")).toBeInTheDocument();

    // Bare counts (open deals, low stock, approvals, tickets,
    // attendance, reviews) render with the plain number formatter.
    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText("4")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("7")).toBeInTheDocument();
    expect(screen.getByText("18")).toBeInTheDocument();
    expect(screen.getByText("5")).toBeInTheDocument();

    // Overdue subtitle pulls a count of 2 from the payload.
    expect(screen.getByText(/2 overdue/i)).toBeInTheDocument();
    // Outstanding AR / AP tiles include the "in USD" subline.
    expect(screen.getAllByText(/in USD/).length).toBeGreaterThanOrEqual(2);
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
    // The string sits inside the "Pipeline 999,999" subtitle so match
    // via regex; the absence of "$" before the digits is the actual
    // assertion (the bare-number branch is hit).
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

    // The tile is a react-router <Link>, so the rendered DOM has a
    // real anchor; verify it points at the expected deep view.
    const ar = screen.getByText(/Outstanding AR/).closest("a");
    expect(ar).toHaveAttribute("href", "/records/finance.ar_invoice");
    const ap = screen.getByText(/Outstanding AP/).closest("a");
    expect(ap).toHaveAttribute("href", "/records/finance.ap_bill");
    const tickets = screen.getByText(/Open tickets/).closest("a");
    expect(tickets).toHaveAttribute("href", "/helpdesk");
  });
});
