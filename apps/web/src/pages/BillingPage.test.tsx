import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BillingPage } from "./BillingPage";

const USAGE = {
  plan: "starter",
  usage: { api_calls: 120, storage_bytes: 2048, krecord_count: 9, user_seats: 3 },
  limits: {
    api_calls: 1000,
    storage_bytes: 0,
    krecord_count: 100,
    user_seats: 5,
  },
  subscription: {
    plan: "starter",
    status: "active",
    cancel_at_period_end: false,
    current_period_end: "2025-12-31T00:00:00Z",
  },
  invoices: [
    {
      stripe_invoice_id: "in_1",
      status: "paid",
      amount_due: 4900,
      amount_paid: 4900,
      currency: "usd",
      hosted_invoice_url: "https://stripe.example/in_1",
      created_at: "2025-01-15T00:00:00Z",
    },
  ],
};

const PLANS = [
  { name: "free", display_name: "Free" },
  { name: "starter", display_name: "Starter" },
  { name: "business", display_name: "Business" },
];

// Route fetch by URL + method so each test can override a single
// endpoint's behaviour while the others return their defaults.
function stubFetch(overrides: {
  usage?: { ok: boolean; status: number; body: unknown };
  subscribe?: { ok: boolean; status: number; body: unknown };
  portal?: { ok: boolean; status: number; body: unknown };
} = {}) {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/v1/billing/usage") {
      const o = overrides.usage ?? { ok: true, status: 200, body: USAGE };
      return {
        ok: o.ok,
        status: o.status,
        json: async () => o.body,
        text: async () => JSON.stringify(o.body),
      } as Response;
    }
    if (url === "/api/v1/plans") {
      return {
        ok: true,
        status: 200,
        json: async () => ({ plans: PLANS }),
      } as Response;
    }
    if (url === "/api/v1/billing/subscribe" && init?.method === "POST") {
      const o =
        overrides.subscribe ??
        { ok: true, status: 200, body: { plan: "free", requires_payment: false } };
      return {
        ok: o.ok,
        status: o.status,
        json: async () => o.body,
        text: async () => JSON.stringify(o.body),
      } as Response;
    }
    if (url === "/api/v1/billing/portal-session" && init?.method === "POST") {
      const o =
        overrides.portal ??
        { ok: true, status: 200, body: { url: "https://stripe.example/portal" } };
      return {
        ok: o.ok,
        status: o.status,
        json: async () => o.body,
        text: async () => JSON.stringify(o.body),
      } as Response;
    }
    throw new Error(`unexpected fetch ${url}`);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

describe("BillingPage", () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("renders the current plan, usage meters and invoice history", async () => {
    stubFetch();
    render(<BillingPage />);

    // Current plan + status. The plan name and status render as
    // separate text nodes inside the <p>, so assert against the
    // paragraph's combined textContent.
    const planValue = await screen.findByText("starter");
    expect(planValue.closest("p")?.textContent).toContain("starter — active");
    // Usage rows.
    expect(screen.getByText("API calls")).toBeInTheDocument();
    expect(screen.getByText("120")).toBeInTheDocument();
    // A zero limit renders as "Unlimited" (storage_bytes limit is 0).
    expect(screen.getByText("Unlimited")).toBeInTheDocument();
    // Invoice currency formatting (4900 minor units = $49.00).
    expect(screen.getByText(/\$49\.00/)).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /View/i }),
    ).toHaveAttribute("href", "https://stripe.example/in_1");
  });

  it("posting a free-plan switch refetches without redirecting", async () => {
    const fetchMock = stubFetch();
    render(<BillingPage />);
    await screen.findByText("starter");

    const user = userEvent.setup();
    await user.click(
      screen.getByRole("button", { name: /Switch to Free/i }),
    );

    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(
        (c) => c[0] === "/api/v1/billing/subscribe",
      );
      expect(calls.length).toBe(1);
    });
    const init = fetchMock.mock.calls.find(
      (c) => c[0] === "/api/v1/billing/subscribe",
    )![1] as RequestInit;
    expect(JSON.parse(init.body as string).plan).toBe("free");
  });

  it("formats zero-decimal currency invoices without dividing by 100", async () => {
    // JPY is a Stripe zero-decimal currency: a 4900 minor-unit amount
    // IS ¥4,900, not ¥49. Guards against the cents-everywhere bug.
    stubFetch({
      usage: {
        ok: true,
        status: 200,
        body: {
          ...USAGE,
          invoices: [
            {
              stripe_invoice_id: "in_jpy",
              status: "paid",
              amount_due: 4900,
              amount_paid: 4900,
              currency: "jpy",
              created_at: "2025-02-01T00:00:00Z",
            },
          ],
        },
      },
    });
    render(<BillingPage />);
    await screen.findByText("starter");
    expect(screen.getByText(/4,900/)).toBeInTheDocument();
    expect(screen.queryByText(/49\.00/)).not.toBeInTheDocument();
  });

  it("renders the load-error banner when usage fails", async () => {
    stubFetch({ usage: { ok: false, status: 500, body: {} } });
    render(<BillingPage />);
    expect(
      await screen.findByText(/Failed to load billing/i),
    ).toBeInTheDocument();
  });
});
