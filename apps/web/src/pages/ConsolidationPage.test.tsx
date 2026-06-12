import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test-utils";
import type {
  ConsolidatedTrialBalance,
  ConsolidationGroup,
} from "../components/ConsolidationApi";

const createGroup = vi.fn();
const runConsolidation = vi.fn();
const runStatements = vi.fn();
const runFxRevaluation = vi.fn();

vi.mock("../components/ConsolidationApi", () => ({
  consolidationApi: {
    createGroup: (...a: unknown[]) => createGroup(...a),
    runConsolidation: (...a: unknown[]) => runConsolidation(...a),
    runStatements: (...a: unknown[]) => runStatements(...a),
    runFxRevaluation: (...a: unknown[]) => runFxRevaluation(...a),
  },
}));

vi.mock("../lib/api", () => ({
  api: {
    listExchangeRates: vi.fn().mockResolvedValue({ rates: [] }),
    upsertExchangeRate: vi.fn(),
    convertCurrency: vi.fn(),
    unrealizedGainLoss: vi.fn(),
  },
}));

import { ConsolidationPage } from "./ConsolidationPage";

const STORAGE_KEY = "kapp.consolidation.groups";

const group: ConsolidationGroup = {
  id: "grp-1",
  name: "Global",
  presentation_currency: "USD",
  member_tenant_ids: ["t1", "t2"],
};

const tb: ConsolidatedTrialBalance = {
  group_id: "grp-1",
  as_of: "2025-03-31T00:00:00Z",
  presentation_currency: "USD",
  total_debit: "1000",
  total_credit: "1000",
  residual: "0",
  cta: "0",
  rows: [
    {
      account_code: "1000",
      account_name: "Cash",
      type: "asset",
      debit: "1000",
      credit: "0",
      balance: "1000",
      contributions: [{ tenant_id: "t1", debit: "1000", credit: "0" }],
    },
  ],
  eliminated: [],
};

function seedGroups(groups: ConsolidationGroup[]) {
  window.localStorage.setItem(STORAGE_KEY, JSON.stringify(groups));
}

describe("ConsolidationPage", () => {
  beforeEach(() => {
    createGroup.mockReset();
    runConsolidation.mockReset();
    runStatements.mockReset();
    window.localStorage.removeItem(STORAGE_KEY);
  });

  it("renders the console header and the four tabs", () => {
    renderWithProviders(<ConsolidationPage />);
    expect(
      screen.getByRole("heading", { name: /Consolidation & FX Review/i }),
    ).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: /Groups & Run/i })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: /Trial Balance/i })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: /Statements/i })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: /FX Review/i })).toBeInTheDocument();
  });

  it("surfaces the active group when one is tracked", () => {
    seedGroups([group]);
    renderWithProviders(<ConsolidationPage />);
    expect(screen.getByText(/Active group/i)).toBeInTheDocument();
    // "Global" appears both in the header badge and the groups list.
    expect(screen.getAllByText("Global").length).toBeGreaterThan(0);
  });

  it("runs a consolidation and renders the trial balance", async () => {
    seedGroups([group]);
    runConsolidation.mockResolvedValueOnce(tb);
    renderWithProviders(<ConsolidationPage />);
    const user = userEvent.setup();

    await user.click(screen.getByRole("tab", { name: /Trial Balance/i }));
    await user.click(screen.getByRole("button", { name: /^Run consolidation$/i }));

    await waitFor(() => expect(runConsolidation).toHaveBeenCalledWith("grp-1", undefined));
    expect(await screen.findByText("Balanced")).toBeInTheDocument();
    expect(screen.getByText(/Consolidated trial balance/i)).toBeInTheDocument();
  });

  it("disables the run button when no group is selected", async () => {
    renderWithProviders(<ConsolidationPage />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("tab", { name: /Trial Balance/i }));
    expect(
      screen.getByRole("button", { name: /^Run consolidation$/i }),
    ).toBeDisabled();
    expect(screen.getByText(/Select or create a group first/i)).toBeInTheDocument();
  });

  it("creates a group from the groups tab and marks it active", async () => {
    createGroup.mockResolvedValueOnce(group);
    renderWithProviders(<ConsolidationPage />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText(/^Name$/i), "Global");
    await user.click(screen.getByRole("button", { name: /^Create group$/i }));

    await waitFor(() => expect(createGroup).toHaveBeenCalledTimes(1));
    // The created group becomes the active group badge in the header.
    expect(await screen.findByText(/Active group/i)).toBeInTheDocument();
  });
});
