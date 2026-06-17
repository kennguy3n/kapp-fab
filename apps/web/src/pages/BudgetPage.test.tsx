import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Budget, BudgetVarianceReport } from "@kapp/client";
import { renderWithProviders } from "../test-utils";

const listBudgets = vi.fn();
const listAccounts = vi.fn();
const listRecords = vi.fn();
const listBudgetLines = vi.fn();
const budgetVariance = vi.fn();
const createBudget = vi.fn();
const upsertBudgetLine = vi.fn();
const deleteBudgetLine = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listBudgets: (...args: unknown[]) => listBudgets(...args),
    listAccounts: (...args: unknown[]) => listAccounts(...args),
    listRecords: (...args: unknown[]) => listRecords(...args),
    listBudgetLines: (...args: unknown[]) => listBudgetLines(...args),
    budgetVariance: (...args: unknown[]) => budgetVariance(...args),
    createBudget: (...args: unknown[]) => createBudget(...args),
    upsertBudgetLine: (...args: unknown[]) => upsertBudgetLine(...args),
    deleteBudgetLine: (...args: unknown[]) => deleteBudgetLine(...args),
  },
}));

import { BudgetPage } from "./BudgetPage";

function makeBudget(overrides: Partial<Budget> = {}): Budget {
  return {
    tenant_id: "tenant-1",
    id: "budget-1",
    name: "Marketing",
    fiscal_year: 2026,
    status: "draft",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

const emptyVariance: BudgetVarianceReport = {
  tenant_id: "tenant-1",
  budget_id: "budget-1",
  budget_name: "Marketing",
  fiscal_year: 2026,
  from: "2026-01-01",
  to: "2026-12-31",
  rows: [],
  total_budgeted: "0",
  total_actual: "0",
  total_variance: "0",
  total_favourable_variance: "0",
  total_unfavourable_variance: "0",
};

describe("BudgetPage", () => {
  beforeEach(() => {
    listBudgets.mockReset();
    listAccounts.mockReset();
    listRecords.mockReset();
    listBudgetLines.mockReset();
    budgetVariance.mockReset();
    createBudget.mockReset();
    upsertBudgetLine.mockReset();
    deleteBudgetLine.mockReset();
    listAccounts.mockResolvedValue([]);
    listRecords.mockResolvedValue([]);
    listBudgetLines.mockResolvedValue([]);
    budgetVariance.mockResolvedValue(emptyVariance);
    createBudget.mockResolvedValue(makeBudget());
  });

  it("lists budgets with status badges and a selection hint", async () => {
    listBudgets.mockResolvedValue([
      makeBudget({ id: "b1", name: "Marketing", status: "draft" }),
      makeBudget({ id: "b2", name: "Operations", status: "active" }),
    ]);
    renderWithProviders(<BudgetPage />);

    expect(await screen.findByText("Marketing")).toBeInTheDocument();
    expect(screen.getByText("Operations")).toBeInTheDocument();
    expect(screen.getByText("Draft")).toBeInTheDocument();
    expect(screen.getByText("Active")).toBeInTheDocument();
    expect(screen.getByText("Your budgets")).toBeInTheDocument();
    expect(
      screen.getByText(/Select a budget to edit its monthly lines/i),
    ).toBeInTheDocument();
  });

  it("shows a teaching empty state when there are no budgets", async () => {
    listBudgets.mockResolvedValue([]);
    renderWithProviders(<BudgetPage />);

    expect(
      await screen.findByText(/No budgets yet\. Create one to start planning\./i),
    ).toBeInTheDocument();
  });

  it("renders an error surface with retry when budgets fail to load", async () => {
    listBudgets.mockRejectedValue(new Error("budgets down"));
    renderWithProviders(<BudgetPage />);

    expect(
      await screen.findByText(/Couldn't load budgets/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });

  it("creates a budget from the inline form", async () => {
    listBudgets.mockResolvedValue([]);
    renderWithProviders(<BudgetPage />);
    await screen.findByText(/No budgets yet/i);

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /new budget/i }));
    await user.type(
      screen.getByPlaceholderText("Marketing FY26"),
      "Q1 marketing",
    );
    await user.click(screen.getByRole("button", { name: /create budget/i }));

    await waitFor(() =>
      expect(createBudget).toHaveBeenCalledWith(
        expect.objectContaining({ name: "Q1 marketing", status: "draft" }),
      ),
    );
  });
});
