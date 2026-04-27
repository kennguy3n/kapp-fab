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
      style={{
        width: 380,
        borderLeft: "1px solid #e5e7eb",
        padding: 16,
        background: "#ffffff",
        display: "flex",
        flexDirection: "column",
        gap: 12,
        position: "sticky",
        top: 0,
        height: "100vh",
        overflowY: "auto",
      }}
      aria-label="Insights dashboard preview"
    >
      <header
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          gap: 8,
        }}
      >
        <h3 style={{ margin: 0, fontSize: 14, fontWeight: 600 }}>
          {bundle.data?.dashboard.name ?? "Dashboard"}
        </h3>
        <div style={{ display: "flex", gap: 4 }}>
          {onOpenFull && (
            <button
              onClick={() => onOpenFull(dashboardId)}
              style={{
                background: "transparent",
                border: "1px solid #d1d5db",
                padding: "4px 8px",
                fontSize: 12,
                cursor: "pointer",
              }}
            >
              Open
            </button>
          )}
          {onClose && (
            <button onClick={onClose} aria-label="Close">
              ×
            </button>
          )}
        </div>
      </header>

      {bundle.isLoading && <p style={{ color: "#6b7280" }}>Loading…</p>}
      {bundle.error && (
        <p style={{ color: "#b91c1c", fontSize: 13 }}>
          Failed to load dashboard: {(bundle.error as Error).message}
        </p>
      )}

      {bundle.data && <MiniDashboard bundle={bundle.data} />}
    </aside>
  );
}

function MiniDashboard({ bundle }: { bundle: InsightsDashboardBundle }) {
  const widgets = bundle.dashboard.widgets ?? [];
  if (widgets.length === 0) {
    return (
      <p style={{ color: "#6b7280", fontSize: 13 }}>
        This dashboard has no widgets yet.
      </p>
    );
  }
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      {widgets.map((w) => {
        const run: InsightsRunResult | null =
          bundle.widget_results[w.id] ?? null;
        const result = run?.result ?? { columns: [], rows: [] };
        return (
          <section
            key={w.id}
            style={{
              border: "1px solid #e5e7eb",
              borderRadius: 6,
              padding: 12,
              display: "flex",
              flexDirection: "column",
              gap: 8,
            }}
          >
            <header style={{ fontSize: 12, color: "#6b7280" }}>
              {w.config.title ?? w.viz_type}
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
