import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FinanceAccount } from "@kapp/client";
import { renderWithProviders } from "../test-utils";

const listAccounts = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listAccounts: (...args: unknown[]) => listAccounts(...args),
  },
}));

import { ChartOfAccountsPage } from "./ChartOfAccountsPage";

const accounts: FinanceAccount[] = [
  { tenant_id: "t1", code: "1000", name: "Cash", type: "asset", active: true },
  {
    tenant_id: "t1",
    code: "1100",
    name: "Accounts receivable",
    type: "asset",
    parent_code: "1000",
    active: true,
  },
  { tenant_id: "t1", code: "4000", name: "Sales", type: "revenue", active: false },
];

function renderPage() {
  return renderWithProviders(<ChartOfAccountsPage />);
}

describe("ChartOfAccountsPage", () => {
  beforeEach(() => {
    listAccounts.mockReset();
  });

  it("groups accounts by type with semantic badges and a structure summary", async () => {
    listAccounts.mockResolvedValueOnce(accounts);
    renderPage();

    expect(await screen.findByText("Cash")).toBeInTheDocument();
    expect(screen.getByText("Accounts receivable")).toBeInTheDocument();
    expect(screen.getByText("Sales")).toBeInTheDocument();

    // Group headers render type tokens as Title-Case badges.
    expect(screen.getByText("Asset")).toBeInTheDocument();
    expect(screen.getByText("Revenue")).toBeInTheDocument();

    // Structure summary: 3 accounts, 2 active, no orphans.
    expect(screen.getByText("3 accounts")).toBeInTheDocument();
    expect(screen.getByText("2 active")).toBeInTheDocument();
    expect(screen.getByText("Structure complete")).toBeInTheDocument();

    // Active/inactive render as status badges.
    expect(screen.getAllByText("Active").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("Inactive")).toBeInTheDocument();
  });

  it("flags orphaned accounts whose parent is missing", async () => {
    listAccounts.mockResolvedValueOnce([
      {
        tenant_id: "t1",
        code: "5000",
        name: "Marketing",
        type: "expense",
        parent_code: "4999",
        active: true,
      },
    ]);
    renderPage();

    expect(await screen.findByText("1 orphaned account")).toBeInTheDocument();
  });

  it("filters the tree by the search term", async () => {
    listAccounts.mockResolvedValueOnce(accounts);
    renderPage();
    await screen.findByText("Cash");

    const user = userEvent.setup();
    await user.type(
      screen.getByPlaceholderText("Search by code or name…"),
      "sales",
    );

    expect(screen.getByText("Sales")).toBeInTheDocument();
    expect(screen.queryByText("Cash")).toBeNull();
  });

  it("renders an error surface with retry when the query fails", async () => {
    listAccounts.mockRejectedValueOnce(new Error("accounts down"));
    renderPage();

    expect(
      await screen.findByText(/Couldn't load the chart of accounts/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });

  it("teaches the empty state when there are no accounts", async () => {
    listAccounts.mockResolvedValueOnce([]);
    renderPage();

    expect(await screen.findByText(/No accounts yet/i)).toBeInTheDocument();
  });
});
