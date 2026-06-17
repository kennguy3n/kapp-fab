import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import type { FinanceAccount, JournalEntry } from "@kapp/client";
import { renderWithProviders } from "../test-utils";

const listJournalEntries = vi.fn();
const listAccounts = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listJournalEntries: (...args: unknown[]) => listJournalEntries(...args),
    listAccounts: (...args: unknown[]) => listAccounts(...args),
  },
}));

import { JournalEntriesPage } from "./JournalEntriesPage";

const accounts: FinanceAccount[] = [
  { tenant_id: "t1", code: "1000", name: "Cash", type: "asset", active: true },
  {
    tenant_id: "t1",
    code: "3000",
    name: "Share capital",
    type: "equity",
    active: true,
  },
];

function makeEntry(overrides: Partial<JournalEntry> = {}): JournalEntry {
  return {
    id: "e1",
    tenant_id: "t1",
    posted_at: "2025-01-15T00:00:00Z",
    memo: "Owner investment",
    source_ktype: "manual",
    created_by: "u1",
    created_at: "2025-01-15T00:00:00Z",
    lines: [
      {
        id: 1,
        tenant_id: "t1",
        entry_id: "e1",
        account_code: "1000",
        debit: "120000.00",
        credit: "0",
        currency: "USD",
        memo: "",
      },
      {
        id: 2,
        tenant_id: "t1",
        entry_id: "e1",
        account_code: "3000",
        debit: "0",
        credit: "120000.00",
        currency: "USD",
        memo: "",
      },
    ],
    ...overrides,
  };
}

function renderPage() {
  return renderWithProviders(<JournalEntriesPage />);
}

describe("JournalEntriesPage", () => {
  beforeEach(() => {
    listJournalEntries.mockReset();
    listAccounts.mockReset();
    listAccounts.mockResolvedValue(accounts);
  });

  it("renders a balanced entry with formatted currency and resolved account names", async () => {
    listJournalEntries.mockResolvedValueOnce([makeEntry()]);
    renderPage();

    expect(await screen.findByText("Owner investment")).toBeInTheDocument();
    expect(screen.getByText("Manual entry")).toBeInTheDocument();

    // Codes resolve to names; amounts are currency-formatted.
    expect(screen.getByText("Cash")).toBeInTheDocument();
    expect(screen.getByText("Share capital")).toBeInTheDocument();
    expect(screen.getAllByText("$120,000.00").length).toBeGreaterThanOrEqual(2);

    expect(screen.getByText("Balanced")).toBeInTheDocument();
  });

  it("flags an entry whose debits and credits don't tie out", async () => {
    listJournalEntries.mockResolvedValueOnce([
      makeEntry({
        lines: [
          {
            id: 1,
            tenant_id: "t1",
            entry_id: "e1",
            account_code: "1000",
            debit: "120000.00",
            credit: "0",
            currency: "USD",
            memo: "",
          },
          {
            id: 2,
            tenant_id: "t1",
            entry_id: "e1",
            account_code: "3000",
            debit: "0",
            credit: "100000.00",
            currency: "USD",
            memo: "",
          },
        ],
      }),
    ]);
    renderPage();

    expect(await screen.findByText("Out of balance")).toBeInTheDocument();
    expect(
      screen.getByText(/Debits and credits don't match/i),
    ).toBeInTheDocument();
  });

  it("renders an error surface with retry when the list query fails", async () => {
    listJournalEntries.mockRejectedValueOnce(new Error("journal offline"));
    renderPage();

    expect(
      await screen.findByText(/Couldn't load journal entries/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });

  it("teaches the empty state when nothing is posted", async () => {
    listJournalEntries.mockResolvedValueOnce([]);
    renderPage();

    expect(
      await screen.findByText(/No journal entries have been posted yet/i),
    ).toBeInTheDocument();
  });
});
