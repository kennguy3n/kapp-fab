// Phase L Insights — dashboard builder.
//
// Picks an existing dashboard (or creates a new one) and renders its
// widgets in a 12-column CSS grid. Each widget binds to a saved
// insights query and selects a viz_type; the per-widget run result
// arrives bundled with the dashboard payload so the page renders
// without a per-widget fan-out. Linked filters live in the dashboard
// `layout` blob — picking a value on one widget re-runs every widget
// whose config maps the same `linked_filter_key`.

import { useEffect, useMemo, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import type {
  InsightsDashboard,
  InsightsDashboardBundle,
  InsightsQuery,
  InsightsRunResult,
  InsightsVizType,
  InsightsWidget,
  InsightsWidgetConfig,
} from "@kapp/client";
import { api } from "../lib/api";
import { Viz } from "../components/insights/Charts";
import { ShareModal } from "../components/insights/ShareModal";

const VIZ_OPTIONS: InsightsVizType[] = [
  "table",
  "bar",
  "line",
  "pie",
  "donut",
  "funnel",
  "number_card",
  "pivot",
];

interface LinkedFilterValues {
  // dashboard layout.linked_filters: { [filter_key]: selected_value }
  [key: string]: unknown;
}

export function InsightsDashboardPage() {
  const qc = useQueryClient();

  const dashboardsQuery = useQuery<{ dashboards: InsightsDashboard[] }>({
    queryKey: ["insights-dashboards"],
    queryFn: () => api.listInsightsDashboards(),
  });
  const queriesQuery = useQuery<{ queries: InsightsQuery[] }>({
    queryKey: ["insights-queries"],
    queryFn: () => api.listInsightsQueries(),
  });

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [shareOpen, setShareOpen] = useState(false);
  const [linkedFilters, setLinkedFilters] = useState<LinkedFilterValues>({});
  const [error, setError] = useState<string | null>(null);

  const bundleQuery = useQuery<InsightsDashboardBundle>({
    queryKey: ["insights-dashboard", selectedId],
    queryFn: () => api.getInsightsDashboard(selectedId!),
    enabled: Boolean(selectedId),
  });

  // Per-widget run results — initially seeded from the bundle, then
  // refreshed when a linked filter selection changes (we re-run the
  // affected widgets through the runner with filter_params).
  const [widgetResults, setWidgetResults] = useState<
    Record<string, InsightsRunResult | null>
  >({});

  useEffect(() => {
    if (bundleQuery.data) {
      setWidgetResults(bundleQuery.data.widget_results);
      setLinkedFilters(
        (bundleQuery.data.dashboard.layout?.linked_filters ?? {}) as LinkedFilterValues
      );
    }
  }, [bundleQuery.data]);

  // Auto-refresh: re-fetches the dashboard every auto_refresh_seconds.
  // Falls back to off when the dashboard sets <= 0.
  const autoRefreshSec = bundleQuery.data?.dashboard.auto_refresh_seconds ?? 0;
  useEffect(() => {
    if (!selectedId || autoRefreshSec <= 0) return;
    const t = setInterval(() => {
      qc.invalidateQueries({ queryKey: ["insights-dashboard", selectedId] });
    }, autoRefreshSec * 1000);
    return () => clearInterval(t);
  }, [selectedId, autoRefreshSec, qc]);

  const createDashboardMut = useMutation({
    mutationFn: (name: string) =>
      api.createInsightsDashboard({
        name,
        auto_refresh_seconds: 0,
        layout: { linked_filters: {} },
      }),
    onSuccess: (d) => {
      setSelectedId(d.id);
      qc.invalidateQueries({ queryKey: ["insights-dashboards"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const updateDashboardMut = useMutation({
    mutationFn: (input: {
      name?: string;
      auto_refresh_seconds?: number;
      linked_filters?: LinkedFilterValues;
    }) => {
      if (!bundleQuery.data) throw new Error("dashboard not loaded");
      const d = bundleQuery.data.dashboard;
      return api.updateInsightsDashboard(d.id, {
        name: input.name ?? d.name,
        description: d.description,
        auto_refresh_seconds:
          input.auto_refresh_seconds ?? d.auto_refresh_seconds,
        layout: {
          ...d.layout,
          linked_filters: input.linked_filters ?? d.layout?.linked_filters,
        },
      });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["insights-dashboard", selectedId] });
      qc.invalidateQueries({ queryKey: ["insights-dashboards"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteDashboardMut = useMutation({
    mutationFn: (id: string) => api.deleteInsightsDashboard(id),
    onSuccess: () => {
      setSelectedId(null);
      qc.invalidateQueries({ queryKey: ["insights-dashboards"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const upsertWidgetMut = useMutation({
    mutationFn: (widget: InsightsWidget) =>
      api.upsertInsightsWidget(widget.dashboard_id, {
        id: widget.id || undefined,
        query_id: widget.query_id ?? null,
        viz_type: widget.viz_type,
        position: widget.position,
        config: widget.config,
      }),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["insights-dashboard", selectedId] }),
    onError: (err: Error) => setError(err.message),
  });

  const deleteWidgetMut = useMutation({
    mutationFn: (widget: InsightsWidget) =>
      api.deleteInsightsWidget(widget.dashboard_id, widget.id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["insights-dashboard", selectedId] }),
    onError: (err: Error) => setError(err.message),
  });

  // Re-run a single widget with the current linked filter selection
  // applied. Used both when the user changes a filter and when adding
  // / editing a widget that uses a filter.
  const rerunWidget = async (widget: InsightsWidget) => {
    if (!widget.query_id) return;
    const params: Record<string, unknown> = {};
    if (
      widget.config.linked_filter_column &&
      widget.config.linked_filter_key &&
      linkedFilters[widget.config.linked_filter_key] !== undefined &&
      linkedFilters[widget.config.linked_filter_key] !== ""
    ) {
      params[widget.config.linked_filter_column] =
        linkedFilters[widget.config.linked_filter_key];
    }
    try {
      const res = await api.runInsightsQuery(widget.query_id, {
        filter_params: params,
        bypass_cache: false,
      });
      setWidgetResults((cur) => ({ ...cur, [widget.id]: res }));
    } catch (err) {
      setError((err as Error).message);
    }
  };

  // When linked filter values change, re-run every widget that opts
  // into the changed key. Re-runs are independent so a slow query
  // doesn't block the others.
  useEffect(() => {
    if (!bundleQuery.data) return;
    const widgets = bundleQuery.data.dashboard.widgets ?? [];
    for (const w of widgets) {
      const k = w.config.linked_filter_key;
      if (k && linkedFilters[k] !== undefined) {
        rerunWidget(w);
      }
    }
    // We only want to re-run when filters change, not on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [linkedFilters]);

  const dashboard = bundleQuery.data?.dashboard;

  // Collect every linked-filter key declared by any widget so the
  // top-of-page filter bar can render an input for each.
  const linkedFilterKeys = useMemo(() => {
    if (!dashboard?.widgets) return [] as string[];
    const keys = new Set<string>();
    for (const w of dashboard.widgets) {
      if (w.config.linked_filter_key) keys.add(w.config.linked_filter_key);
    }
    return [...keys];
  }, [dashboard]);

  return (
    <section>
      <h1>Insights — Dashboards</h1>

      <div style={{ display: "flex", gap: 16 }}>
        <aside style={{ flex: "0 0 220px" }}>
          <div
            style={{
              display: "flex",
              justifyContent: "space-between",
              alignItems: "center",
            }}
          >
            <h3 style={{ fontSize: 14 }}>Dashboards</h3>
            <button
              onClick={() => {
                const name = prompt("New dashboard name");
                if (name) createDashboardMut.mutate(name);
              }}
              style={{ fontSize: 12 }}
            >
              + New
            </button>
          </div>
          {dashboardsQuery.isLoading && <p>Loading…</p>}
          <ul style={{ listStyle: "none", padding: 0, margin: 0, fontSize: 13 }}>
            {(dashboardsQuery.data?.dashboards ?? []).map((d) => (
              <li key={d.id} style={{ padding: "4px 0" }}>
                <button
                  onClick={() => setSelectedId(d.id)}
                  style={{
                    background: "none",
                    border: "none",
                    color: selectedId === d.id ? "#111" : "#2563eb",
                    fontWeight: selectedId === d.id ? 600 : 400,
                    cursor: "pointer",
                    padding: 0,
                  }}
                >
                  {d.name}
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: 12 }}>
          {!dashboard && (
            <p style={{ color: "#9ca3af" }}>
              Select or create a dashboard to start adding widgets.
            </p>
          )}

          {dashboard && (
            <>
              <DashboardHeader
                dashboard={dashboard}
                onUpdate={(input) => updateDashboardMut.mutate(input)}
                onDelete={() => {
                  if (confirm(`Delete dashboard "${dashboard.name}"?`)) {
                    deleteDashboardMut.mutate(dashboard.id);
                  }
                }}
                onShare={() => setShareOpen(true)}
              />

              {linkedFilterKeys.length > 0 && (
                <fieldset
                  style={{
                    border: "1px solid #e5e7eb",
                    borderRadius: 6,
                    padding: 8,
                    fontSize: 13,
                  }}
                >
                  <legend style={{ padding: "0 6px", fontWeight: 600 }}>
                    Linked filters
                  </legend>
                  <div style={{ display: "flex", gap: 12, flexWrap: "wrap" }}>
                    {linkedFilterKeys.map((k) => (
                      <label
                        key={k}
                        style={{ display: "flex", gap: 6, alignItems: "center" }}
                      >
                        {k}:
                        <input
                          value={String(linkedFilters[k] ?? "")}
                          onChange={(e) => {
                            const next = {
                              ...linkedFilters,
                              [k]: e.target.value,
                            };
                            setLinkedFilters(next);
                            updateDashboardMut.mutate({ linked_filters: next });
                          }}
                        />
                      </label>
                    ))}
                  </div>
                </fieldset>
              )}

              <WidgetGrid
                widgets={dashboard.widgets ?? []}
                widgetResults={widgetResults}
                queries={queriesQuery.data?.queries ?? []}
                onUpsert={(w) => upsertWidgetMut.mutate(w)}
                onDelete={(w) => deleteWidgetMut.mutate(w)}
                dashboardId={dashboard.id}
              />
            </>
          )}

          {error && (
            <div style={{ color: "#dc2626", fontSize: 13 }}>{error}</div>
          )}
        </div>
      </div>

      {shareOpen && dashboard && (
        <ShareModal
          resource="dashboard"
          resourceId={dashboard.id}
          resourceName={dashboard.name}
          onClose={() => setShareOpen(false)}
        />
      )}
    </section>
  );
}

function DashboardHeader({
  dashboard,
  onUpdate,
  onDelete,
  onShare,
}: {
  dashboard: InsightsDashboard;
  onUpdate: (input: {
    name?: string;
    auto_refresh_seconds?: number;
  }) => void;
  onDelete: () => void;
  onShare: () => void;
}) {
  const [name, setName] = useState(dashboard.name);
  const [autoRefresh, setAutoRefresh] = useState(
    dashboard.auto_refresh_seconds
  );
  useEffect(() => {
    setName(dashboard.name);
    setAutoRefresh(dashboard.auto_refresh_seconds);
  }, [dashboard.id, dashboard.name, dashboard.auto_refresh_seconds]);

  return (
    <div
      style={{
        display: "flex",
        gap: 8,
        alignItems: "center",
      }}
    >
      <input
        value={name}
        onChange={(e) => setName(e.target.value)}
        onBlur={() => {
          if (name !== dashboard.name) onUpdate({ name });
        }}
        style={{ flex: 1, fontSize: 16, fontWeight: 600 }}
      />
      <label style={{ fontSize: 12, display: "flex", alignItems: "center", gap: 4 }}>
        Auto-refresh (s):
        <input
          type="number"
          value={autoRefresh}
          onChange={(e) => setAutoRefresh(Number(e.target.value))}
          onBlur={() => {
            if (autoRefresh !== dashboard.auto_refresh_seconds) {
              onUpdate({ auto_refresh_seconds: autoRefresh });
            }
          }}
          style={{ width: 80 }}
        />
      </label>
      <button onClick={onShare}>Share…</button>
      <button onClick={onDelete} style={{ color: "#dc2626" }}>
        Delete
      </button>
    </div>
  );
}

function WidgetGrid({
  widgets,
  widgetResults,
  queries,
  onUpsert,
  onDelete,
  dashboardId,
}: {
  widgets: InsightsWidget[];
  widgetResults: Record<string, InsightsRunResult | null>;
  queries: InsightsQuery[];
  onUpsert: (w: InsightsWidget) => void;
  onDelete: (w: InsightsWidget) => void;
  dashboardId: string;
}) {
  const addWidget = () => {
    const blank: InsightsWidget = {
      tenant_id: "",
      id: "",
      dashboard_id: dashboardId,
      query_id: null,
      viz_type: "table",
      position: { x: 0, y: 0, w: 6, h: 4 },
      config: {},
      created_at: "",
      updated_at: "",
    };
    onUpsert(blank);
  };

  return (
    <div>
      <div style={{ marginBottom: 8 }}>
        <button onClick={addWidget}>+ Add widget</button>
      </div>
      <div
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(12, 1fr)",
          gap: 12,
          gridAutoRows: "70px",
        }}
      >
        {widgets.map((w) => {
          const pos = w.position ?? {};
          const x = (pos.x ?? 0) + 1;
          const w_ = pos.w ?? 6;
          const h = pos.h ?? 4;
          return (
            <div
              key={w.id}
              style={{
                gridColumn: `${x} / span ${w_}`,
                gridRow: `span ${h}`,
                border: "1px solid #e5e7eb",
                borderRadius: 6,
                padding: 12,
                background: "white",
                overflow: "hidden",
                display: "flex",
                flexDirection: "column",
              }}
            >
              <WidgetView
                widget={w}
                result={widgetResults[w.id] ?? null}
                queries={queries}
                onUpdate={onUpsert}
                onDelete={() => onDelete(w)}
              />
            </div>
          );
        })}
      </div>
    </div>
  );
}

function WidgetView({
  widget,
  result,
  queries,
  onUpdate,
  onDelete,
}: {
  widget: InsightsWidget;
  result: InsightsRunResult | null;
  queries: InsightsQuery[];
  onUpdate: (w: InsightsWidget) => void;
  onDelete: () => void;
}) {
  const [editing, setEditing] = useState(false);
  return (
    <>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          marginBottom: 6,
        }}
      >
        <strong style={{ fontSize: 14 }}>
          {widget.config.title ??
            queries.find((q) => q.id === widget.query_id)?.name ??
            "Untitled widget"}
        </strong>
        <span>
          <button onClick={() => setEditing((v) => !v)}>
            {editing ? "Done" : "Edit"}
          </button>
          <button onClick={onDelete} style={{ color: "#dc2626" }}>
            ✕
          </button>
        </span>
      </div>
      {editing ? (
        <WidgetConfigPanel
          widget={widget}
          queries={queries}
          onSave={(updated) => {
            onUpdate(updated);
            setEditing(false);
          }}
        />
      ) : result ? (
        <div style={{ flex: 1, minHeight: 0 }}>
          <Viz
            vizType={widget.viz_type}
            result={result.result}
            config={widget.config}
            height={undefined}
          />
        </div>
      ) : widget.query_id ? (
        <p style={{ color: "#9ca3af", fontSize: 13 }}>Loading…</p>
      ) : (
        <p style={{ color: "#9ca3af", fontSize: 13 }}>
          Bind this widget to a saved query.
        </p>
      )}
    </>
  );
}

function WidgetConfigPanel({
  widget,
  queries,
  onSave,
}: {
  widget: InsightsWidget;
  queries: InsightsQuery[];
  onSave: (w: InsightsWidget) => void;
}) {
  const [queryId, setQueryId] = useState<string | null>(widget.query_id ?? null);
  const [vizType, setVizType] = useState<InsightsVizType>(widget.viz_type);
  const [config, setConfig] = useState<InsightsWidgetConfig>(widget.config);
  const [position, setPosition] = useState(widget.position);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6, fontSize: 13 }}>
      <label>
        Saved query:
        <select
          value={queryId ?? ""}
          onChange={(e) => setQueryId(e.target.value || null)}
          style={{ width: "100%" }}
        >
          <option value="">— choose —</option>
          {queries.map((q) => (
            <option key={q.id} value={q.id}>
              {q.name}
            </option>
          ))}
        </select>
      </label>
      <label>
        Visualisation:
        <select
          value={vizType}
          onChange={(e) => setVizType(e.target.value as InsightsVizType)}
        >
          {VIZ_OPTIONS.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
      </label>
      <label>
        Title:
        <input
          value={config.title ?? ""}
          onChange={(e) => setConfig({ ...config, title: e.target.value })}
        />
      </label>
      <div style={{ display: "flex", gap: 6 }}>
        <label style={{ flex: 1 }}>
          X:
          <input
            value={config.x_column ?? ""}
            onChange={(e) =>
              setConfig({ ...config, x_column: e.target.value })
            }
          />
        </label>
        <label style={{ flex: 1 }}>
          Y / value:
          <input
            value={config.y_column ?? config.value_column ?? ""}
            onChange={(e) =>
              setConfig({ ...config, y_column: e.target.value })
            }
          />
        </label>
      </div>
      <div style={{ display: "flex", gap: 6 }}>
        <label style={{ flex: 1 }}>
          Linked filter key:
          <input
            value={config.linked_filter_key ?? ""}
            onChange={(e) =>
              setConfig({
                ...config,
                linked_filter_key: e.target.value || undefined,
              })
            }
          />
        </label>
        <label style={{ flex: 1 }}>
          → column:
          <input
            value={config.linked_filter_column ?? ""}
            onChange={(e) =>
              setConfig({
                ...config,
                linked_filter_column: e.target.value || undefined,
              })
            }
          />
        </label>
      </div>
      <div style={{ display: "flex", gap: 6 }}>
        <label>
          x:
          <input
            type="number"
            value={position.x ?? 0}
            onChange={(e) =>
              setPosition({ ...position, x: Number(e.target.value) })
            }
            style={{ width: 60 }}
          />
        </label>
        <label>
          w:
          <input
            type="number"
            value={position.w ?? 6}
            onChange={(e) =>
              setPosition({ ...position, w: Number(e.target.value) })
            }
            style={{ width: 60 }}
          />
        </label>
        <label>
          h:
          <input
            type="number"
            value={position.h ?? 4}
            onChange={(e) =>
              setPosition({ ...position, h: Number(e.target.value) })
            }
            style={{ width: 60 }}
          />
        </label>
      </div>
      <button
        onClick={() =>
          onSave({
            ...widget,
            query_id: queryId,
            viz_type: vizType,
            config,
            position,
          })
        }
      >
        Save widget
      </button>
    </div>
  );
}
