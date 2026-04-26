import {
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  FunnelChart,
  Funnel,
  LabelList,
  Legend,
  Line,
  LineChart,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type {
  InsightsRunResult,
  InsightsVizType,
  ReportResult,
} from "@kapp/client";

// Pull palette + viz-specific config off the widget's config blob
// without forcing every caller to reshape it — the backend stores
// position and config as opaque JSONB so the frontend owns the shape
// and keeps old widgets readable when we evolve the renderer.
export interface WidgetChartConfig {
  x_column?: string;
  y_column?: string;
  value_column?: string;
  label_column?: string;
  title?: string;
  /** For number_card / pivot renderers that don't lean on x/y. */
  format?: "number" | "currency" | "percent";
}

const palette = [
  "#2563eb",
  "#10b981",
  "#f59e0b",
  "#ef4444",
  "#8b5cf6",
  "#14b8a6",
  "#f97316",
  "#6366f1",
];

interface Props {
  viz: InsightsVizType | string;
  result: InsightsRunResult | ReportResult | null | undefined;
  config?: WidgetChartConfig;
  /** Fallback message when the widget has no saved query. */
  emptyText?: string;
}

/**
 * InsightsWidgetChart renders a single widget's query result as the
 * requested visualization. Every viz falls back to a table when the
 * result shape is incompatible (e.g. non-numeric y-column for a bar
 * chart) so the user sees *something* instead of an empty frame.
 */
export function InsightsWidgetChart({ viz, result, config = {}, emptyText }: Props) {
  const reportResult = resolveResult(result);
  if (!reportResult) {
    return (
      <div style={{ padding: 12, color: "#9ca3af", fontStyle: "italic" }}>
        {emptyText ?? "No data"}
      </div>
    );
  }
  const rows = reportResult.rows ?? [];
  const columns = reportResult.columns ?? [];
  if (rows.length === 0) {
    return (
      <div style={{ padding: 12, color: "#9ca3af", fontStyle: "italic" }}>
        Empty result
      </div>
    );
  }

  const xCol = config.x_column ?? columns[0] ?? "";
  const yCol =
    config.y_column ?? columns.find((c) => c !== xCol) ?? columns[0] ?? "";

  switch (viz) {
    case "bar":
      return (
        <ResponsiveContainer width="100%" height={260}>
          <BarChart data={rows as Array<Record<string, unknown>>}>
            <CartesianGrid strokeDasharray="3 3" />
            <XAxis dataKey={xCol} />
            <YAxis />
            <Tooltip />
            <Legend />
            <Bar dataKey={yCol} fill={palette[0]} />
          </BarChart>
        </ResponsiveContainer>
      );
    case "line":
      return (
        <ResponsiveContainer width="100%" height={260}>
          <LineChart data={rows as Array<Record<string, unknown>>}>
            <CartesianGrid strokeDasharray="3 3" />
            <XAxis dataKey={xCol} />
            <YAxis />
            <Tooltip />
            <Legend />
            <Line type="monotone" dataKey={yCol} stroke={palette[0]} />
          </LineChart>
        </ResponsiveContainer>
      );
    case "pie":
    case "donut": {
      const labelCol = config.label_column ?? xCol;
      const valueCol = config.value_column ?? yCol;
      return (
        <ResponsiveContainer width="100%" height={260}>
          <PieChart>
            <Pie
              data={rows as Array<Record<string, unknown>>}
              dataKey={valueCol}
              nameKey={labelCol}
              cx="50%"
              cy="50%"
              outerRadius={100}
              innerRadius={viz === "donut" ? 55 : 0}
              label
            >
              {(rows as Array<Record<string, unknown>>).map((_, i) => (
                <Cell key={i} fill={palette[i % palette.length]} />
              ))}
            </Pie>
            <Tooltip />
          </PieChart>
        </ResponsiveContainer>
      );
    }
    case "funnel": {
      const labelCol = config.label_column ?? xCol;
      const valueCol = config.value_column ?? yCol;
      const funnelData = (rows as Array<Record<string, unknown>>).map((r, i) => ({
        name: String(r[labelCol] ?? ""),
        value: Number(r[valueCol] ?? 0),
        fill: palette[i % palette.length],
      }));
      return (
        <ResponsiveContainer width="100%" height={260}>
          <FunnelChart>
            <Tooltip />
            <Funnel dataKey="value" data={funnelData} isAnimationActive>
              <LabelList position="right" fill="#111" stroke="none" dataKey="name" />
            </Funnel>
          </FunnelChart>
        </ResponsiveContainer>
      );
    }
    case "number_card": {
      // Display the first (metric) value from the first row, labelled
      // with the configured title or the column name.
      const metricCol = config.value_column ?? yCol;
      const raw = rows[0]?.[metricCol];
      return (
        <div style={{ padding: 16, textAlign: "center" }}>
          <div style={{ fontSize: 12, color: "#6b7280", textTransform: "uppercase" }}>
            {config.title ?? metricCol}
          </div>
          <div style={{ fontSize: 36, fontWeight: 600, marginTop: 4 }}>
            {formatMetric(raw, config.format)}
          </div>
        </div>
      );
    }
    case "pivot":
      return renderPivot(reportResult);
    case "table":
    default:
      return renderTable(reportResult);
  }
}

function resolveResult(
  input: InsightsRunResult | ReportResult | null | undefined
): ReportResult | null {
  if (!input) return null;
  if ("result" in input) return input.result ?? null;
  return input as ReportResult;
}

function renderTable(result: ReportResult) {
  const columns = result.columns ?? [];
  const rows = result.rows ?? [];
  return (
    <div style={{ overflowX: "auto", maxHeight: 260 }}>
      <table style={{ borderCollapse: "collapse", fontSize: 12, width: "100%" }}>
        <thead>
          <tr style={{ background: "#f9fafb" }}>
            {columns.map((c) => (
              <th
                key={c}
                style={{
                  textAlign: "left",
                  padding: "4px 8px",
                  borderBottom: "1px solid #e5e7eb",
                }}
              >
                {c}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              {columns.map((c) => (
                <td
                  key={c}
                  style={{ padding: "4px 8px", borderBottom: "1px solid #f3f4f6" }}
                >
                  {String((r as Record<string, unknown>)[c] ?? "")}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function renderPivot(result: ReportResult) {
  // The reporting runner pre-computes the pivot on the server when
  // the definition requests it; fall back to flat-table when it
  // didn't (e.g. a widget configured `viz_type = pivot` on a plain
  // definition).
  const pivot = result.pivot;
  if (!pivot) return renderTable(result);
  const { row_headers, column_headers, cells } = pivot;
  return (
    <div style={{ overflowX: "auto", maxHeight: 260 }}>
      <table style={{ borderCollapse: "collapse", fontSize: 12 }}>
        <thead>
          <tr style={{ background: "#f9fafb" }}>
            <th style={{ padding: "4px 8px" }} />
            {column_headers.map((c, i) => (
              <th key={i} style={{ padding: "4px 8px", textAlign: "right" }}>
                {String(c)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {row_headers.map((r, i) => (
            <tr key={i}>
              <th
                style={{
                  padding: "4px 8px",
                  textAlign: "left",
                  background: "#f9fafb",
                }}
              >
                {String(r)}
              </th>
              {(cells[i] ?? []).map((v, j) => (
                <td key={j} style={{ padding: "4px 8px", textAlign: "right" }}>
                  {String(v ?? "")}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function formatMetric(v: unknown, format?: "number" | "currency" | "percent"): string {
  if (v === null || v === undefined) return "—";
  const n = Number(v);
  if (!Number.isFinite(n)) return String(v);
  switch (format) {
    case "currency":
      return n.toLocaleString(undefined, { style: "currency", currency: "USD" });
    case "percent":
      return `${(n * 100).toFixed(1)}%`;
    case "number":
    default:
      return n.toLocaleString();
  }
}
