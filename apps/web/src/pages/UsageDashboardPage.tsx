import { useQuery } from "@tanstack/react-query";
import { BarChart3, RefreshCw } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  cn,
  EmptyState,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
} from "@kapp/ui";
import { api } from "../lib/api";
import { tenantKey } from "../lib/tenant";
import { useFormatter } from "../lib/i18n";
import { humanizeLabel, humanizeToken } from "../lib/ktypeView";
import { AdminErrorState, AdminPageHeader } from "./adminKit";
import type { PlanLimits } from "@kapp/client";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

const METRICS: Array<{
  key: string;
  label: string;
  format: (n: number, fmt: ReturnType<typeof useFormatter>) => string;
}> = [
  { key: "api_calls", label: "API calls", format: (n, fmt) => fmt.number(n) },
  { key: "storage_bytes", label: "Storage", format: (n) => formatBytes(n) },
  { key: "krecord_count", label: "Records", format: (n, fmt) => fmt.number(n) },
  { key: "user_seats", label: "Seats", format: (n, fmt) => fmt.number(n) },
];

function usageStatus(value: number, limit: number): {
  label: string;
  variant: BadgeVariant;
  bar: string;
} {
  if (limit <= 0)
    return { label: "No limit", variant: "neutral", bar: "bg-fg-subtle" };
  if (value > limit)
    return { label: "Over limit", variant: "danger", bar: "bg-danger" };
  if (value / limit >= 0.8)
    return { label: "Approaching", variant: "warning", bar: "bg-warning" };
  return { label: "Healthy", variant: "success", bar: "bg-success" };
}

export function UsageDashboardPage() {
  const fmt = useFormatter();
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

  const data = usageQuery.data;

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Usage"
        description="Track this workspace's consumption against its plan limits. The daily meter rolls up API calls, storage, records, and seats."
        actions={
          <div className="flex flex-wrap items-center gap-2">
            {data && (
              <Badge variant="accent" size="md">
                {data.plan ? humanizeToken(data.plan) : "—"} plan
              </Badge>
            )}
            <Button
              variant="secondary"
              size="sm"
              leadingIcon={<RefreshCw className="h-4 w-4" />}
              disabled={usageQuery.isFetching}
              onClick={() => {
                void usageQuery.refetch();
                void historyQuery.refetch();
              }}
            >
              {usageQuery.isFetching ? "Refreshing…" : "Refresh"}
            </Button>
          </div>
        }
      />

      {usageQuery.isLoading ? (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Card key={i}>
              <CardContent className="flex flex-col gap-3 pt-4">
                <Skeleton variant="text" className="w-20" />
                <Skeleton variant="text" className="h-7 w-28" />
                <Skeleton variant="rect" className="h-2 w-full" />
              </CardContent>
            </Card>
          ))}
        </div>
      ) : usageQuery.error ? (
        <AdminErrorState
          title="Couldn't load usage"
          error={usageQuery.error}
          onRetry={() => usageQuery.refetch()}
        />
      ) : !data ? (
        <EmptyState
          icon={<BarChart3 />}
          title="No usage recorded yet"
          description="Usage appears here once the daily meter runs for this workspace."
        />
      ) : (
        <>
          <p className="text-xs text-fg-muted">
            Billing period started {fmt.date(new Date(data.period_start))}
            {usageQuery.dataUpdatedAt
              ? ` · Updated ${fmt.dateTime(new Date(usageQuery.dataUpdatedAt))}`
              : ""}
          </p>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {METRICS.map(({ key, label, format }) => {
              const value = data.usage[key] ?? 0;
              const limit = (data.limits as PlanLimits)[key] ?? 0;
              const pct = limit > 0 ? Math.min(100, (value / limit) * 100) : 0;
              const status = usageStatus(value, limit);
              return (
                <Card key={key}>
                  <CardContent className="flex flex-col gap-2 pt-4">
                    <div className="flex items-center justify-between gap-2">
                      <span className="truncate text-sm text-fg-muted">
                        {label}
                      </span>
                      <Badge variant={status.variant} size="xs">
                        {status.label}
                      </Badge>
                    </div>
                    <div className="font-tabular text-2xl font-semibold text-fg">
                      {format(value, fmt)}
                    </div>
                    <div className="text-xs text-fg-muted">
                      {limit > 0
                        ? `of ${format(limit, fmt)}`
                        : "No plan limit"}
                    </div>
                    <div
                      className="mt-1 h-2 overflow-hidden rounded-pill bg-bg-muted"
                      role="progressbar"
                      aria-valuenow={Math.round(pct)}
                      aria-valuemin={0}
                      aria-valuemax={100}
                      aria-label={`${label} usage`}
                    >
                      <div
                        className={cn("h-full rounded-pill", status.bar)}
                        style={{ width: `${limit > 0 ? pct : 0}%` }}
                      />
                    </div>
                  </CardContent>
                </Card>
              );
            })}
          </div>

          {historyQuery.isLoading ? (
            <Card>
              <CardContent className="pt-4">
                <Skeleton variant="rect" className="h-24 w-full" />
              </CardContent>
            </Card>
          ) : historyQuery.data && historyQuery.data.rows.length > 0 ? (
            <Card>
              <CardHeader>
                <CardTitle>Usage trend</CardTitle>
              </CardHeader>
              <CardContent>
                <UsageHistoryChart rows={historyQuery.data.rows} />
              </CardContent>
            </Card>
          ) : null}

          {plansQuery.data && plansQuery.data.plans.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle>Available plans</CardTitle>
              </CardHeader>
              <CardContent>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Plan</TableHead>
                      <TableHead className="text-end">API calls</TableHead>
                      <TableHead className="text-end">Storage</TableHead>
                      <TableHead className="text-end">Seats</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {plansQuery.data.plans.map((p) => {
                      const current = p.name === data.plan;
                      return (
                        <TableRow key={p.name}>
                          <TableCell>
                            <span className="font-medium text-fg">
                              {p.display_name || humanizeToken(p.name)}
                            </span>
                            {current && (
                              <Badge
                                variant="accent"
                                size="xs"
                                className="ms-2"
                              >
                                Current
                              </Badge>
                            )}
                          </TableCell>
                          <TableCell className="text-end font-tabular">
                            {fmt.number(p.limits.api_calls ?? 0)}
                          </TableCell>
                          <TableCell className="text-end font-tabular">
                            {formatBytes(p.limits.storage_bytes ?? 0)}
                          </TableCell>
                          <TableCell className="text-end font-tabular">
                            {fmt.number(p.limits.user_seats ?? 0)}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          )}
        </>
      )}
    </section>
  );
}

// UsageHistoryChart renders a small per-metric bar series over the
// supplied (period_start, metric, value) rows. No external charting
// library is pulled in — token-styled bars keep the bundle small and
// match the rest of the dashboard's visual vocabulary.
function UsageHistoryChart({
  rows,
}: {
  rows: Array<{ period_start: string; metric: string; value: number }>;
}) {
  const fmt = useFormatter();
  const periods = Array.from(new Set(rows.map((r) => r.period_start))).sort();
  const metrics = Array.from(new Set(rows.map((r) => r.metric))).sort();
  const byPeriod = new Map<string, Map<string, number>>();
  for (const r of rows) {
    if (!byPeriod.has(r.period_start)) byPeriod.set(r.period_start, new Map());
    byPeriod.get(r.period_start)!.set(r.metric, r.value);
  }
  return (
    <div className="flex flex-col gap-6">
      {metrics.map((m) => {
        const values = periods.map((p) => byPeriod.get(p)?.get(m) ?? 0);
        const max = Math.max(...values, 1);
        return (
          <div key={m} className="flex flex-col gap-1">
            <div className="text-xs font-medium text-fg-muted">
              {humanizeLabel(m)}
            </div>
            <div className="flex h-24 items-end gap-1.5">
              {periods.map((p, i) => {
                const v = values[i]!;
                const h = (v / max) * 100;
                return (
                  <div
                    key={p}
                    title={`${fmt.date(new Date(p))} — ${fmt.number(v)}`}
                    aria-label={`${fmt.date(new Date(p))}: ${fmt.number(v)}`}
                    className="flex-1 rounded-t-sm bg-accent/80"
                    style={{ height: `${h}%`, minHeight: v > 0 ? 4 : 0 }}
                  />
                );
              })}
            </div>
            <div className="flex gap-1.5">
              {periods.map((p) => (
                <div
                  key={p}
                  className="flex-1 text-center text-[10px] text-fg-muted"
                >
                  {fmt.date(new Date(p), { month: "short" })}
                </div>
              ))}
            </div>
          </div>
        );
      })}
    </div>
  );
}
