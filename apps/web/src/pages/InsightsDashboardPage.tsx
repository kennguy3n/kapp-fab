// Insights — dashboard builder.
//
// Picks an existing dashboard (or creates a new one) and renders its
// widgets in a 12-column CSS grid of chart cards. Each widget binds to
// a saved insights query and selects a viz_type; the per-widget run
// result arrives bundled with the dashboard payload so the page renders
// without a per-widget fan-out. Cards can be dragged to reorder — the
// drop swaps the two cards' grid positions and persists both through
// the widget upsert endpoint (no extra layout library). Linked filters
// live in the dashboard `layout` blob — picking a value on one widget
// re-runs every widget whose config maps the same `linked_filter_key`.

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
import {
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  PromptDialog,
  Select,
  Skeleton,
  cn,
} from "@kapp/ui";
import { BarChart3, GripVertical, LayoutDashboard, Plus } from "lucide-react";
import { api } from "../lib/api";
import { humanizeLabel } from "../lib/ktypeView";
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

// Plain-language label for each viz type (never surface the raw token).
const VIZ_LABELS: Record<InsightsVizType, string> = {
  table: "Table",
  bar: "Bar chart",
  line: "Line chart",
  pie: "Pie chart",
  donut: "Donut chart",
  funnel: "Funnel",
  number_card: "Single number",
  pivot: "Pivot table",
};

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
  const [newOpen, setNewOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
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
    // Await-mutation pattern (mirrors the delete flow): keep the
    // prompt open showing "Working…" until the create settles, then
    // close regardless of outcome (errors surface in the banner).
    onSettled: () => setNewOpen(false),
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
    // Await-mutation pattern: the dialog stays open showing "Working…"
    // until the delete settles, then closes regardless of outcome
    // (an error surfaces in the page-level error banner).
    onSettled: () => setDeleteOpen(false),
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
    <section className="flex flex-col gap-5">
      <header>
        <Eyebrow>Insights</Eyebrow>
        <h1 className="mt-1 text-2xl font-semibold tracking-tight text-fg">
          Dashboards
        </h1>
        <p className="mt-1 max-w-prose text-sm text-fg-muted">
          Pin your saved queries as charts, arrange them by dragging, and share
          a live view with your team.
        </p>
      </header>

      <div className="flex flex-col gap-5 lg:flex-row">
        <aside className="lg:flex-[0_0_240px]">
          <div className="flex items-center justify-between gap-2">
            <h2 className="text-sm font-semibold text-fg">Dashboards</h2>
            <Button size="sm" variant="ghost" onClick={() => setNewOpen(true)}>
              + New
            </Button>
          </div>
          {dashboardsQuery.isLoading ? (
            <div className="mt-2 flex flex-col gap-1.5" aria-hidden>
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-4/5" />
            </div>
          ) : dashboardsQuery.isError ? (
            <div className="mt-2 rounded-lg border border-border p-3 text-sm">
              <p className="text-fg-muted">Couldn’t load your dashboards.</p>
              <Button
                size="sm"
                variant="outline"
                className="mt-2"
                onClick={() => dashboardsQuery.refetch()}
              >
                Try again
              </Button>
            </div>
          ) : (dashboardsQuery.data?.dashboards ?? []).length === 0 ? (
            <p className="mt-2 text-sm text-fg-muted">No dashboards yet.</p>
          ) : (
            <ul className="mt-2 flex list-none flex-col gap-0.5 p-0">
              {(dashboardsQuery.data?.dashboards ?? []).map((d) => (
                <li key={d.id}>
                  <button
                    onClick={() => setSelectedId(d.id)}
                    className={cn(
                      "w-full cursor-pointer truncate rounded-md px-2.5 py-1.5 text-left text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                      selectedId === d.id
                        ? "bg-bg-muted font-medium text-fg"
                        : "text-fg-muted hover:bg-bg-subtle hover:text-fg",
                    )}
                  >
                    {d.name}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </aside>

        <div className="flex min-w-0 flex-1 flex-col gap-4">
          {!selectedId && (
            <EmptyState
              icon={<LayoutDashboard className="h-6 w-6" aria-hidden />}
              title="No dashboard selected"
              description="Select or create a dashboard to start adding widgets."
              action={
                <Button onClick={() => setNewOpen(true)}>
                  <Plus className="h-4 w-4" aria-hidden /> New dashboard
                </Button>
              }
            />
          )}

          {selectedId && bundleQuery.isLoading && (
            <div className="flex flex-col gap-4">
              <Skeleton className="h-10 w-1/2" />
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <Skeleton className="h-56 w-full" />
                <Skeleton className="h-56 w-full" />
              </div>
            </div>
          )}

          {selectedId && bundleQuery.isError && (
            <div className="rounded-lg border border-border p-6 text-center">
              <p className="text-sm text-fg-muted">
                We couldn’t load this dashboard.
              </p>
              <Button
                variant="outline"
                className="mt-3"
                onClick={() => bundleQuery.refetch()}
              >
                Try again
              </Button>
            </div>
          )}

          {dashboard && (
            <>
              <DashboardHeader
                dashboard={dashboard}
                onUpdate={(input) => updateDashboardMut.mutate(input)}
                onDelete={() => setDeleteOpen(true)}
                onShare={() => setShareOpen(true)}
              />

              {linkedFilterKeys.length > 0 && (
                <div className="rounded-lg border border-border bg-bg-elevated p-3">
                  <h3 className="mb-2 text-sm font-semibold text-fg">
                    Filters
                  </h3>
                  <div className="flex flex-wrap gap-3">
                    {linkedFilterKeys.map((k) => (
                      <Field key={k} label={humanizeLabel(k)} className="w-48">
                        <Input
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
                      </Field>
                    ))}
                  </div>
                </div>
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
            <div
              role="alert"
              className="rounded-md border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-danger"
            >
              {error}
            </div>
          )}
        </div>
      </div>

      <PromptDialog
        open={newOpen}
        onOpenChange={(open) => {
          if (!open && createDashboardMut.isPending) return;
          setNewOpen(open);
        }}
        title="New dashboard"
        label="Dashboard name"
        placeholder="e.g. Sales overview"
        loading={createDashboardMut.isPending}
        onSubmit={(name) => {
          const trimmed = name.trim();
          if (trimmed) createDashboardMut.mutate(trimmed);
        }}
      />

      {dashboard && (
        <ConfirmDialog
          open={deleteOpen}
          onOpenChange={(open) => {
            if (!open && deleteDashboardMut.isPending) return;
            setDeleteOpen(open);
          }}
          title={`Delete dashboard "${dashboard.name}"?`}
          description="This permanently removes the dashboard and its widgets."
          destructive
          loading={deleteDashboardMut.isPending}
          onConfirm={() => deleteDashboardMut.mutate(dashboard.id)}
        />
      )}

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
    <div className="flex flex-wrap items-end justify-between gap-3 border-b border-border pb-4">
      <Field label="Dashboard name" hideLabel className="min-w-0 flex-1">
        <Input
          aria-label="Dashboard name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          onBlur={() => {
            if (name !== dashboard.name) onUpdate({ name });
          }}
          className="text-base font-semibold"
        />
      </Field>
      <div className="flex flex-wrap items-end gap-2">
        <Field
          label="Refresh every"
          help="Seconds — 0 turns auto-refresh off."
          className="w-36"
        >
          <Input
            type="number"
            min={0}
            aria-label="Auto-refresh seconds"
            value={autoRefresh}
            onChange={(e) => setAutoRefresh(Number(e.target.value))}
            onBlur={() => {
              if (autoRefresh !== dashboard.auto_refresh_seconds) {
                onUpdate({ auto_refresh_seconds: autoRefresh });
              }
            }}
          />
        </Field>
        <Button variant="outline" onClick={onShare}>
          Share
        </Button>
        <Button
          variant="ghost"
          className="text-danger hover:text-danger"
          onClick={onDelete}
        >
          Delete
        </Button>
      </div>
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
  const [dragId, setDragId] = useState<string | null>(null);

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

  // Drag-to-reorder: dropping card A onto card B swaps their grid
  // positions and persists both. Swapping (rather than free placement)
  // keeps the 12-column grid gap-free without a layout library.
  const onDropOnto = (target: InsightsWidget) => {
    if (!dragId || dragId === target.id) return;
    const dragged = widgets.find((w) => w.id === dragId);
    if (!dragged) return;
    onUpsert({ ...dragged, position: target.position });
    onUpsert({ ...target, position: dragged.position });
    setDragId(null);
  };

  if (widgets.length === 0) {
    return (
      <EmptyState
        icon={<BarChart3 className="h-6 w-6" aria-hidden />}
        title="No charts yet"
        description="Add your first chart, then bind it to a saved query to bring this dashboard to life."
        action={
          <Button onClick={addWidget}>
            <Plus className="h-4 w-4" aria-hidden /> Add chart
          </Button>
        }
      />
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <div>
        <Button size="sm" variant="outline" onClick={addWidget}>
          <Plus className="h-4 w-4" aria-hidden /> Add chart
        </Button>
      </div>
      <div className="grid auto-rows-[72px] grid-cols-12 gap-3">
        {widgets.map((w) => {
          const pos = w.position ?? {};
          const x = (pos.x ?? 0) + 1;
          const w_ = pos.w ?? 6;
          const h = pos.h ?? 4;
          return (
            <div
              key={w.id}
              onDragOver={(e) => {
                if (dragId) e.preventDefault();
              }}
              onDrop={(e) => {
                e.preventDefault();
                onDropOnto(w);
              }}
              className={cn(
                "flex flex-col overflow-hidden rounded-lg border border-border bg-bg-elevated p-3 transition-shadow",
                dragId && dragId !== w.id && "ring-1 ring-border",
              )}
              style={{
                gridColumn: `${x} / span ${w_}`,
                gridRow: `span ${h}`,
              }}
            >
              <WidgetView
                widget={w}
                result={widgetResults[w.id] ?? null}
                queries={queries}
                onUpdate={onUpsert}
                onDelete={() => onDelete(w)}
                onDragStart={() => setDragId(w.id)}
                onDragEnd={() => setDragId(null)}
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
  onDragStart,
  onDragEnd,
}: {
  widget: InsightsWidget;
  result: InsightsRunResult | null;
  queries: InsightsQuery[];
  onUpdate: (w: InsightsWidget) => void;
  onDelete: () => void;
  onDragStart: () => void;
  onDragEnd: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const title =
    widget.config.title ??
    queries.find((q) => q.id === widget.query_id)?.name ??
    "Untitled widget";
  return (
    <>
      <div className="mb-1.5 flex items-center justify-between gap-1">
        <div
          className="flex min-w-0 items-center gap-1.5"
          draggable={!editing}
          onDragStart={(e) => {
            e.dataTransfer.effectAllowed = "move";
            onDragStart();
          }}
          onDragEnd={onDragEnd}
        >
          {!editing && (
            <GripVertical
              className="h-4 w-4 shrink-0 cursor-grab text-fg-subtle"
              aria-hidden
            />
          )}
          <strong className="truncate text-sm text-fg">{title}</strong>
        </div>
        <span className="flex shrink-0 items-center gap-1">
          {!editing && (
            <Badge variant="neutral" size="xs">
              {VIZ_LABELS[widget.viz_type]}
            </Badge>
          )}
          <Button
            size="sm"
            variant="ghost"
            onClick={() => setEditing((v) => !v)}
          >
            {editing ? "Done" : "Edit"}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            className="text-danger hover:text-danger"
            aria-label="Delete widget"
            onClick={onDelete}
          >
            ✕
          </Button>
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
        <div className="min-h-0 flex-1">
          <Viz
            vizType={widget.viz_type}
            result={result.result}
            config={widget.config}
            height={undefined}
          />
        </div>
      ) : widget.query_id ? (
        <Skeleton className="h-full min-h-24 w-full" />
      ) : (
        <p className="flex-1 text-sm text-fg-subtle">
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
    <div className="flex flex-col gap-3 overflow-auto text-sm">
      <Field label="Saved query">
        <Select
          value={queryId ?? ""}
          onChange={(e) => setQueryId(e.target.value || null)}
        >
          <option value="">Select a saved query…</option>
          {queries.map((q) => (
            <option key={q.id} value={q.id}>
              {q.name}
            </option>
          ))}
        </Select>
      </Field>
      <Field label="Chart type">
        <Select
          value={vizType}
          onChange={(e) => setVizType(e.target.value as InsightsVizType)}
        >
          {VIZ_OPTIONS.map((v) => (
            <option key={v} value={v}>
              {VIZ_LABELS[v]}
            </option>
          ))}
        </Select>
      </Field>
      <Field label="Title" help="Optional — defaults to the query name.">
        <Input
          value={config.title ?? ""}
          onChange={(e) => setConfig({ ...config, title: e.target.value })}
        />
      </Field>
      <div className="grid grid-cols-2 gap-2">
        <Field label="Category (X) field">
          <Input
            value={config.x_column ?? ""}
            onChange={(e) => setConfig({ ...config, x_column: e.target.value })}
          />
        </Field>
        <Field label="Value (Y) field">
          <Input
            value={config.y_column ?? config.value_column ?? ""}
            onChange={(e) => setConfig({ ...config, y_column: e.target.value })}
          />
        </Field>
      </div>
      <div className="grid grid-cols-2 gap-2">
        <Field
          label="Filter key"
          help="Connect this chart to a dashboard filter."
        >
          <Input
            value={config.linked_filter_key ?? ""}
            onChange={(e) =>
              setConfig({
                ...config,
                linked_filter_key: e.target.value || undefined,
              })
            }
          />
        </Field>
        <Field label="Applies to field">
          <Input
            value={config.linked_filter_column ?? ""}
            onChange={(e) =>
              setConfig({
                ...config,
                linked_filter_column: e.target.value || undefined,
              })
            }
          />
        </Field>
      </div>
      <div className="grid grid-cols-2 gap-2">
        <Field label="Width" help="Out of 12 columns.">
          <Select
            value={String(position.w ?? 6)}
            onChange={(e) =>
              setPosition({ ...position, w: Number(e.target.value) })
            }
          >
            {[3, 4, 6, 8, 12].map((n) => (
              <option key={n} value={n}>
                {n === 12 ? "Full width" : `${n} / 12`}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Height" help="Grid rows.">
          <Select
            value={String(position.h ?? 4)}
            onChange={(e) =>
              setPosition({ ...position, h: Number(e.target.value) })
            }
          >
            {[3, 4, 6, 8].map((n) => (
              <option key={n} value={n}>
                {n} rows
              </option>
            ))}
          </Select>
        </Field>
      </div>
      <Button
        size="sm"
        className="self-start"
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
      </Button>
    </div>
  );
}
