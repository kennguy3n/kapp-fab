import { useQuery } from "@tanstack/react-query";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
      <p className="text-[13px] text-fg-muted">
        Plan: <strong>{data.plan}</strong> &middot; Period starting{" "}
        {new Date(data.period_start).toLocaleDateString()}
      </p>
      <div className="mt-6">
        {METRIC_ORDER.map(({ key, label, format }) => {
          const value = data.usage[key] ?? 0;
          const limit = (data.limits as PlanLimits)[key] ?? 0;
          const pct = limit > 0 ? Math.min(100, (value / limit) * 100) : 0;
          const over = limit > 0 && value > limit;
          return (
            <div key={key} className="mb-[18px]">
              <div className="mb-1 flex justify-between">
                <strong>{label}</strong>
                <span className="tabular-nums">
                  {format(value)} {limit > 0 ? `/ ${format(limit)}` : ""}
                </span>
              </div>
              <div className="h-3.5 overflow-hidden rounded-md bg-bg-muted">
                <div
                  className="h-full transition-all duration-200"
                  style={{
                    width: `${pct}%`,
                    background: over
                      ? "var(--danger)"
                      : pct > 80
                        ? "var(--warning)"
                        : "var(--success)",
                  }}
                />
              </div>
            </div>
          );
        })}
      </div>
      {historyQuery.data && historyQuery.data.rows.length > 0 && (
        <section className="mt-8">
          <h2 className="text-base">Last 6 months</h2>
          <UsageHistoryChart rows={historyQuery.data.rows} />
        </section>
      )}
      {plansQuery.data && (
        <section className="mt-8">
          <h2 className="text-base">Available plans</h2>
          <Table className="mt-2">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>API Calls</TableHead>
                <TableHead>Storage</TableHead>
                <TableHead>Seats</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {plansQuery.data.plans.map((p) => (
                <TableRow key={p.name}>
                  <TableCell>
                    {p.display_name} {p.name === data.plan ? " (current)" : ""}
                  </TableCell>
                  <TableCell>
                    {(p.limits.api_calls ?? 0).toLocaleString()}
                  </TableCell>
                  <TableCell>
                    {((p.limits.storage_bytes ?? 0) / (1024 * 1024 * 1024)).toFixed(1)} GB
                  </TableCell>
                  <TableCell>
                    {(p.limits.user_seats ?? 0).toLocaleString()}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
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
          <div key={m} className="mb-[18px]">
            <div className="mb-1 text-xs uppercase text-fg">
              {m.replaceAll("_", " ")}
            </div>
            <div className="flex h-20 items-end gap-1">
              {periods.map((p, i) => {
                const v = values[i];
                const h = (v / max) * 100;
                return (
                  <div
                    key={p}
                    title={`${new Date(p).toLocaleDateString()} — ${v}`}
                    className="flex-1 rounded-t bg-accent"
                    style={{
                      height: `${h}%`,
                      minHeight: v > 0 ? 4 : 0,
                    }}
                  />
                );
              })}
            </div>
            <div className="mt-1 flex gap-1">
              {periods.map((p) => (
                <div key={p} className="flex-1 text-center text-[10px] text-fg-muted">
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
