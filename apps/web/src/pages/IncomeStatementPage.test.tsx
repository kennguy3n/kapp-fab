import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { IncomeStatement } from "@kapp/client";
import { renderWithProviders } from "../test-utils";

const getIncomeStatement = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    getIncomeStatement: (...args: unknown[]) => getIncomeStatement(...args),
  },
}));

import { IncomeStatementPage } from "./IncomeStatementPage";

const report: IncomeStatement = {
  from: "2025-01-01",
  to: "2025-06-30",
  revenue: [
    { account_code: "4000", account_name: "Sales", amount: "200000.00" },
  ],
  expense: [{ account_code: "5000", account_name: "Rent", amount: "50000.00" }],
  total_revenue: "200000.00",
  total_expense: "50000.00",
  net_income: "150000.00",
};

function renderPage() {
  return renderWithProviders(<IncomeStatementPage />);
}

describe("IncomeStatementPage", () => {
  beforeEach(() => {
    getIncomeStatement.mockReset();
  });

  it("renders revenue, expenses and net income with formatted totals", async () => {
    getIncomeStatement.mockResolvedValue(report);
    renderPage();

    expect(await screen.findByText("Sales")).toBeInTheDocument();
    expect(screen.getByText("Rent")).toBeInTheDocument();

    expect(screen.getAllByText("Total revenue").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Total expenses").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Net income").length).toBeGreaterThanOrEqual(1);

    expect(screen.getAllByText("200,000.00").length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText("150,000.00").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("Profit for the period")).toBeInTheDocument();
  });

  it("labels a negative bottom line as a loss", async () => {
    getIncomeStatement.mockResolvedValue({
      ...report,
      revenue: [
        { account_code: "4000", account_name: "Sales", amount: "50000.00" },
      ],
      expense: [
        { account_code: "5000", account_name: "Rent", amount: "200000.00" },
      ],
      total_revenue: "50000.00",
      total_expense: "200000.00",
      net_income: "-150000.00",
    });
    renderPage();

    expect(await screen.findByText("Loss for the period")).toBeInTheDocument();
    expect(screen.getAllByText("-150,000.00").length).toBeGreaterThanOrEqual(1);
  });

  it("reveals comparison columns when comparing to the previous period", async () => {
    getIncomeStatement.mockResolvedValue(report);
    renderPage();
    await screen.findByText("Sales");

    const user = userEvent.setup();
    await user.click(screen.getByRole("checkbox"));

    expect(
      (await screen.findAllByText("Previous")).length,
    ).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Change").length).toBeGreaterThanOrEqual(1);
  });

  it("renders an error surface with retry when the query fails", async () => {
    getIncomeStatement.mockRejectedValue(new Error("statement offline"));
    renderPage();

    expect(
      await screen.findByText(/Couldn't load the income statement/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });
});
