import { useMemo, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { DashboardSummary } from "@kapp/client";
import {
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  EmptyState,
  Eyebrow,
  Skeleton,
  StatCard,
} from "@kapp/ui";
import {
  AlertTriangle,
  ArrowRight,
  ClipboardCheck,
  ClipboardList,
  LifeBuoy,
  PackageX,
  Plus,
  Receipt,
  TrendingUp,
  Users,
  Wallet,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { useTenantName } from "../lib/tenant";

/**
 * DashboardPage answers "how is my business doing and what needs me
 * today?" at a glance. It pairs an Action center (the few things that
 * need a decision now) with a KPI grid; every surface deep-links to the
 * underlying worklist so an operator can drill in.
 */
export function DashboardPage() {
  const q = useQuery<DashboardSummary>({
    queryKey: ["dashboard", "summary"],
    queryFn: () => api.getDashboardSummary(),
  });
  // Locale-aware Intl formatter — picks up the active LocaleContext tag
  // so a pt-BR / es / fr-CA tenant sees "R$ 1.234", "1 234 $" instead
  // of the en-US "$1,234". The currency code itself is the tenant's
  // base currency reported by the API.
  const fmt = useFormatter();

  return (
    <section className="flex flex-col gap-6">
      <DashboardGreeting />

      {q.isLoading ? (
        <DashboardSkeleton />
      ) : q.isError ? (
        <Card>
          <CardContent className="pt-4">
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
          </CardContent>
        </Card>
      ) : q.data ? (
        <>
          <ActionCenter summary={q.data} formatter={fmt} />
          <section className="flex flex-col gap-3">
            <Eyebrow>Key metrics</Eyebrow>
            <KpiGrid summary={q.data} formatter={fmt} />
          </section>
        </>
      ) : null}
    </section>
  );
}

const QUICK_CREATE: { label: string; to: string }[] = [
  { label: "New deal", to: "/records/crm.deal/new" },
  { label: "New ticket", to: "/records/helpdesk.ticket/new" },
  { label: "New invoice", to: "/records/finance.ar_invoice/new" },
];

/**
 * Time-of-day greeting header. The tenant name stands in for the
 * signed-in user (the web app has no per-user identity surface yet) and
 * is resolved via `useTenantName` so the heading shows the display name
 * ("Acme Corp"), never the raw tenant UUID. Quick-create actions sit on
 * the trailing edge so creating common records is always one click away.
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
    <header className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
      <div className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          {part}, {name}
        </h1>
        <p className="text-sm text-fg-muted">
          {dateLabel} · Here&apos;s what needs you and how the business is
          tracking.
        </p>
      </div>
      <div className="flex flex-wrap gap-2">
        {QUICK_CREATE.map((action) => (
          <Button key={action.to} asChild variant="secondary" size="sm">
            <Link to={action.to}>
              <Plus className="h-4 w-4" aria-hidden="true" />
              {action.label}
            </Link>
          </Button>
        ))}
      </div>
    </header>
  );
}

type Formatter = ReturnType<typeof useFormatter>;

/** Bind the formatter into a `formatAmount(value, currency?)` closure. */
function makeFormatAmount(formatter: Formatter) {
  return (value: number, currency?: string): string => {
    if (currency) {
      try {
        return formatter.currency(value, currency, {
          maximumFractionDigits: 0,
        });
      } catch {
        // Synthetic / non-ISO currency codes make Intl throw on
        // construction; fall back to a bare locale-aware number rather
        // than crash the dashboard.
      }
    }
    return formatter.number(value, { maximumFractionDigits: 0 });
  };
}

type ActionTone = "danger" | "warning" | "info";

interface ActionItem {
  key: string;
  label: string;
  hint: string;
  value: string;
  to: string;
  icon: ReactNode;
  tone: ActionTone;
}

const ACTION_TONE: Record<ActionTone, string> = {
  danger: "text-danger",
  warning: "text-warning",
  info: "text-info",
};

/**
 * The "what needs me today" surface: a short, prioritised list of the
 * decisions and exceptions that warrant attention right now. Only items
 * with a non-zero count surface; when everything is clear the operator
 * gets a calm all-caught-up state instead of empty rows.
 */
function ActionCenter({
  summary,
  formatter,
}: {
  summary: DashboardSummary;
  formatter: Formatter;
}) {
  const s = summary;
  const formatAmount = makeFormatAmount(formatter);

  const items: ActionItem[] = [];
  if (s.pending_approvals > 0) {
    items.push({
      key: "approvals",
      label: "Approvals to review",
      hint: "Decisions waiting on you",
      value: formatter.number(s.pending_approvals),
      to: "/approvals",
      icon: <ClipboardCheck className="h-5 w-5" aria-hidden="true" />,
      tone: "warning",
    });
  }
  if (s.overdue_tickets_count > 0) {
    items.push({
      key: "overdue-tickets",
      label: "Tickets overdue",
      hint: "Past their resolution target",
      value: formatter.number(s.overdue_tickets_count),
      to: "/helpdesk",
      icon: <LifeBuoy className="h-5 w-5" aria-hidden="true" />,
      tone: "danger",
    });
  }
  if (s.low_stock_items_count > 0) {
    items.push({
      key: "low-stock",
      label: "Items to reorder",
      hint: "Below their reorder point",
      value: formatter.number(s.low_stock_items_count),
      to: "/inventory/stock-levels",
      icon: <PackageX className="h-5 w-5" aria-hidden="true" />,
      tone: "warning",
    });
  }
  if (s.outstanding_ar > 0) {
    items.push({
      key: "receivables",
      label: "Receivables to collect",
      hint: "Outstanding from customers",
      value: formatAmount(s.outstanding_ar, s.base_currency),
      to: "/records/finance.ar_invoice",
      icon: <Receipt className="h-5 w-5" aria-hidden="true" />,
      tone: "info",
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Needs your attention</CardTitle>
        <CardDescription>
          The decisions and exceptions worth a look right now.
        </CardDescription>
      </CardHeader>
      <CardContent className="pt-0">
        {items.length === 0 ? (
          <p className="rounded-md bg-bg-subtle px-4 py-6 text-center text-sm text-fg-muted">
            You&apos;re all caught up — nothing needs you right now.
          </p>
        ) : (
          <ul className="flex flex-col divide-y divide-border">
            {items.map((item) => (
              <li key={item.key}>
                <Link
                  to={item.to}
                  className="group flex items-center gap-3 rounded-md px-2 py-3 transition-colors hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-2 focus-visible:ring-offset-(--bg)"
                >
                  <span className={ACTION_TONE[item.tone]}>{item.icon}</span>
                  <span className="flex min-w-0 flex-1 flex-col">
                    <span className="truncate text-sm font-medium text-fg">
                      {item.label}
                    </span>
                    <span className="truncate text-xs text-fg-subtle">
                      {item.hint}
                    </span>
                  </span>
                  <span className="font-tabular text-base font-semibold text-fg">
                    {item.value}
                  </span>
                  <ArrowRight
                    className="h-4 w-4 shrink-0 text-fg-subtle transition-transform group-hover:translate-x-0.5"
                    aria-hidden="true"
                  />
                </Link>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

interface WidgetSpec {
  label: string;
  value: string | number;
  sub?: string;
  to: string;
  icon: ReactNode;
}

function KpiGrid({
  summary,
  formatter,
}: {
  summary: DashboardSummary;
  formatter: Formatter;
}) {
  const s = summary;
  const formatAmount = makeFormatAmount(formatter);

  const widgets: WidgetSpec[] = [
    {
      label: "Open deals",
      value: formatter.number(s.open_deals_count),
      sub: `Pipeline ${formatAmount(s.pipeline_value, s.base_currency)}`,
      to: "/records/crm.deal",
      icon: <TrendingUp />,
    },
    {
      label: "Outstanding AR",
      value: formatAmount(s.outstanding_ar, s.base_currency),
      sub: `Owed to you · in ${s.base_currency}`,
      to: "/records/finance.ar_invoice",
      icon: <Receipt />,
    },
    {
      label: "Outstanding AP",
      value: formatAmount(s.outstanding_ap, s.base_currency),
      sub: `You owe · in ${s.base_currency}`,
      to: "/records/finance.ap_bill",
      icon: <Wallet />,
    },
    {
      label: "Low-stock items",
      value: formatter.number(s.low_stock_items_count),
      sub: "Below reorder point",
      to: "/inventory/stock-levels",
      icon: <PackageX />,
    },
    {
      label: "Pending approvals",
      value: formatter.number(s.pending_approvals),
      sub: "Waiting on you",
      to: "/approvals",
      icon: <ClipboardCheck />,
    },
    {
      label: "Open tickets",
      value: formatter.number(s.open_tickets_count),
      sub: `${formatter.number(s.overdue_tickets_count)} overdue`,
      to: "/helpdesk",
      icon: <LifeBuoy />,
    },
    {
      label: "Present today",
      value: formatter.number(s.present_today ?? 0),
      sub: "Attendance — today",
      to: "/records/hr.attendance",
      icon: <Users />,
    },
    {
      label: "Pending reviews",
      value: formatter.number(s.pending_reviews ?? 0),
      sub: "Performance reviews",
      to: "/records/hr.appraisal",
      icon: <ClipboardList />,
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
          icon={w.icon}
          renderContainer={({ className, children }) => (
            <Link to={w.to} className={className} aria-label={`${w.label} — open list`}>
              {children}
            </Link>
          )}
        />
      ))}
    </div>
  );
}

/** Loading placeholder matching the action center + eight-tile KPI grid. */
function DashboardSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      <div className="rounded-lg border border-border bg-bg-elevated p-4">
        <Skeleton variant="text" className="w-40" />
        <div className="mt-4 flex flex-col gap-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <div key={i} className="flex items-center gap-3">
              <Skeleton variant="circle" className="h-8 w-8" />
              <div className="flex flex-1 flex-col gap-1">
                <Skeleton variant="text" className="w-32" />
                <Skeleton variant="text" className="w-48" />
              </div>
              <Skeleton className="h-5 w-8" />
            </div>
          ))}
        </div>
      </div>
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
    </div>
  );
}
