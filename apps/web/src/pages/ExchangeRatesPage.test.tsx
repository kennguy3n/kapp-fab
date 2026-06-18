import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test-utils";

// Mock the api module *before* importing the page so the page's
// top-level `import { api } from "../lib/api"` resolves to the
// stub. The mock returns a thenable that resolves with a fixture
// list of exchange rates plus a mutation spy we can interrogate.
const listExchangeRates = vi.fn();
const upsertExchangeRate = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listExchangeRates: (...args: unknown[]) => listExchangeRates(...args),
    upsertExchangeRate: (...args: unknown[]) => upsertExchangeRate(...args),
  },
}));

import { ExchangeRatesPage } from "./ExchangeRatesPage";

function renderPage() {
  // renderWithProviders gives each render a fresh QueryClient (retries
  // disabled) plus the LocaleProvider/Router the page now needs for
  // its date/number formatting.
  return renderWithProviders(<ExchangeRatesPage />);
}

describe("ExchangeRatesPage", () => {
  beforeEach(() => {
    listExchangeRates.mockReset();
    upsertExchangeRate.mockReset();
  });

  it("renders the empty state when the tenant has no rates", async () => {
    listExchangeRates.mockResolvedValueOnce({ rates: [] });
    renderPage();
    expect(
      await screen.findByText(/No exchange rates yet/i),
    ).toBeInTheDocument();
    // Header + the per-tenant explainer paragraph must always render.
    expect(screen.getByRole("heading", { name: /Exchange Rates/i })).toBeInTheDocument();
  });

  it("renders the fetched rate rows with pair, date and provider", async () => {
    listExchangeRates.mockResolvedValueOnce({
      rates: [
        {
          tenant_id: "t1",
          from_currency: "USD",
          to_currency: "EUR",
          rate_date: "2025-01-15T00:00:00Z",
          rate: "0.91",
          provider: "ECB",
          created_at: "2025-01-15T00:00:00Z",
          updated_at: "2025-01-15T00:00:00Z",
        },
        {
          tenant_id: "t1",
          from_currency: "GBP",
          to_currency: "EUR",
          rate_date: "2025-01-14T00:00:00Z",
          rate: "1.17",
          provider: null,
          created_at: "2025-01-14T00:00:00Z",
          updated_at: "2025-01-14T00:00:00Z",
        },
      ],
    });
    renderPage();

    expect(await screen.findByText("USD", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(/GBP/)).toBeInTheDocument();
    // The rate column is locale-formatted (up to 6 fraction digits),
    // which is a no-op for these already-short decimals.
    expect(screen.getByText("0.91")).toBeInTheDocument();
    expect(screen.getByText("1.17")).toBeInTheDocument();
    // Provider null falls through to an em dash; ECB renders as a badge.
    expect(screen.getByText("ECB")).toBeInTheDocument();
    // Date column is locale-formatted (medium) rather than the raw ISO.
    expect(screen.getByText("Jan 15, 2025")).toBeInTheDocument();
  });

  it("shows an em dash instead of the literal NaN for a malformed rate", async () => {
    listExchangeRates.mockResolvedValueOnce({
      rates: [
        {
          tenant_id: "t1",
          from_currency: "USD",
          to_currency: "EUR",
          rate_date: "2025-01-15T00:00:00Z",
          rate: "not-a-number",
          provider: "manual",
          created_at: "2025-01-15T00:00:00Z",
          updated_at: "2025-01-15T00:00:00Z",
        },
      ],
    });
    renderPage();

    // The pair still renders, but the unparseable rate degrades to an
    // em dash rather than surfacing a raw "NaN" machine value.
    expect(await screen.findByText("USD", { exact: false })).toBeInTheDocument();
    expect(screen.queryByText("NaN")).toBeNull();
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(1);
  });

  it("upsertExchangeRate is invoked with uppercased currency codes", async () => {
    listExchangeRates.mockResolvedValueOnce({ rates: [] });
    upsertExchangeRate.mockResolvedValueOnce({
      tenant_id: "t1",
      from_currency: "USD",
      to_currency: "JPY",
      rate_date: "2025-03-01",
      rate: "149.20",
      provider: "manual",
      created_at: "2025-03-01T00:00:00Z",
      updated_at: "2025-03-01T00:00:00Z",
    });
    renderPage();
    await screen.findByText(/No exchange rates yet/i);

    // The form is pre-populated with USD → EUR / today / 1.0. We
    // overwrite a few fields with lowercase input to prove that the
    // submit handler normalises the codes to uppercase.
    const user = userEvent.setup();
    const fromInput = screen.getByPlaceholderText("USD") as HTMLInputElement;
    const toInput = screen.getByPlaceholderText("EUR") as HTMLInputElement;
    const rateInput = screen.getByPlaceholderText("0.91") as HTMLInputElement;
    const providerInput = screen.getByPlaceholderText(
      "manual",
    ) as HTMLInputElement;

    await user.clear(fromInput);
    await user.type(fromInput, "usd");
    await user.clear(toInput);
    await user.type(toInput, "jpy");
    await user.clear(rateInput);
    await user.type(rateInput, "149.20");
    await user.type(providerInput, "manual");

    await user.click(screen.getByRole("button", { name: /Save rate/i }));

    await waitFor(() => {
      expect(upsertExchangeRate).toHaveBeenCalledTimes(1);
    });
    const arg = upsertExchangeRate.mock.calls[0]![0] as Record<string, unknown>;
    expect(arg.from_currency).toBe("USD");
    expect(arg.to_currency).toBe("JPY");
    expect(arg.rate).toBe("149.20");
    expect(arg.provider).toBe("manual");
  });

  it("renders the load-error banner when the list query fails", async () => {
    listExchangeRates.mockRejectedValueOnce(new Error("network down"));
    renderPage();
    // The error surface shows a "Failed to load rates" heading plus the
    // underlying message and a retry affordance.
    expect(
      await screen.findByText(/Failed to load rates/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/network down/i)).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
    // The error and the empty-state placeholder are mutually
    // exclusive: when the list query fails we should not also tell
    // the user "no rates yet" (which would imply the API said zero
    // rather than that it crashed). Locks the fix that gates the
    // empty-state behind !q.isError.
    expect(screen.queryByText(/No exchange rates yet/i)).toBeNull();
  });
});
