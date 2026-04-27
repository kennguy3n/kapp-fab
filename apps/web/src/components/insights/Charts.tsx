// Phase L Insights — chart renderers.
//
// Each component accepts a `ReportResult` (the shape returned by the
// insights runner: `{ columns: string[]; rows: Record<string, unknown>[] }`)
// plus the per-widget config selecting which columns map to which axis.
// Mirrors viz_type values in `internal/insights.VizType*`.

import {
  Bar,
  BarChart as RcBarChart,
  CartesianGrid,
  Cell,
  Funnel,
  FunnelChart as RcFunnelChart,
  LabelList,
  Legend,
  Line,
  LineChart as RcLineChart,
  Pie,
  PieChart as RcPieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type {
  InsightsVizType,
  InsightsWidgetConfig,
  ReportResult,
} from "@kapp/client";

export interface ChartProps {
  result: ReportResult;
  config?: InsightsWidgetConfig;
  height?: number;
}

// Categorical palette tuned for accessibility on light backgrounds; the
// pie / donut / funnel renderers cycle through this list per slice. Kept
// short on purpose — anything past 8 categories should be a stacked bar
// or a table.
const PALETTE = [
  "#2563eb",
  "#16a34a",
  "#dc2626",
  "#ea580c",
  "#9333ea",
  "#0891b2",
  "#ca8a04",
  "#db2777",
];

function firstNumericColumn(result: ReportResult): string | undefined {
  for (const col of result.columns) {
    if (
      result.rows.some(
        (r) => typeof r[col] === "number" || typeof r[col] === "bigint"
      )
    ) {
      return col;
    }
  }
  return undefined;
}

function asNumber(value: unknown): number {
  if (typeof value === "number") return value;
  if (typeof value === "bigint") return Number(value);
  if (typeof value === "string") {
    const n = Number(value);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}

function asLabel(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}

function pickXY(
  result: ReportResult,
  config: InsightsWidgetConfig | undefined
): { x: string; y: string } {
  const x = config?.x_column ?? result.columns[0] ?? "x";
  const y =
    config?.y_column ??
    config?.value_column ??
    firstNumericColumn(result) ??
    result.columns[1] ??
    result.columns[0] ??
    "y";
  return { x, y };
}

export function BarChart({ result, config, height = 280 }: ChartProps) {
  const { x, y } = pickXY(result, config);
  const data = result.rows.map((row) => ({
    [x]: asLabel(row[x]),
    [y]: asNumber(row[y]),
  }));
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcBarChart data={data}>
        <CartesianGrid strokeDasharray="3 3" />
        <XAxis dataKey={x} />
        <YAxis />
        <Tooltip />
        <Legend />
        <Bar dataKey={y} fill={PALETTE[0]} />
      </RcBarChart>
    </ResponsiveContainer>
  );
}

export function LineChart({ result, config, height = 280 }: ChartProps) {
  const { x, y } = pickXY(result, config);
  const data = result.rows.map((row) => ({
    [x]: asLabel(row[x]),
    [y]: asNumber(row[y]),
  }));
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcLineChart data={data}>
        <CartesianGrid strokeDasharray="3 3" />
        <XAxis dataKey={x} />
        <YAxis />
        <Tooltip />
        <Legend />
        <Line
          type="monotone"
          dataKey={y}
          stroke={PALETTE[0]}
          strokeWidth={2}
          dot={false}
        />
      </RcLineChart>
    </ResponsiveContainer>
  );
}

function pieData(
  result: ReportResult,
  config: InsightsWidgetConfig | undefined
) {
  const category =
    config?.category_column ??
    config?.x_column ??
    result.columns[0] ??
    "category";
  const value =
    config?.value_column ??
    config?.y_column ??
    firstNumericColumn(result) ??
    result.columns[1] ??
    "value";
  return result.rows.map((row) => ({
    name: asLabel(row[category]),
    value: asNumber(row[value]),
  }));
}

export function PieChart({ result, config, height = 280 }: ChartProps) {
  const data = pieData(result, config);
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcPieChart>
        <Tooltip />
        <Legend />
        <Pie data={data} dataKey="value" nameKey="name" outerRadius={100} label>
          {data.map((_, i) => (
            <Cell key={i} fill={PALETTE[i % PALETTE.length]} />
          ))}
        </Pie>
      </RcPieChart>
    </ResponsiveContainer>
  );
}

export function DonutChart({ result, config, height = 280 }: ChartProps) {
  const data = pieData(result, config);
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcPieChart>
        <Tooltip />
        <Legend />
        <Pie
          data={data}
          dataKey="value"
          nameKey="name"
          innerRadius={60}
          outerRadius={100}
          label
        >
          {data.map((_, i) => (
            <Cell key={i} fill={PALETTE[i % PALETTE.length]} />
          ))}
        </Pie>
      </RcPieChart>
    </ResponsiveContainer>
  );
}

export function FunnelChart({ result, config, height = 280 }: ChartProps) {
  const data = pieData(result, config);
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcFunnelChart>
        <Tooltip />
        <Funnel dataKey="value" data={data} isAnimationActive={false}>
          <LabelList position="right" fill="#111" stroke="none" dataKey="name" />
          {data.map((_, i) => (
            <Cell key={i} fill={PALETTE[i % PALETTE.length]} />
          ))}
        </Funnel>
      </RcFunnelChart>
    </ResponsiveContainer>
  );
}

function formatValue(value: number, format?: string): string {
  if (!format) return new Intl.NumberFormat().format(value);
  if (format === "currency") {
    return new Intl.NumberFormat(undefined, {
      style: "currency",
      currency: "USD",
    }).format(value);
  }
  if (format === "percent") {
    return new Intl.NumberFormat(undefined, {
      style: "percent",
      maximumFractionDigits: 2,
    }).format(value);
  }
  return new Intl.NumberFormat().format(value);
}

export function NumberCard({ result, config, height = 120 }: ChartProps) {
  const valueCol =
    config?.value_column ?? firstNumericColumn(result) ?? result.columns[0];
  const total = result.rows.reduce(
    (sum, row) => sum + asNumber(row[valueCol ?? ""]),
    0
  );
  const display = result.rows.length === 1
    ? asNumber(result.rows[0][valueCol ?? ""])
    : total;
  return (
    <div
      style={{
        height,
        display: "flex",
        flexDirection: "column",
        justifyContent: "center",
        alignItems: "center",
        gap: 6,
      }}
    >
      <div style={{ fontSize: 32, fontWeight: 600, color: "#111" }}>
        {formatValue(display, config?.format)}
      </div>
      <div style={{ fontSize: 12, color: "#6b7280" }}>
        {config?.title ?? valueCol ?? "value"}
      </div>
    </div>
  );
}

export function TableView({ result, height = 280 }: ChartProps) {
  return (
    <div style={{ maxHeight: height, overflow: "auto" }}>
      <table
        style={{
          width: "100%",
          fontSize: 13,
          borderCollapse: "collapse",
        }}
      >
        <thead style={{ position: "sticky", top: 0, background: "#f9fafb" }}>
          <tr>
            {result.columns.map((c) => (
              <th
                key={c}
                style={{
                  textAlign: "left",
                  padding: "6px 8px",
                  borderBottom: "1px solid #e5e7eb",
                  fontWeight: 600,
                }}
              >
                {c}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {result.rows.map((row, i) => (
            <tr key={i}>
              {result.columns.map((c) => (
                <td
                  key={c}
                  style={{
                    padding: "4px 8px",
                    borderBottom: "1px solid #f3f4f6",
                  }}
                >
                  {asLabel(row[c])}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// PivotTable renders the optional `pivot` block returned by the
// reporting runner alongside the row grid. Falls back to TableView
// if the runner did not pivot the result.
export function PivotTable({ result, height = 320 }: ChartProps) {
  const pivot = result.pivot;
  if (!pivot) return <TableView result={result} height={height} />;
  return (
    <div style={{ maxHeight: height, overflow: "auto" }}>
      <table style={{ width: "100%", fontSize: 13, borderCollapse: "collapse" }}>
        <thead style={{ position: "sticky", top: 0, background: "#f9fafb" }}>
          <tr>
            <th style={{ padding: "6px 8px", textAlign: "left" }}> </th>
            {pivot.column_headers.map((h, i) => (
              <th
                key={i}
                style={{
                  padding: "6px 8px",
                  textAlign: "right",
                  borderBottom: "1px solid #e5e7eb",
                }}
              >
                {asLabel(h)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {pivot.row_headers.map((rh, i) => (
            <tr key={i}>
              <td
                style={{
                  padding: "4px 8px",
                  fontWeight: 500,
                  borderBottom: "1px solid #f3f4f6",
                }}
              >
                {asLabel(rh)}
              </td>
              {(pivot.cells[i] ?? []).map((cell, j) => (
                <td
                  key={j}
                  style={{
                    padding: "4px 8px",
                    textAlign: "right",
                    borderBottom: "1px solid #f3f4f6",
                  }}
                >
                  {asLabel(cell)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export interface VizProps {
  vizType: InsightsVizType;
  result: ReportResult;
  config?: InsightsWidgetConfig;
  height?: number;
}

// Viz dispatches to the right renderer based on viz_type. Used by
// both the QueryBuilder live preview and the Dashboard widget grid
// so the mapping lives in exactly one place.
export function Viz({ vizType, result, config, height }: VizProps) {
  switch (vizType) {
    case "bar":
      return <BarChart result={result} config={config} height={height} />;
    case "line":
      return <LineChart result={result} config={config} height={height} />;
    case "pie":
      return <PieChart result={result} config={config} height={height} />;
    case "donut":
      return <DonutChart result={result} config={config} height={height} />;
    case "funnel":
      return <FunnelChart result={result} config={config} height={height} />;
    case "number_card":
      return <NumberCard result={result} config={config} height={height} />;
    case "pivot":
      return <PivotTable result={result} config={config} height={height} />;
    case "table":
    default:
      return <TableView result={result} config={config} height={height} />;
  }
}
