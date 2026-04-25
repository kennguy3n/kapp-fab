import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import type { PlanLimits } from "@kapp/client";

const tenantKey = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

const METRIC_ORDER: Array<{ key: string; label: string; format: (n: number) => string }> = [
  { key: "api_calls", label: "API Calls", format: (n) => n.toLocaleString() },
  {
    key: "storage_bytes",
    label: "Storage",
    format: (n) => {
      if (n < 1024) return `${n} B`;
      if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
      if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
      return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
    },
  },
  { key: "krecord_count", label: "Records", format: (n) => n.toLocaleString() },
  { key: "user_seats", label: "Seats", format: (n) => n.toLocaleString() },
];

export function UsageDashboardPage() {
  const tenantId = tenantKey();
  const usageQuery = useQuery({
    queryKey: ["tenant-usage", tenantId],
    queryFn: () => api.getTenantUsage(tenantId),
  });
  const historyQuery = useQuery({
    queryKey: ["tenant-usage-history", tenantId, 6],
    queryFn: () => api.getTenantUsageHistory(tenantId, 6),
  });
  const plansQuery = useQuery({
    queryKey: ["plans"],
    queryFn: () => api.listPlans(),
  });

  if (usageQuery.isLoading) return <div>Loading usage…</div>;
  if (usageQuery.error) return <div>Error loading usage.</div>;
  const data = usageQuery.data;
  if (!data) return null;

  return (
    <section>
      <h1>Usage</h1>
      <p style={{ color: "#6b7280", fontSize: 13 }}>
        Plan: <strong>{data.plan}</strong> &middot; Period starting{" "}
        {new Date(data.period_start).toLocaleDateString()}
      </p>
      <div style={{ marginTop: 24 }}>
        {METRIC_ORDER.map(({ key, label, format }) => {
          const value = data.usage[key] ?? 0;
          const limit = (data.limits as PlanLimits)[key] ?? 0;
          const pct = limit > 0 ? Math.min(100, (value / limit) * 100) : 0;
          const over = limit > 0 && value > limit;
          return (
            <div key={key} style={{ marginBottom: 18 }}>
              <div
                style={{
                  display: "flex",
                  justifyContent: "space-between",
                  marginBottom: 4,
                }}
              >
                <strong>{label}</strong>
                <span style={{ fontVariantNumeric: "tabular-nums" }}>
                  {format(value)} {limit > 0 ? `/ ${format(limit)}` : ""}
                </span>
              </div>
              <div
                style={{
                  background: "#f3f4f6",
                  height: 14,
                  borderRadius: 6,
                  overflow: "hidden",
                }}
              >
                <div
                  style={{
                    width: `${pct}%`,
                    height: "100%",
                    background: over ? "#dc2626" : pct > 80 ? "#f59e0b" : "#10b981",
                    transition: "width 200ms ease",
                  }}
                />
              </div>
            </div>
          );
        })}
      </div>
      {historyQuery.data && historyQuery.data.rows.length > 0 && (
        <section style={{ marginTop: 32 }}>
          <h2 style={{ fontSize: 16 }}>Last 6 months</h2>
          <UsageHistoryChart rows={historyQuery.data.rows} />
        </section>
      )}
      {plansQuery.data && (
        <section style={{ marginTop: 32 }}>
          <h2 style={{ fontSize: 16 }}>Available plans</h2>
          <table style={{ width: "100%", borderCollapse: "collapse", marginTop: 8 }}>
            <thead>
              <tr>
                <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb", padding: "6px 4px" }}>
                  Name
                </th>
                <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb", padding: "6px 4px" }}>
                  API Calls
                </th>
                <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb", padding: "6px 4px" }}>
                  Storage
                </th>
                <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb", padding: "6px 4px" }}>
                  Seats
                </th>
              </tr>
            </thead>
            <tbody>
              {plansQuery.data.plans.map((p) => (
                <tr key={p.name} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td style={{ padding: "6px 4px" }}>
                    {p.display_name} {p.name === data.plan ? " (current)" : ""}
                  </td>
                  <td style={{ padding: "6px 4px" }}>
                    {(p.limits.api_calls ?? 0).toLocaleString()}
                  </td>
                  <td style={{ padding: "6px 4px" }}>
                    {((p.limits.storage_bytes ?? 0) / (1024 * 1024 * 1024)).toFixed(1)} GB
                  </td>
                  <td style={{ padding: "6px 4px" }}>
                    {(p.limits.user_seats ?? 0).toLocaleString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
    </section>
  );
}

// UsageHistoryChart renders a simple per-metric stacked bar series
// over the supplied (period_start, metric, value) rows. No external
// charting library is pulled in — a tiny div-based bar grouped by
// metric keeps the bundle small and matches the rest of the
// dashboard's visual vocabulary.
function UsageHistoryChart({
  rows,
}: {
  rows: Array<{ period_start: string; metric: string; value: number }>;
}) {
  // Pivot rows -> { period: { metric: value } }.
  const periods = Array.from(new Set(rows.map((r) => r.period_start))).sort();
  const metrics = Array.from(new Set(rows.map((r) => r.metric))).sort();
  const byPeriod = new Map<string, Map<string, number>>();
  for (const r of rows) {
    if (!byPeriod.has(r.period_start)) byPeriod.set(r.period_start, new Map());
    byPeriod.get(r.period_start)!.set(r.metric, r.value);
  }
  return (
    <div>
      {metrics.map((m) => {
        const values = periods.map((p) => byPeriod.get(p)?.get(m) ?? 0);
        const max = Math.max(...values, 1);
        return (
          <div key={m} style={{ marginBottom: 18 }}>
            <div
              style={{
                fontSize: 12,
                color: "#374151",
                marginBottom: 4,
                textTransform: "uppercase",
              }}
            >
              {m.replaceAll("_", " ")}
            </div>
            <div style={{ display: "flex", gap: 4, alignItems: "flex-end", height: 80 }}>
              {periods.map((p, i) => {
                const v = values[i];
                const h = (v / max) * 100;
                return (
                  <div
                    key={p}
                    title={`${new Date(p).toLocaleDateString()} — ${v}`}
                    style={{
                      flex: 1,
                      background: "#3b82f6",
                      height: `${h}%`,
                      minHeight: v > 0 ? 4 : 0,
                      borderRadius: "4px 4px 0 0",
                    }}
                  />
                );
              })}
            </div>
            <div style={{ display: "flex", gap: 4, marginTop: 4 }}>
              {periods.map((p) => (
                <div key={p} style={{ flex: 1, fontSize: 10, textAlign: "center", color: "#6b7280" }}>
                  {new Date(p).toLocaleDateString(undefined, { month: "short" })}
                </div>
              ))}
            </div>
          </div>
        );
      })}
    </div>
  );
}
