import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import type {
  InsightsDashboardBundle,
  InsightsDashboardWidget,
  InsightsQuery,
  InsightsRunResult,
  InsightsShare,
  InsightsVizType,
} from "@kapp/client";
import { api } from "../lib/api";
import {
  InsightsWidgetChart,
  type WidgetChartConfig,
} from "../components/InsightsWidgetChart";

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

const GRID_COLS = 12;

// Widget position shape — {x, y, w, h} on a 12-column grid so the
// layout reads cleanly for humans editing the JSONB directly and
// matches CSS grid semantics. Missing values fall back to sensible
// defaults at render time.
interface WidgetPosition {
  x?: number;
  y?: number;
  w?: number;
  h?: number;
}

/**
 * InsightsDashboardPage renders a saved dashboard as a grid of widgets
 * and lets the owner edit name, auto-refresh, widgets and share grants
 * in-place. The page uses the bundled GET /insights/dashboards/{id}
 * response so each widget's query is cache-resolved in a single
 * round-trip.
 */
export function InsightsDashboardPage() {
  const { id } = useParams<{ id: string }>();
  const qc = useQueryClient();
  const nav = useNavigate();

  const [isEditing, setIsEditing] = useState(false);
  const [sharesOpen, setSharesOpen] = useState(false);

  const queries = useQuery<{ queries: InsightsQuery[] }>({
    queryKey: ["insights", "queries"],
    queryFn: () => api.listInsightsQueries(),
  });

  const bundleQuery = useQuery<InsightsDashboardBundle>({
    queryKey: ["insights", "dashboard", id],
    queryFn: () => api.getInsightsDashboard(id!),
    enabled: !!id,
  });

  const autoRefresh = bundleQuery.data?.dashboard.auto_refresh_seconds ?? 0;
  useEffect(() => {
    if (!autoRefresh || autoRefresh <= 0) return;
    const t = setInterval(() => {
      qc.invalidateQueries({ queryKey: ["insights", "dashboard", id] });
    }, autoRefresh * 1000);
    return () => clearInterval(t);
  }, [autoRefresh, id, qc]);

  const bundle = bundleQuery.data;
  const dash = bundle?.dashboard;
  const widgets = dash?.widgets ?? [];
  const widgetResults = bundle?.widget_results ?? {};

  // Linked-filter widgets (viz_type = filter) are selected client-side
  // from the widget list — when active they push filter_params into
  // the other widgets' run calls. The backend is oblivious to the
  // concept; the frontend owns it.
  const [activeFilters, setActiveFilters] = useState<Record<string, unknown>>({});
  const filterWidgets = widgets.filter(
    (w) => typeof (w.config as Record<string, unknown> | null)?.filter_key === "string"
  );

  if (!id) return null;
  if (bundleQuery.isLoading) return <p>Loading…</p>;
  if (bundleQuery.error || !dash) {
    return (
      <p style={{ color: "#b91c1c" }}>
        Failed to load dashboard: {(bundleQuery.error as Error)?.message ?? "not found"}
      </p>
    );
  }

  return (
    <section>
      <DashboardHeader
        dashboard={dash}
        onBack={() => nav("/insights/dashboards")}
        onToggleEdit={() => setIsEditing((v) => !v)}
        onToggleShares={() => setSharesOpen((v) => !v)}
        isEditing={isEditing}
      />
      {sharesOpen && <SharesPanel dashboardId={id} />}

      {filterWidgets.length > 0 && (
        <FilterBar
          widgets={filterWidgets}
          values={activeFilters}
          onChange={setActiveFilters}
        />
      )}

      <WidgetGrid
        widgets={widgets}
        widgetResults={widgetResults}
        activeFilters={activeFilters}
        isEditing={isEditing}
        dashboardId={id}
      />

      {isEditing && (
        <AddWidgetCard
          dashboardId={id}
          queries={queries.data?.queries ?? []}
          nextY={nextRow(widgets)}
        />
      )}
    </section>
  );
}

// ---------- Header ----------

function DashboardHeader({
  dashboard,
  isEditing,
  onBack,
  onToggleEdit,
  onToggleShares,
}: {
  dashboard: InsightsDashboardBundle["dashboard"];
  isEditing: boolean;
  onBack: () => void;
  onToggleEdit: () => void;
  onToggleShares: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(dashboard.name);
  const [description, setDescription] = useState(dashboard.description ?? "");
  const [autoRefresh, setAutoRefresh] = useState(dashboard.auto_refresh_seconds ?? 0);

  useEffect(() => {
    setName(dashboard.name);
    setDescription(dashboard.description ?? "");
    setAutoRefresh(dashboard.auto_refresh_seconds ?? 0);
  }, [dashboard]);

  const save = useMutation({
    mutationFn: () =>
      api.updateInsightsDashboard(dashboard.id, {
        name,
        description,
        layout: dashboard.layout,
        auto_refresh_seconds: autoRefresh,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["insights", "dashboards"] });
      qc.invalidateQueries({
        queryKey: ["insights", "dashboard", dashboard.id],
      });
    },
  });

  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        justifyContent: "space-between",
        gap: 8,
        marginBottom: 12,
      }}
    >
      <div style={{ display: "flex", alignItems: "center", gap: 8, flex: 1 }}>
        <button onClick={onBack}>←</button>
        {isEditing ? (
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            style={{
              fontSize: 20,
              fontWeight: 600,
              padding: "2px 6px",
              border: "1px solid #d1d5db",
              borderRadius: 4,
              flex: 1,
            }}
          />
        ) : (
          <h1 style={{ margin: 0 }}>{dashboard.name}</h1>
        )}
      </div>
      {isEditing && (
        <>
          <input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="description"
            style={{
              padding: "4px 6px",
              border: "1px solid #d1d5db",
              borderRadius: 4,
            }}
          />
          <label style={{ fontSize: 12, color: "#374151" }}>
            Auto-refresh&nbsp;(s)
            <input
              type="number"
              min={0}
              value={autoRefresh}
              onChange={(e) => setAutoRefresh(Math.max(0, Number(e.target.value)))}
              style={{
                width: 80,
                marginLeft: 4,
                padding: "2px 4px",
                border: "1px solid #d1d5db",
                borderRadius: 4,
              }}
            />
          </label>
          <button onClick={() => save.mutate()} disabled={save.isPending}>
            Save dashboard
          </button>
        </>
      )}
      <button onClick={onToggleShares}>Share</button>
      <button onClick={onToggleEdit}>{isEditing ? "Done" : "Edit"}</button>
    </div>
  );
}

// ---------- Filter bar ----------

function FilterBar({
  widgets,
  values,
  onChange,
}: {
  widgets: InsightsDashboardWidget[];
  values: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
}) {
  return (
    <div
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 6,
        padding: 8,
        marginBottom: 12,
        display: "flex",
        gap: 8,
        flexWrap: "wrap",
        alignItems: "center",
      }}
    >
      <strong style={{ fontSize: 13 }}>Filters</strong>
      {widgets.map((w) => {
        const config = (w.config as Record<string, unknown> | null) ?? {};
        const key = config.filter_key as string;
        const label = (config.label as string) ?? key;
        return (
          <label key={w.id} style={{ fontSize: 13 }}>
            {label}:&nbsp;
            <input
              value={String(values[key] ?? "")}
              onChange={(e) => {
                // Drop the key entirely when the input is cleared so
                // `Object.keys(activeFilters).length` is an accurate
                // trigger for the sibling widgets' live-run effect —
                // otherwise clearing a filter would leave a stale
                // `{ region: undefined }` entry, pass the length guard,
                // and fire a spurious POST /run per widget even though
                // JSON.stringify drops the undefined in the payload.
                const next = { ...values };
                const v = e.target.value;
                if (v) next[key] = v;
                else delete next[key];
                onChange(next);
              }}
              placeholder="(all)"
              style={{
                padding: "2px 4px",
                border: "1px solid #d1d5db",
                borderRadius: 4,
              }}
            />
          </label>
        );
      })}
    </div>
  );
}

// ---------- Widget grid ----------

function WidgetGrid({
  widgets,
  widgetResults,
  activeFilters,
  isEditing,
  dashboardId,
}: {
  widgets: InsightsDashboardWidget[];
  widgetResults: Record<string, InsightsRunResult | null>;
  activeFilters: Record<string, unknown>;
  isEditing: boolean;
  dashboardId: string;
}) {
  const sorted = useMemo(() => {
    return [...widgets].sort((a, b) => {
      const pa = (a.position as WidgetPosition) ?? {};
      const pb = (b.position as WidgetPosition) ?? {};
      const ya = pa.y ?? 0;
      const yb = pb.y ?? 0;
      if (ya !== yb) return ya - yb;
      return (pa.x ?? 0) - (pb.x ?? 0);
    });
  }, [widgets]);

  return (
    <div
      style={{
        display: "grid",
        gridTemplateColumns: `repeat(${GRID_COLS}, 1fr)`,
        gap: 12,
      }}
    >
      {sorted.map((w) => {
        const pos = (w.position as WidgetPosition) ?? {};
        const w12 = Math.max(1, Math.min(GRID_COLS, pos.w ?? 6));
        const h = Math.max(1, pos.h ?? 1);
        return (
          <div
            key={w.id}
            style={{
              gridColumn: `span ${w12}`,
              border: "1px solid #e5e7eb",
              borderRadius: 6,
              padding: 12,
              minHeight: 120 * h,
            }}
          >
            <WidgetCard
              widget={w}
              result={widgetResults[w.id] ?? null}
              activeFilters={activeFilters}
              isEditing={isEditing}
              dashboardId={dashboardId}
            />
          </div>
        );
      })}
    </div>
  );
}

// ---------- Single widget card ----------

function WidgetCard({
  widget,
  result,
  activeFilters,
  isEditing,
  dashboardId,
}: {
  widget: InsightsDashboardWidget;
  result: InsightsRunResult | null;
  activeFilters: Record<string, unknown>;
  isEditing: boolean;
  dashboardId: string;
}) {
  const qc = useQueryClient();
  const queries = useQuery<{ queries: InsightsQuery[] }>({
    queryKey: ["insights", "queries"],
    queryFn: () => api.listInsightsQueries(),
  });

  const config = (widget.config as Record<string, unknown> | null) ?? {};
  const title = (config.title as string) ?? vizTitle(widget, queries.data?.queries ?? []);
  const isFilterWidget = typeof config.filter_key === "string";

  // When linked filters are active, re-run the widget's saved query
  // with filter_params so the chart updates live instead of waiting
  // for the next dashboard poll. We cache by widget id + active
  // filters so repeated edits don't thrash.
  const filterKey = JSON.stringify(activeFilters);
  const live = useQuery<InsightsRunResult | null>({
    queryKey: ["insights", "widget-live", widget.id, filterKey],
    queryFn: async () => {
      if (!widget.query_id) return null;
      if (Object.keys(activeFilters).length === 0) return null;
      return api.runInsightsQuery(widget.query_id, { filter_params: activeFilters });
    },
    enabled: !!widget.query_id && Object.keys(activeFilters).length > 0 && !isFilterWidget,
  });

  const del = useMutation({
    mutationFn: () => api.deleteInsightsWidget(dashboardId, widget.id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["insights", "dashboard", dashboardId] }),
  });

  return (
    <>
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "baseline",
          marginBottom: 4,
        }}
      >
        <div style={{ fontSize: 13, fontWeight: 600 }}>{title}</div>
        {isEditing && (
          <div style={{ display: "flex", gap: 4 }}>
            <WidgetEditMenu widget={widget} dashboardId={dashboardId} />
            <button onClick={() => del.mutate()} style={{ color: "#b91c1c" }}>
              Remove
            </button>
          </div>
        )}
      </div>
      {isFilterWidget ? (
        <div style={{ color: "#6b7280", fontSize: 12 }}>
          Filter widget — value lives in the toolbar above.
        </div>
      ) : (
        <InsightsWidgetChart
          viz={widget.viz_type}
          result={live.data ?? result}
          config={config as WidgetChartConfig}
          emptyText={widget.query_id ? "Run failed" : "No query bound to this widget"}
        />
      )}
    </>
  );
}

// ---------- Widget edit menu ----------

function WidgetEditMenu({
  widget,
  dashboardId,
}: {
  widget: InsightsDashboardWidget;
  dashboardId: string;
}) {
  const qc = useQueryClient();
  const queries = useQuery<{ queries: InsightsQuery[] }>({
    queryKey: ["insights", "queries"],
    queryFn: () => api.listInsightsQueries(),
  });

  const [open, setOpen] = useState(false);
  const [viz, setViz] = useState<string>(widget.viz_type);
  const [queryId, setQueryId] = useState<string>(widget.query_id ?? "");
  const pos = (widget.position as WidgetPosition) ?? {};
  const [w, setW] = useState<number>(pos.w ?? 6);
  const [h, setH] = useState<number>(pos.h ?? 1);
  const existingConfig = (widget.config as Record<string, unknown> | null) ?? {};
  const [title, setTitle] = useState<string>((existingConfig.title as string) ?? "");
  const [xColumn, setXColumn] = useState<string>(
    (existingConfig.x_column as string) ?? ""
  );
  const [yColumn, setYColumn] = useState<string>(
    (existingConfig.y_column as string) ?? ""
  );
  const [filterKey, setFilterKey] = useState<string>(
    (existingConfig.filter_key as string) ?? ""
  );

  const save = useMutation({
    mutationFn: () => {
      const cfg: Record<string, unknown> = { ...existingConfig };
      if (title) cfg.title = title;
      else delete cfg.title;
      if (xColumn) cfg.x_column = xColumn;
      else delete cfg.x_column;
      if (yColumn) cfg.y_column = yColumn;
      else delete cfg.y_column;
      if (filterKey) cfg.filter_key = filterKey;
      else delete cfg.filter_key;
      return api.upsertInsightsWidget(dashboardId, {
        id: widget.id,
        query_id: queryId || null,
        viz_type: viz,
        position: { ...pos, w, h },
        config: cfg,
      });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["insights", "dashboard", dashboardId] });
      setOpen(false);
    },
  });

  return (
    <div style={{ position: "relative" }}>
      <button onClick={() => setOpen((v) => !v)} style={{ fontSize: 12 }}>
        Edit
      </button>
      {open && (
        <div
          style={{
            position: "absolute",
            right: 0,
            top: 24,
            zIndex: 10,
            background: "#fff",
            border: "1px solid #e5e7eb",
            borderRadius: 6,
            padding: 12,
            width: 280,
            boxShadow: "0 4px 12px rgba(0,0,0,0.08)",
            fontSize: 12,
          }}
        >
          <div style={{ fontWeight: 600, marginBottom: 6 }}>Widget</div>
          <label style={fieldLabel}>
            Title
            <input
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              style={fieldInput}
            />
          </label>
          <label style={fieldLabel}>
            Query
            <select
              value={queryId}
              onChange={(e) => setQueryId(e.target.value)}
              style={fieldInput}
            >
              <option value="">(none — filter widget)</option>
              {(queries.data?.queries ?? []).map((q) => (
                <option key={q.id} value={q.id}>
                  {q.name}
                </option>
              ))}
            </select>
          </label>
          <label style={fieldLabel}>
            Viz
            <select
              value={viz}
              onChange={(e) => setViz(e.target.value)}
              style={fieldInput}
            >
              {VIZ_OPTIONS.map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </select>
          </label>
          <div style={{ display: "flex", gap: 6 }}>
            <label style={fieldLabel}>
              X column
              <input
                value={xColumn}
                onChange={(e) => setXColumn(e.target.value)}
                style={fieldInput}
              />
            </label>
            <label style={fieldLabel}>
              Y column
              <input
                value={yColumn}
                onChange={(e) => setYColumn(e.target.value)}
                style={fieldInput}
              />
            </label>
          </div>
          <label style={fieldLabel}>
            Filter key (turns this into a filter widget)
            <input
              value={filterKey}
              onChange={(e) => setFilterKey(e.target.value)}
              style={fieldInput}
              placeholder="e.g. region"
            />
          </label>
          <div style={{ display: "flex", gap: 6 }}>
            <label style={fieldLabel}>
              Width (1–12)
              <input
                type="number"
                min={1}
                max={GRID_COLS}
                value={w}
                onChange={(e) =>
                  setW(Math.max(1, Math.min(GRID_COLS, Number(e.target.value))))
                }
                style={fieldInput}
              />
            </label>
            <label style={fieldLabel}>
              Height
              <input
                type="number"
                min={1}
                value={h}
                onChange={(e) => setH(Math.max(1, Number(e.target.value)))}
                style={fieldInput}
              />
            </label>
          </div>
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 6, marginTop: 8 }}>
            <button onClick={() => setOpen(false)} style={{ fontSize: 12 }}>
              Cancel
            </button>
            <button onClick={() => save.mutate()} disabled={save.isPending}>
              Save
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

// ---------- Add widget ----------

function AddWidgetCard({
  dashboardId,
  queries,
  nextY,
}: {
  dashboardId: string;
  queries: InsightsQuery[];
  nextY: number;
}) {
  const qc = useQueryClient();
  const [queryId, setQueryId] = useState("");
  const [viz, setViz] = useState<string>("bar");
  const [title, setTitle] = useState("");

  const add = useMutation({
    mutationFn: () =>
      api.upsertInsightsWidget(dashboardId, {
        query_id: queryId || null,
        viz_type: viz,
        position: { x: 0, y: nextY, w: 6, h: 1 },
        config: title ? { title } : {},
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["insights", "dashboard", dashboardId] });
      setQueryId("");
      setTitle("");
    },
  });

  return (
    <div
      style={{
        border: "1px dashed #d1d5db",
        borderRadius: 6,
        padding: 12,
        marginTop: 12,
        display: "flex",
        gap: 6,
        flexWrap: "wrap",
        alignItems: "center",
      }}
    >
      <strong style={{ fontSize: 13 }}>+ Add widget</strong>
      <select value={viz} onChange={(e) => setViz(e.target.value)}>
        {VIZ_OPTIONS.map((v) => (
          <option key={v} value={v}>
            {v}
          </option>
        ))}
      </select>
      <select value={queryId} onChange={(e) => setQueryId(e.target.value)}>
        <option value="">(none — filter widget)</option>
        {queries.map((q) => (
          <option key={q.id} value={q.id}>
            {q.name}
          </option>
        ))}
      </select>
      <input
        value={title}
        onChange={(e) => setTitle(e.target.value)}
        placeholder="widget title (optional)"
        style={{
          padding: "2px 4px",
          border: "1px solid #d1d5db",
          borderRadius: 4,
        }}
      />
      <button onClick={() => add.mutate()}>Add</button>
    </div>
  );
}

// ---------- Shares panel ----------

function SharesPanel({ dashboardId }: { dashboardId: string }) {
  const qc = useQueryClient();
  const shares = useQuery<{ shares: InsightsShare[] }>({
    queryKey: ["insights", "dashboard-shares", dashboardId],
    queryFn: () => api.listInsightsDashboardShares(dashboardId),
  });
  const [granteeType, setGranteeType] = useState("user");
  const [grantee, setGrantee] = useState("");
  const [permission, setPermission] = useState("view");
  const create = useMutation({
    mutationFn: () =>
      api.createInsightsDashboardShare(dashboardId, {
        grantee_type: granteeType,
        grantee,
        permission,
      }),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: ["insights", "dashboard-shares", dashboardId],
      });
      setGrantee("");
    },
  });
  return (
    <div
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 6,
        padding: 12,
        marginBottom: 12,
        background: "#f9fafb",
      }}
    >
      <strong>Shares</strong>
      {(shares.data?.shares ?? []).length === 0 && (
        <div style={{ color: "#9ca3af", fontStyle: "italic", fontSize: 13 }}>
          Not shared.
        </div>
      )}
      <ul style={{ listStyle: "none", padding: 0, margin: "6px 0", fontSize: 13 }}>
        {(shares.data?.shares ?? []).map((s) => (
          <li key={s.id} style={{ padding: "2px 0" }}>
            <strong>{s.grantee_type}</strong>: {s.grantee} — {s.permission}
          </li>
        ))}
      </ul>
      <div style={{ display: "flex", gap: 4, marginTop: 4 }}>
        <select value={granteeType} onChange={(e) => setGranteeType(e.target.value)}>
          <option value="user">user</option>
          <option value="role">role</option>
        </select>
        <input
          value={grantee}
          onChange={(e) => setGrantee(e.target.value)}
          placeholder="user id or role name"
          style={{
            flex: 1,
            padding: "2px 4px",
            border: "1px solid #d1d5db",
            borderRadius: 4,
          }}
        />
        <select value={permission} onChange={(e) => setPermission(e.target.value)}>
          <option value="view">view</option>
          <option value="edit">edit</option>
        </select>
        <button onClick={() => create.mutate()} disabled={!grantee.trim()}>
          Add
        </button>
      </div>
    </div>
  );
}

// ---------- utilities ----------

function nextRow(widgets: InsightsDashboardWidget[]): number {
  let max = 0;
  for (const w of widgets) {
    const p = (w.position as WidgetPosition) ?? {};
    const bottom = (p.y ?? 0) + (p.h ?? 1);
    if (bottom > max) max = bottom;
  }
  return max;
}

function vizTitle(
  w: InsightsDashboardWidget,
  queries: InsightsQuery[]
): string {
  if (w.query_id) {
    const q = queries.find((x) => x.id === w.query_id);
    if (q) return q.name;
  }
  return `${w.viz_type} widget`;
}

const fieldLabel: React.CSSProperties = {
  display: "block",
  fontSize: 12,
  color: "#374151",
  marginTop: 4,
  flex: 1,
};
const fieldInput: React.CSSProperties = {
  width: "100%",
  padding: "2px 4px",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 12,
};
