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
