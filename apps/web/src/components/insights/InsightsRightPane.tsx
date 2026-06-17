// InsightsRightPane renders a compact dashboard preview in the
// right-hand pane of KChat (and any host that wants to show a
// dashboard summary alongside other content). The component intentionally
// uses the same data path as the full dashboard page — it calls
// GET /insights/dashboards/{id} and renders one Viz per widget — but
// trims the chrome (no edit affordances, no share modal, no widget
// CRUD) so it fits in a 380 px column without clutter.
//
// Hosts pass the dashboard id and an optional onOpenFull handler that
// navigates to the full-page experience when the user wants to drill
// in. Errors and the empty state are inlined so the host doesn't
// need to model loading itself.

import { useQuery } from "@tanstack/react-query";
import type { InsightsDashboardBundle, InsightsRunResult } from "@kapp/client";
import { Button, Skeleton } from "@kapp/ui";
import { api } from "../../lib/api";
import { Viz } from "./Charts";

interface Props {
  dashboardId: string;
  onClose?: () => void;
  onOpenFull?: (dashboardId: string) => void;
}

export function InsightsRightPane({ dashboardId, onClose, onOpenFull }: Props) {
  const bundle = useQuery({
    queryKey: ["insights-dashboard-mini", dashboardId],
    queryFn: () => api.getInsightsDashboard(dashboardId),
    enabled: !!dashboardId,
  });

  return (
    <aside
      aria-label="Insights dashboard preview"
      className="sticky top-0 flex h-screen w-[380px] flex-col gap-3 overflow-y-auto border-l border-border bg-bg p-4"
    >
      <header className="flex items-center justify-between gap-2">
        <h3 className="min-w-0 truncate text-sm font-semibold text-fg">
          {bundle.data?.dashboard.name ?? "Dashboard"}
        </h3>
        <div className="flex shrink-0 items-center gap-1">
          {onOpenFull && (
            <Button
              size="sm"
              variant="outline"
              onClick={() => onOpenFull(dashboardId)}
            >
              Open
            </Button>
          )}
          {onClose && (
            <Button
              size="icon"
              variant="ghost"
              aria-label="Close"
              onClick={onClose}
            >
              ✕
            </Button>
          )}
        </div>
      </header>

      {bundle.isLoading && (
        <div className="flex flex-col gap-3" aria-hidden>
          <Skeleton className="h-40 w-full" />
          <Skeleton className="h-40 w-full" />
        </div>
      )}
      {bundle.isError && (
        <div className="rounded-lg border border-border p-4 text-center">
          <p className="text-sm text-fg-muted">
            We couldn’t load this dashboard.
          </p>
          <Button
            size="sm"
            variant="outline"
            className="mt-2"
            onClick={() => bundle.refetch()}
          >
            Try again
          </Button>
        </div>
      )}

      {bundle.data && <MiniDashboard bundle={bundle.data} />}
    </aside>
  );
}

function MiniDashboard({ bundle }: { bundle: InsightsDashboardBundle }) {
  const widgets = bundle.dashboard.widgets ?? [];
  if (widgets.length === 0) {
    return (
      <p className="text-sm text-fg-muted">
        This dashboard doesn’t have any charts yet.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-3">
      {widgets.map((w) => {
        const run: InsightsRunResult | null =
          bundle.widget_results[w.id] ?? null;
        const result = run?.result ?? { columns: [], rows: [] };
        return (
          <section
            key={w.id}
            className="flex flex-col gap-2 rounded-lg border border-border bg-bg-elevated p-3"
          >
            <header className="truncate text-xs font-medium text-fg-muted">
              {w.config.title ?? "Untitled chart"}
            </header>
            <Viz
              vizType={w.viz_type}
              result={result}
              config={w.config}
              height={140}
            />
          </section>
        );
      })}
    </div>
  );
}
