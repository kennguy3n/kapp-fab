import { useEffect, useState } from "react";

// BillingPage is the Workstream 1 tenant billing dashboard. It reads
// GET /api/v1/billing/usage (current plan, metered usage rolled up
// against the plan limits, the Stripe subscription, and invoice
// history) and drives the two write paths:
//
//   - Upgrade / downgrade: POST /api/v1/billing/subscribe. For a paid
//     plan the response carries a Stripe Checkout URL we redirect to;
//     the plan only actually switches once Stripe confirms payment via
//     webhook. The free plan switches immediately.
//   - Manage payment method: POST /api/v1/billing/portal-session opens
//     the Stripe Billing Portal.
//
// Every request carries the tenant + bearer headers from localStorage
// (the same convention SetupWizardPage uses) so it runs under the
// caller's tenant context — a caller can only ever see or change their
// own tenant's billing.

interface PlanLimits {
  api_calls: number;
  storage_bytes: number;
  krecord_count: number;
  user_seats: number;
}

interface Subscription {
  plan: string;
  status: string;
  cancel_at_period_end: boolean;
  current_period_end?: string;
  trial_end?: string;
}

interface Invoice {
  stripe_invoice_id: string;
  status: string;
  amount_due: number;
  amount_paid: number;
  currency: string;
  hosted_invoice_url?: string;
  period_start?: string;
  period_end?: string;
  created_at: string;
}

interface BillingUsage {
  plan: string;
  usage: Record<string, number>;
  limits: PlanLimits;
  subscription: Subscription | null;
  invoices: Invoice[];
}

interface PlanOption {
  name: string;
  display_name: string;
}

interface SubscribeResult {
  plan: string;
  requires_payment: boolean;
  checkout_url?: string;
}

function authHeaders(): Record<string, string> {
  const h: Record<string, string> = {
    "Content-Type": "application/json",
    "X-Tenant-ID": localStorage.getItem("kapp.tenant") ?? "default",
  };
  const t = localStorage.getItem("kapp.token");
  if (t) h.Authorization = `Bearer ${t}`;
  return h;
}

// The canonical metered counters, in display order, paired with a
// human label and the matching limit key. Keeping this list explicit
// (rather than iterating the usage map) gives a stable row order and
// lets us render a meter even when a counter has not been written yet.
const METRICS: { key: string; label: string; limit: keyof PlanLimits }[] = [
  { key: "api_calls", label: "API calls", limit: "api_calls" },
  { key: "storage_bytes", label: "Storage (bytes)", limit: "storage_bytes" },
  { key: "krecord_count", label: "Records", limit: "krecord_count" },
  { key: "user_seats", label: "User seats", limit: "user_seats" },
];

// Stripe zero-decimal currencies: the API expects/returns amounts in
// the major unit (no cents), so they must NOT be divided by 100.
// https://stripe.com/docs/currencies#zero-decimal
const ZERO_DECIMAL_CURRENCIES = new Set([
  "bif", "clp", "djf", "gnf", "jpy", "kmf", "krw", "mga", "pyg",
  "rwf", "ugx", "vnd", "vuv", "xaf", "xof", "xpf",
]);

function formatMoney(minor: number, currency: string): string {
  // Stripe amounts are in the currency's smallest unit — 1/100th of the
  // major unit for most currencies (cents), but already the major unit
  // for zero-decimal currencies (JPY, KRW, VND, …), which must not be
  // divided. An unknown/empty currency falls back to the raw value.
  const isZeroDecimal =
    !!currency && ZERO_DECIMAL_CURRENCIES.has(currency.toLowerCase());
  const major = isZeroDecimal ? minor : minor / 100;
  if (!currency) return major.toFixed(2);
  try {
    return new Intl.NumberFormat(undefined, {
      style: "currency",
      currency: currency.toUpperCase(),
    }).format(major);
  } catch {
    return `${major.toFixed(isZeroDecimal ? 0 : 2)} ${currency.toUpperCase()}`;
  }
}

export function BillingPage() {
  const [data, setData] = useState<BillingUsage | null>(null);
  const [plans, setPlans] = useState<PlanOption[]>([]);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const r = await fetch("/api/v1/billing/usage", {
          headers: authHeaders(),
        });
        if (!r.ok) throw new Error(`Failed to load billing (${r.status})`);
        const body = (await r.json()) as BillingUsage;
        if (!cancelled) setData(body);
      } catch (e) {
        if (!cancelled) {
          setLoadErr(e instanceof Error ? e.message : String(e));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [reloadKey]);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const r = await fetch("/api/v1/plans", { headers: authHeaders() });
        if (!r.ok) return;
        const body = (await r.json()) as { plans?: PlanOption[] };
        if (!cancelled && Array.isArray(body.plans)) setPlans(body.plans);
      } catch {
        // Plan list is optional — without it the change-plan control
        // is hidden but the dashboard still renders.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const changePlan = async (plan: string) => {
    setBusy(true);
    setActionErr(null);
    try {
      const r = await fetch("/api/v1/billing/subscribe", {
        method: "POST",
        headers: authHeaders(),
        body: JSON.stringify({
          plan,
          success_url: window.location.origin + "/billing",
          cancel_url: window.location.origin + "/billing",
        }),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(text || `Plan change failed (${r.status})`);
      }
      const body = (await r.json()) as SubscribeResult;
      if (body.requires_payment && body.checkout_url) {
        // Hand off to Stripe Checkout; the plan switches on webhook.
        window.location.href = body.checkout_url;
        return;
      }
      // Free plan switched immediately — refetch the dashboard.
      setReloadKey((k) => k + 1);
    } catch (e) {
      setActionErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const openPortal = async () => {
    setBusy(true);
    setActionErr(null);
    try {
      const r = await fetch("/api/v1/billing/portal-session", {
        method: "POST",
        headers: authHeaders(),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(text || `Could not open billing portal (${r.status})`);
      }
      const body = (await r.json()) as { url: string };
      if (body.url) window.location.href = body.url;
    } catch (e) {
      setActionErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  if (loadErr) {
    return (
      <div>
        <h1>Billing</h1>
        <p style={{ color: "red" }}>{loadErr}</p>
      </div>
    );
  }

  if (!data) {
    return (
      <div>
        <h1>Billing</h1>
        <p>Loading…</p>
      </div>
    );
  }

  const sub = data.subscription;

  return (
    <div style={{ maxWidth: 720 }}>
      <h1>Billing</h1>

      <section>
        <h2>Current plan</h2>
        <p>
          <strong>{data.plan}</strong>
          {sub && sub.status ? ` — ${sub.status}` : ""}
          {sub && sub.cancel_at_period_end ? " (cancels at period end)" : ""}
        </p>
        {sub?.trial_end && (
          <p>Trial ends {sub.trial_end.slice(0, 10)}</p>
        )}
        {sub?.current_period_end && (
          <p>Current period ends {sub.current_period_end.slice(0, 10)}</p>
        )}
      </section>

      <section>
        <h2>Usage</h2>
        <table>
          <thead>
            <tr>
              <th>Metric</th>
              <th>Used</th>
              <th>Limit</th>
            </tr>
          </thead>
          <tbody>
            {METRICS.map((m) => {
              const used = data.usage[m.key] ?? 0;
              const limit = data.limits[m.limit] ?? 0;
              return (
                <tr key={m.key}>
                  <td>{m.label}</td>
                  <td>{used}</td>
                  {/* A zero limit means "unlimited" at the API layer. */}
                  <td>{limit === 0 ? "Unlimited" : limit}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </section>

      {plans.length > 0 && (
        <section>
          <h2>Change plan</h2>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            {plans.map((p) => (
              <button
                key={p.name}
                type="button"
                disabled={busy || p.name === data.plan}
                onClick={() => void changePlan(p.name)}
              >
                {p.name === data.plan
                  ? `${p.display_name} (current)`
                  : `Switch to ${p.display_name}`}
              </button>
            ))}
          </div>
        </section>
      )}

      <section>
        <h2>Payment method</h2>
        <button type="button" onClick={() => void openPortal()} disabled={busy}>
          Manage payment method
        </button>
      </section>

      <section>
        <h2>Invoices</h2>
        {data.invoices.length === 0 ? (
          <p>No invoices yet.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Date</th>
                <th>Status</th>
                <th>Amount</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.invoices.map((inv) => (
                <tr key={inv.stripe_invoice_id}>
                  <td>{inv.created_at.slice(0, 10)}</td>
                  <td>{inv.status}</td>
                  <td>{formatMoney(inv.amount_due, inv.currency)}</td>
                  <td>
                    {inv.hosted_invoice_url && (
                      <a
                        href={inv.hosted_invoice_url}
                        target="_blank"
                        rel="noreferrer"
                      >
                        View
                      </a>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {actionErr && <p style={{ color: "red" }}>{actionErr}</p>}
    </div>
  );
}
