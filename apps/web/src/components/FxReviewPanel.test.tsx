import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test-utils";
import { FxReviewPanel } from "./FxReviewPanel";
import type { RevaluationResult } from "./ConsolidationApi";

const listExchangeRates = vi.fn();
const upsertExchangeRate = vi.fn();
const convertCurrency = vi.fn();
const unrealizedGainLoss = vi.fn();
const runFxRevaluation = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listExchangeRates: (...a: unknown[]) => listExchangeRates(...a),
    upsertExchangeRate: (...a: unknown[]) => upsertExchangeRate(...a),
    convertCurrency: (...a: unknown[]) => convertCurrency(...a),
    unrealizedGainLoss: (...a: unknown[]) => unrealizedGainLoss(...a),
  },
}));

vi.mock("./ConsolidationApi", () => ({
  consolidationApi: {
    runFxRevaluation: (...a: unknown[]) => runFxRevaluation(...a),
  },
}));

const reval: RevaluationResult = {
  tenant_id: "t1",
  as_of: "2025-03-31T00:00:00Z",
  total_gain: "50",
  total_loss: "0",
  net: "50",
  lines: [
    {
      account_code: "1500",
      currency: "EUR",
      base_currency: "USD",
      foreign_net: "1000",
      current_rate: "1.10",
      recorded_base: "1050",
      revalued_base: "1100",
      delta: "50",
      gain_loss_account: "7200",
      entry_id: "e1",
    },
  ],
  skipped: [
    {
      account_code: "1200",
      currency: "JPY",
      base_currency: "USD",
      foreign_net: "300",
      reason: "no rate for JPY on 2025-03-31",
    },
  ],
};

describe("FxReviewPanel", () => {
  beforeEach(() => {
    listExchangeRates.mockReset();
    upsertExchangeRate.mockReset();
    convertCurrency.mockReset();
    unrealizedGainLoss.mockReset();
    runFxRevaluation.mockReset();
    listExchangeRates.mockResolvedValue({ rates: [] });
  });

  it("lists exchange rates", async () => {
    listExchangeRates.mockResolvedValue({
      rates: [
        {
          tenant_id: "t1",
          from_currency: "EUR",
          to_currency: "USD",
          rate_date: "2025-03-31T00:00:00Z",
          rate: "1.10",
          provider: "ECB",
          created_at: "x",
          updated_at: "x",
        },
      ],
    });
    renderWithProviders(<FxReviewPanel />);
    expect(await screen.findByText("ECB")).toBeInTheDocument();
    expect(screen.getByText("EUR → USD")).toBeInTheDocument();
  });

  it("translates an amount at the current rate", async () => {
    convertCurrency.mockResolvedValueOnce({
      amount: "1000",
      from: "EUR",
      to: "USD",
      date: "2025-03-31",
      rate: "1.10",
      converted: "1100",
    });
    renderWithProviders(<FxReviewPanel />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /^Translate$/i }));
    const result = await screen.findByTestId("fx-convert-result");
    expect(within(result).getByText(/1100 USD/)).toBeInTheDocument();
  });

  it("computes unrealized gain/loss for review", async () => {
    unrealizedGainLoss.mockResolvedValueOnce({ unrealized_gain_loss: "42" });
    renderWithProviders(<FxReviewPanel />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /Compute delta/i }));
    const result = await screen.findByTestId("fx-unrealized-result");
    expect(within(result).getByText("42")).toBeInTheDocument();
  });

  it("runs a revaluation and shows per-account lines, totals and skips", async () => {
    runFxRevaluation.mockResolvedValueOnce(reval);
    renderWithProviders(<FxReviewPanel />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText(/Tenant ID/i), "t1");
    await user.click(screen.getByRole("button", { name: /^Run revaluation$/i }));

    const result = await screen.findByTestId("fx-reval-result");
    expect(within(result).getByText("1100")).toBeInTheDocument(); // revalued base
    expect(within(result).getByText("7200")).toBeInTheDocument(); // gl account
    // Skipped balance surfaced with reason.
    expect(
      within(result).getByText(/no rate for JPY/i),
    ).toBeInTheDocument();
    expect(runFxRevaluation).toHaveBeenCalledWith(
      expect.objectContaining({ tenant_id: "t1" }),
    );
  });
});
