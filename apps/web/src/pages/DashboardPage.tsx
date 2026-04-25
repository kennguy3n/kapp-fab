import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { DashboardSummary } from "@kapp/client";
import { api } from "../lib/api";

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

  if (q.isLoading) return <p>Loading…</p>;
  if (q.isError) {
    return (
      <p style={{ color: "#b91c1c" }}>
        Failed to load dashboard: {(q.error as Error).message}
      </p>
    );
  }
  const s = q.data!;

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

// formatAmount renders a monetary total in the tenant's base currency.
// The server folds foreign-currency krecords through ExchangeRateStore
// before responding, so the dashboard now gets a single converted total
// per widget. Falls back to a bare number when the currency code is
// missing or unknown to Intl (older browsers / synthetic ISO codes).
function formatAmount(v: number, currency?: string): string {
  if (currency) {
    try {
      return new Intl.NumberFormat("en-US", {
        style: "currency",
        currency,
        maximumFractionDigits: 0,
      }).format(v);
    } catch {
      // fall through to bare-number formatting
    }
  }
  return new Intl.NumberFormat("en-US", {
    maximumFractionDigits: 0,
  }).format(v);
}
