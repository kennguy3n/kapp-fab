import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { DashboardSummary } from "@kapp/client";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";

/**
 * DashboardPage renders a KPI grid backed by /api/v1/dashboard/summary.
 * Each widget links to the deep list view of the underlying records
 * so an operator can drill in.
 */
export function DashboardPage() {
  const q = useQuery<DashboardSummary>({
    queryKey: ["dashboard", "summary"],
    queryFn: () => api.getDashboardSummary(),
  });
  // Locale-aware Intl formatter — picks up the active
  // LocaleContext tag so a pt-BR / es / fr-CA tenant sees
  // "R$ 1.234", "$ 1.234", "1 234 $" instead of the en-US
  // "$1,234" the dashboard hardcoded prior to PR-2d. Currency
  // placement, decimal separator, and digit grouping all follow
  // the active locale's CLDR rules; the currency code itself is
  // still the tenant's base currency reported by the API.
  const fmt = useFormatter();

  if (q.isLoading) return <p>Loading…</p>;
  if (q.isError) {
    return (
      <p style={{ color: "#b91c1c" }}>
        Failed to load dashboard: {(q.error as Error).message}
      </p>
    );
  }
  const s = q.data!;
  // Bind the formatter into a closure that mirrors the prior
  // formatAmount(value, currency?) signature so the JSX below
  // stays unchanged. When the API doesn't surface a currency
  // code (older payloads) we fall back to a plain locale-aware
  // number — Intl.NumberFormat without style:"currency" still
  // honours grouping and decimal conventions.
  const formatAmount = (value: number, currency?: string): string => {
    if (currency) {
      try {
        return fmt.currency(value, currency, { maximumFractionDigits: 0 });
      } catch {
        // fall through to bare-number formatting (synthetic ISO
        // codes the runtime rejects on construction)
      }
    }
    return fmt.number(value, { maximumFractionDigits: 0 });
  };

  return (
    <section>
      <h1>Dashboard</h1>
      <p style={{ color: "#6b7280" }}>
        At-a-glance KPIs. Each tile links to the underlying worklist.
      </p>

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(auto-fill, minmax(220px, 1fr))",
          gap: 12,
          marginTop: 16,
        }}
      >
        <Widget
          label="Open deals"
          value={s.open_deals_count}
          sub={`Pipeline ${formatAmount(s.pipeline_value, s.base_currency)}`}
          to="/records/crm.deal"
        />
        <Widget
          label="Outstanding AR"
          value={formatAmount(s.outstanding_ar, s.base_currency)}
          sub={`in ${s.base_currency}`}
          to="/records/finance.ar_invoice"
        />
        <Widget
          label="Outstanding AP"
          value={formatAmount(s.outstanding_ap, s.base_currency)}
          sub={`in ${s.base_currency}`}
          to="/records/finance.ap_bill"
        />
        <Widget
          label="Low-stock items"
          value={s.low_stock_items_count}
          to="/inventory/stock-levels"
        />
        <Widget
          label="Pending approvals"
          value={s.pending_approvals}
          to="/approvals"
        />
        <Widget
          label="Open tickets"
          value={s.open_tickets_count}
          sub={`${s.overdue_tickets_count} overdue`}
          to="/helpdesk"
        />
        <Widget
          label="Present today"
          value={s.present_today ?? 0}
          sub="hr.attendance — UTC day"
          to="/records/hr.attendance"
        />
        <Widget
          label="Pending reviews"
          value={s.pending_reviews ?? 0}
          sub="submitted + reviewed"
          to="/records/hr.appraisal"
        />
      </div>
    </section>
  );
}

function Widget({
  label,
  value,
  sub,
  to,
}: {
  label: string;
  value: string | number;
  sub?: string;
  to: string;
}) {
  return (
    <Link
      to={to}
      style={{
        display: "block",
        padding: 16,
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        textDecoration: "none",
        color: "inherit",
        background: "#fafafa",
      }}
    >
      <div style={{ fontSize: 12, color: "#6b7280", textTransform: "uppercase" }}>
        {label}
      </div>
      <div style={{ fontSize: 26, fontWeight: 600, marginTop: 4 }}>{value}</div>
      {sub && <div style={{ fontSize: 12, color: "#6b7280", marginTop: 4 }}>{sub}</div>}
    </Link>
  );
}

// formatAmount used to live here as a standalone helper hardcoded to
// "en-US" digit grouping. PR-2d (Americas tax pack rollout) lifted it
// into the DashboardPage component body so it can close over the
// useFormatter() hook from ../lib/i18n — the formatter now resolves
// against the active LocaleContext tag, so a pt-BR / es / fr-CA tenant
// sees CLDR-correct currency placement and digit grouping.
