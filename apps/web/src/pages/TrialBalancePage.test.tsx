import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import type { TrialBalanceReport } from "@kapp/client";
import { renderWithProviders } from "../test-utils";

// Mock the api module before importing the page so the page's
// top-level `import { api } from "../lib/api"` resolves to the stub.
const getTrialBalance = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    getTrialBalance: (...args: unknown[]) => getTrialBalance(...args),
  },
}));

import { TrialBalancePage } from "./TrialBalancePage";

const balancedReport: TrialBalanceReport = {
  tenant_id: "t1",
  as_of: "2025-01-31",
  rows: [
    {
      account_code: "1000",
      account_name: "Cash",
      type: "asset",
      debit: "120000.00",
      credit: "0",
      balance: "120000.00",
    },
    {
      account_code: "3000",
      account_name: "Share capital",
      type: "equity",
      debit: "0",
      credit: "120000.00",
      balance: "-120000.00",
    },
  ],
  total_debit: "120000.00",
  total_credit: "120000.00",
  residual: "0.00",
};

function renderPage() {
  return renderWithProviders(<TrialBalancePage />);
}

describe("TrialBalancePage", () => {
  beforeEach(() => {
    getTrialBalance.mockReset();
  });

  it("formats amounts, badges account types, and shows the balanced pill", async () => {
    getTrialBalance.mockResolvedValueOnce(balancedReport);
    renderPage();

    expect(await screen.findByText("Cash")).toBeInTheDocument();
    expect(screen.getByText("Share capital")).toBeInTheDocument();

    // Raw 120000.00 must never reach the user — it is grouped + 2dp.
    expect(screen.getAllByText("120,000.00").length).toBeGreaterThanOrEqual(3);
    expect(screen.getByText("-120,000.00")).toBeInTheDocument();

    // Lowercase enum tokens render as Title-Case semantic badges.
    expect(screen.getByText("Asset")).toBeInTheDocument();
    expect(screen.getByText("Equity")).toBeInTheDocument();

    // Residual is zero → the balanced indicator is shown.
    expect(screen.getByText("Balanced")).toBeInTheDocument();
    // Zeroed cells collapse to an em dash rather than "0.00".
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(2);
  });

  it("flags an out-of-balance report", async () => {
    getTrialBalance.mockResolvedValueOnce({
      ...balancedReport,
      total_credit: "119900.00",
      residual: "100.00",
    });
    renderPage();

    expect(await screen.findByText("Out of balance")).toBeInTheDocument();
    expect(screen.queryByText("Balanced")).toBeNull();
    expect(screen.getByText("100.00")).toBeInTheDocument();
  });

  it("renders an error surface with a retry action when the query fails", async () => {
    getTrialBalance.mockRejectedValueOnce(new Error("ledger offline"));
    renderPage();

    expect(
      await screen.findByText(/Couldn't load this report/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/ledger offline/i)).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });

  it("shows a teaching empty state when no balances are posted", async () => {
    getTrialBalance.mockResolvedValueOnce({
      ...balancedReport,
      rows: [],
      total_debit: "0",
      total_credit: "0",
      residual: "0",
    });
    renderPage();

    expect(
      await screen.findByText(/No posted balances as of/i),
    ).toBeInTheDocument();
  });
});
