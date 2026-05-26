import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

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
  // A fresh QueryClient per test so cached fixtures from one
  // assertion don't bleed into the next.
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ExchangeRatesPage />
    </QueryClientProvider>,
  );
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
    // The rate column is rendered as the raw decimal string.
    expect(screen.getByText("0.91")).toBeInTheDocument();
    expect(screen.getByText("1.17")).toBeInTheDocument();
    // Provider null falls through to an empty cell; ECB renders.
    expect(screen.getByText("ECB")).toBeInTheDocument();
    // Date column slices the leading 10 chars off the timestamp.
    expect(screen.getByText("2025-01-15")).toBeInTheDocument();
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
    const fromInput = screen.getByPlaceholderText("from") as HTMLInputElement;
    const toInput = screen.getByPlaceholderText("to") as HTMLInputElement;
    const rateInput = screen.getByPlaceholderText("rate") as HTMLInputElement;
    const providerInput = screen.getByPlaceholderText(
      /provider \(optional\)/i,
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
    expect(
      await screen.findByText(/Failed to load rates: network down/i),
    ).toBeInTheDocument();
    // The error and the empty-state placeholder are mutually
    // exclusive: when the list query fails we should not also tell
    // the user "no rates yet" (which would imply the API said zero
    // rather than that it crashed). Locks the fix that gates the
    // empty-state behind !q.isError.
    expect(screen.queryByText(/No exchange rates yet/i)).toBeNull();
  });
});
