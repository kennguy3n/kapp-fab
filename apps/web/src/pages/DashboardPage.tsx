import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { DashboardSummary } from "@kapp/client";
import {
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Skeleton,
  StatCard,
} from "@kapp/ui";
import { AlertTriangle } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { useTenantName } from "../lib/tenant";

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

  return (
    <section className="flex flex-col gap-6">
      <DashboardGreeting />

      <Card>
        <CardHeader>
          <CardTitle>Key metrics</CardTitle>
        </CardHeader>
        <CardContent>
          {q.isLoading ? (
            <DashboardSkeleton />
          ) : q.isError ? (
            <EmptyState
              icon={<AlertTriangle />}
              title="Couldn't load the dashboard"
              description={`Failed to load dashboard: ${(q.error as Error).message}`}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void q.refetch()}
                  disabled={q.isFetching}
                >
                  Retry
                </Button>
              }
            />
          ) : q.data ? (
            <DashboardGrid summary={q.data} formatter={fmt} />
          ) : null}
        </CardContent>
      </Card>
    </section>
  );
}

/**
 * Time-of-day greeting header.  The tenant name stands in for the
 * signed-in user (the web app has no per-user identity surface yet)
 * and is resolved via `useTenantName` so the heading shows the
 * display name ("Acme Corp"), never the raw tenant UUID.
 */
function DashboardGreeting() {
  // Snapshot the clock once per mount so the heading is deterministic
  // across re-renders (a fresh `new Date()` each render could otherwise
  // flip the greeting at an hour boundary).
  const { part, dateLabel } = useMemo(() => {
    const now = new Date();
    const hour = now.getHours();
    return {
      part:
        hour < 12
          ? "Good morning"
          : hour < 18
            ? "Good afternoon"
            : "Good evening",
      dateLabel: now.toLocaleDateString(undefined, {
        weekday: "long",
        year: "numeric",
        month: "long",
        day: "numeric",
      }),
    };
  }, []);
  const { name } = useTenantName();
  return (
    <header className="flex flex-col gap-1">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        {part}, {name}
      </h1>
      <p className="text-sm text-fg-muted">
        {dateLabel} · At-a-glance KPIs. Each tile links to the underlying
        worklist.
      </p>
    </header>
  );
}

type Formatter = ReturnType<typeof useFormatter>;

interface WidgetSpec {
  label: string;
  value: string | number;
  sub?: string;
  to: string;
}

function DashboardGrid({
  summary,
  formatter,
}: {
  summary: DashboardSummary;
  formatter: Formatter;
}) {
  const s = summary;
  // Bind the formatter into a closure that mirrors the prior
  // formatAmount(value, currency?) signature. When the API doesn't
  // surface a currency code (older payloads) we fall back to a plain
  // locale-aware number — Intl.NumberFormat without style:"currency"
  // still honours grouping and decimal conventions.
  const formatAmount = (value: number, currency?: string): string => {
    if (currency) {
      try {
        return formatter.currency(value, currency, {
          maximumFractionDigits: 0,
        });
      } catch {
        // fall through to bare-number formatting (synthetic ISO
        // codes the runtime rejects on construction)
      }
    }
    return formatter.number(value, { maximumFractionDigits: 0 });
  };

  const widgets: WidgetSpec[] = [
    {
      label: "Open deals",
      value: s.open_deals_count,
      sub: `Pipeline ${formatAmount(s.pipeline_value, s.base_currency)}`,
      to: "/records/crm.deal",
    },
    {
      label: "Outstanding AR",
      value: formatAmount(s.outstanding_ar, s.base_currency),
      sub: `in ${s.base_currency}`,
      to: "/records/finance.ar_invoice",
    },
    {
      label: "Outstanding AP",
      value: formatAmount(s.outstanding_ap, s.base_currency),
      sub: `in ${s.base_currency}`,
      to: "/records/finance.ap_bill",
    },
    {
      label: "Low-stock items",
      value: s.low_stock_items_count,
      to: "/inventory/stock-levels",
    },
    {
      label: "Pending approvals",
      value: s.pending_approvals,
      to: "/approvals",
    },
    {
      label: "Open tickets",
      value: s.open_tickets_count,
      sub: `${s.overdue_tickets_count} overdue`,
      to: "/helpdesk",
    },
    {
      label: "Present today",
      value: s.present_today ?? 0,
      sub: "hr.attendance — UTC day",
      to: "/records/hr.attendance",
    },
    {
      label: "Pending reviews",
      value: s.pending_reviews ?? 0,
      sub: "submitted + reviewed",
      to: "/records/hr.appraisal",
    },
  ];

  return (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(220px,1fr))] gap-3">
      {widgets.map((w) => (
        <StatCard
          key={w.to + w.label}
          label={w.label}
          value={w.value}
          sub={w.sub}
          renderContainer={({ className, children }) => (
            <Link to={w.to} className={className}>
              {children}
            </Link>
          )}
        />
      ))}
    </div>
  );
}

/** Loading placeholder matching the eight-tile KPI grid. */
function DashboardSkeleton() {
  return (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(220px,1fr))] gap-3">
      {Array.from({ length: 8 }).map((_, i) => (
        <div
          key={i}
          className="rounded-lg border border-border bg-bg-elevated p-4"
        >
          <Skeleton variant="text" className="w-24" />
          <Skeleton className="mt-3 h-7 w-16" />
          <Skeleton variant="text" className="mt-3 w-28" />
        </div>
      ))}
    </div>
  );
}
