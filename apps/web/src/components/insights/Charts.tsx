// Insights — chart renderers.
//
// Each component accepts a `ReportResult` (the shape returned by the
// insights runner: `{ columns: string[]; rows: Record<string, unknown>[] }`)
// plus the per-widget config selecting which columns map to which axis.
// Mirrors viz_type values in `internal/insights.VizType*`.
//
// Styling rules (KChat design language):
//   - Series colours come from a violet-family palette that tracks the
//     KChat accent. Because `@kapp/ui`'s globals.css can't be imported
//     into JS (and carries no chart-series tokens), the palette is
//     defined here as the single local source of truth and mirrors the
//     token colour space (oklch). It re-resolves on light/dark toggle.
//   - Axis / grid / tooltip chrome uses the same theme-aware token set.
//   - Column headers are humanized field labels and cell values are
//     locale-formatted — never raw field keys, enum tokens, UUIDs, or
//     ISO timestamps.

import { useEffect, useState } from "react";
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
import { humanizeLabel, humanizeToken } from "../../lib/ktypeView";

export interface ChartProps {
  result: ReportResult;
  config?: InsightsWidgetConfig;
  height?: number;
}

// Violet-family series palette tuned for the KChat accent (~277°).
// Index 0 is the accent itself; the rest fan across the violet →
// indigo → magenta arc so adjacent series stay distinguishable while
// reading as one branded family. Light values are saturated for
// contrast on a light surface; dark values lift in lightness for a
// dark surface (mirrors the accent's light/dark token treatment).
const SERIES_LIGHT = [
  "oklch(48% 0.23 277)",
  "oklch(58% 0.19 292)",
  "oklch(55% 0.17 318)",
  "oklch(50% 0.15 255)",
  "oklch(64% 0.16 300)",
  "oklch(43% 0.20 285)",
  "oklch(68% 0.13 330)",
  "oklch(53% 0.14 240)",
];
const SERIES_DARK = [
  "oklch(80% 0.11 282)",
  "oklch(76% 0.12 295)",
  "oklch(78% 0.11 320)",
  "oklch(74% 0.10 258)",
  "oklch(82% 0.10 300)",
  "oklch(72% 0.12 285)",
  "oklch(84% 0.09 332)",
  "oklch(77% 0.10 242)",
];

interface ChartTheme {
  series: string[];
  grid: string;
  axis: string;
  tooltipBg: string;
  tooltipBorder: string;
  tooltipFg: string;
}

const THEME_LIGHT: ChartTheme = {
  series: SERIES_LIGHT,
  grid: "oklch(92% 0.004 286)",
  axis: "oklch(44% 0.01 286)",
  tooltipBg: "oklch(99% 0.002 286)",
  tooltipBorder: "oklch(90% 0.005 286)",
  tooltipFg: "oklch(25% 0.01 286)",
};
const THEME_DARK: ChartTheme = {
  series: SERIES_DARK,
  grid: "oklch(34% 0.008 286)",
  axis: "oklch(72% 0.02 286)",
  tooltipBg: "oklch(24% 0.008 286)",
  tooltipBorder: "oklch(36% 0.008 286)",
  tooltipFg: "oklch(96% 0.004 286)",
};

// Tracks the `.dark` class the theme runtime toggles on <html> so the
// SVG charts (whose colours are attributes, not CSS, and therefore
// can't inherit a CSS variable) recolour on theme switch.
function useChartTheme(): ChartTheme {
  const read = () =>
    typeof document !== "undefined" &&
    document.documentElement.classList.contains("dark");
  const [dark, setDark] = useState(read);
  useEffect(() => {
    if (typeof document === "undefined") return;
    const el = document.documentElement;
    const obs = new MutationObserver(() => setDark(el.classList.contains("dark")));
    obs.observe(el, { attributes: true, attributeFilter: ["class"] });
    setDark(el.classList.contains("dark"));
    return () => obs.disconnect();
  }, []);
  return dark ? THEME_DARK : THEME_LIGHT;
}

const numberFmt = new Intl.NumberFormat();
const dateFmt = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });
const dateTimeFmt = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?)?$/;
const ENUM_TOKEN_RE = /^[a-z][a-z0-9]*(_[a-z0-9]+)*$/;

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

function columnIsNumeric(result: ReportResult, col: string): boolean {
  let sawValue = false;
  for (const row of result.rows) {
    const v = row[col];
    if (v === null || v === undefined || v === "") continue;
    sawValue = true;
    if (typeof v !== "number" && typeof v !== "bigint") return false;
  }
  return sawValue;
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

// Axis / legend / slice label: humanize enum-ish tokens so a bar's
// x-axis reads "Closed Won" rather than "closed_won".
function asLabel(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "number" || typeof value === "bigint") {
    return numberFmt.format(Number(value));
  }
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (typeof value === "object") return JSON.stringify(value);
  const s = String(value);
  if (ENUM_TOKEN_RE.test(s)) return humanizeToken(s);
  return s;
}

// Table / pivot cell: never surface a raw UUID or ISO timestamp;
// format numbers and dates; humanize enum tokens.
function formatCell(value: unknown): string {
  if (value === null || value === undefined || value === "") return "—";
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (typeof value === "number") return numberFmt.format(value);
  if (typeof value === "bigint") return numberFmt.format(Number(value));
  if (typeof value === "object") return JSON.stringify(value);
  const s = String(value);
  if (UUID_RE.test(s)) return "—";
  if (ISO_DATE_RE.test(s)) {
    const d = new Date(s);
    if (!Number.isNaN(d.getTime())) {
      return s.length > 10 ? dateTimeFmt.format(d) : dateFmt.format(d);
    }
  }
  if (ENUM_TOKEN_RE.test(s)) return humanizeToken(s);
  return s;
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
  const theme = useChartTheme();
  const { x, y } = pickXY(result, config);
  const data = result.rows.map((row) => ({
    [x]: asLabel(row[x]),
    [y]: asNumber(row[y]),
  }));
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcBarChart data={data} margin={{ top: 8, right: 12, bottom: 4, left: 4 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={theme.grid} vertical={false} />
        <XAxis
          dataKey={x}
          stroke={theme.axis}
          tick={{ fill: theme.axis, fontSize: 12 }}
          tickLine={false}
        />
        <YAxis
          stroke={theme.axis}
          tick={{ fill: theme.axis, fontSize: 12 }}
          tickLine={false}
          tickFormatter={(v) => numberFmt.format(Number(v))}
        />
        <Tooltip
          cursor={{ fill: theme.grid, opacity: 0.3 }}
          formatter={(v) => numberFmt.format(Number(v))}
          labelFormatter={(l) => humanizeLabel(x) + ": " + asLabel(l)}
          contentStyle={{
            background: theme.tooltipBg,
            border: `1px solid ${theme.tooltipBorder}`,
            borderRadius: 8,
            color: theme.tooltipFg,
          }}
          labelStyle={{ color: theme.tooltipFg }}
          itemStyle={{ color: theme.tooltipFg }}
        />
        <Legend formatter={() => humanizeLabel(y)} />
        <Bar dataKey={y} name={humanizeLabel(y)} fill={theme.series[0]} radius={[4, 4, 0, 0]} />
      </RcBarChart>
    </ResponsiveContainer>
  );
}

export function LineChart({ result, config, height = 280 }: ChartProps) {
  const theme = useChartTheme();
  const { x, y } = pickXY(result, config);
  const data = result.rows.map((row) => ({
    [x]: asLabel(row[x]),
    [y]: asNumber(row[y]),
  }));
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcLineChart data={data} margin={{ top: 8, right: 12, bottom: 4, left: 4 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={theme.grid} vertical={false} />
        <XAxis
          dataKey={x}
          stroke={theme.axis}
          tick={{ fill: theme.axis, fontSize: 12 }}
          tickLine={false}
        />
        <YAxis
          stroke={theme.axis}
          tick={{ fill: theme.axis, fontSize: 12 }}
          tickLine={false}
          tickFormatter={(v) => numberFmt.format(Number(v))}
        />
        <Tooltip
          formatter={(v) => numberFmt.format(Number(v))}
          labelFormatter={(l) => humanizeLabel(x) + ": " + asLabel(l)}
          contentStyle={{
            background: theme.tooltipBg,
            border: `1px solid ${theme.tooltipBorder}`,
            borderRadius: 8,
            color: theme.tooltipFg,
          }}
          labelStyle={{ color: theme.tooltipFg }}
          itemStyle={{ color: theme.tooltipFg }}
        />
        <Legend formatter={() => humanizeLabel(y)} />
        <Line
          type="monotone"
          dataKey={y}
          name={humanizeLabel(y)}
          stroke={theme.series[0]}
          strokeWidth={2}
          dot={{ r: 3, fill: theme.series[0], strokeWidth: 0 }}
          activeDot={{ r: 5 }}
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

function PieTooltip({ theme }: { theme: ChartTheme }) {
  return (
    <Tooltip
      formatter={(v: unknown, n: unknown) => [numberFmt.format(Number(v)), String(n)]}
      contentStyle={{
        background: theme.tooltipBg,
        border: `1px solid ${theme.tooltipBorder}`,
        borderRadius: 8,
        color: theme.tooltipFg,
      }}
      labelStyle={{ color: theme.tooltipFg }}
      itemStyle={{ color: theme.tooltipFg }}
    />
  );
}

export function PieChart({ result, config, height = 280 }: ChartProps) {
  const theme = useChartTheme();
  const data = pieData(result, config);
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcPieChart>
        {PieTooltip({ theme })}
        <Legend />
        <Pie data={data} dataKey="value" nameKey="name" outerRadius="80%" label>
          {data.map((_, i) => (
            <Cell key={i} fill={theme.series[i % theme.series.length]} />
          ))}
        </Pie>
      </RcPieChart>
    </ResponsiveContainer>
  );
}

export function DonutChart({ result, config, height = 280 }: ChartProps) {
  const theme = useChartTheme();
  const data = pieData(result, config);
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcPieChart>
        {PieTooltip({ theme })}
        <Legend />
        <Pie
          data={data}
          dataKey="value"
          nameKey="name"
          innerRadius="55%"
          outerRadius="80%"
          paddingAngle={2}
          label
        >
          {data.map((_, i) => (
            <Cell key={i} fill={theme.series[i % theme.series.length]} />
          ))}
        </Pie>
      </RcPieChart>
    </ResponsiveContainer>
  );
}

export function FunnelChart({ result, config, height = 280 }: ChartProps) {
  const theme = useChartTheme();
  const data = pieData(result, config);
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RcFunnelChart>
        {PieTooltip({ theme })}
        <Funnel dataKey="value" data={data} isAnimationActive={false}>
          <LabelList
            position="right"
            fill={theme.axis}
            stroke="none"
            dataKey="name"
          />
          {data.map((_, i) => (
            <Cell key={i} fill={theme.series[i % theme.series.length]} />
          ))}
        </Funnel>
      </RcFunnelChart>
    </ResponsiveContainer>
  );
}

function formatValue(value: number, format?: string): string {
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
  return numberFmt.format(value);
}

export function NumberCard({ result, config, height = 120 }: ChartProps) {
  const valueCol =
    config?.value_column ?? firstNumericColumn(result) ?? result.columns[0];
  const total = result.rows.reduce(
    (sum, row) => sum + asNumber(row[valueCol ?? ""]),
    0
  );
  const display =
    result.rows.length === 1
      ? asNumber(result.rows[0][valueCol ?? ""])
      : total;
  const label =
    config?.title ?? (valueCol ? humanizeLabel(valueCol) : "Value");
  return (
    <div
      className="flex flex-col items-center justify-center gap-1.5 text-center"
      style={{ height }}
    >
      <div className="font-tabular text-4xl font-semibold tracking-tight text-fg">
        {formatValue(display, config?.format)}
      </div>
      <div className="text-sm text-fg-muted">{label}</div>
    </div>
  );
}

export function TableView({ result, height = 280 }: ChartProps) {
  const numericCols = new Set(
    result.columns.filter((c) => columnIsNumeric(result, c))
  );
  return (
    <div
      className="overflow-auto rounded-lg border border-border"
      style={{ maxHeight: height }}
    >
      <table className="w-full border-collapse text-sm">
        <thead className="sticky top-0 z-10 bg-bg-subtle">
          <tr>
            {result.columns.map((c) => (
              <th
                key={c}
                scope="col"
                className={`whitespace-nowrap border-b border-border px-3 py-2 font-medium text-fg-muted ${
                  numericCols.has(c) ? "text-right" : "text-left"
                }`}
              >
                {humanizeLabel(c)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {result.rows.map((row, i) => (
            <tr
              key={i}
              className="border-b border-border last:border-0 hover:bg-bg-subtle"
            >
              {result.columns.map((c) => (
                <td
                  key={c}
                  className={`px-3 py-2 text-fg ${
                    numericCols.has(c)
                      ? "text-right font-tabular tabular-nums"
                      : "text-left"
                  }`}
                >
                  {formatCell(row[c])}
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
export function PivotTable({ result, config, height = 320 }: ChartProps) {
  const pivot = result.pivot;
  if (!pivot) return <TableView result={result} config={config} height={height} />;
  return (
    <div
      className="overflow-auto rounded-lg border border-border"
      style={{ maxHeight: height }}
    >
      <table className="w-full border-collapse text-sm">
        <thead className="sticky top-0 z-10 bg-bg-subtle">
          <tr>
            <th scope="col" className="border-b border-border px-3 py-2 text-left font-medium text-fg-muted">
              {" "}
            </th>
            {pivot.column_headers.map((h, i) => (
              <th
                key={i}
                scope="col"
                className="whitespace-nowrap border-b border-border px-3 py-2 text-right font-medium text-fg-muted"
              >
                {formatCell(h)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {pivot.row_headers.map((rh, i) => (
            <tr
              key={i}
              className="border-b border-border last:border-0 hover:bg-bg-subtle"
            >
              <th
                scope="row"
                className="px-3 py-2 text-left font-medium text-fg"
              >
                {formatCell(rh)}
              </th>
              {(pivot.cells[i] ?? []).map((cell, j) => (
                <td
                  key={j}
                  className="px-3 py-2 text-right font-tabular tabular-nums text-fg"
                >
                  {formatCell(cell)}
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
