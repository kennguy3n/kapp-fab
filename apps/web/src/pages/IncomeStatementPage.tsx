import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";
import type { IncomeStatementLine } from "@kapp/client";
import {
  Button,
  Eyebrow,
  Field,
  Input,
  StatCard,
  type StatTrend,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import {
  csvFilename,
  downloadCsv,
  type Money,
  parseAmount,
  useMoney,
} from "../lib/finance/format";
import { FinanceError, TableSkeleton } from "../lib/finance/presentation";

interface MergedLine {
  code: string;
  name: string;
  current: number;
  prior?: number;
}

/**
 * IncomeStatementPage shows revenue, expenses, and net income for a
 * chosen period, with quick presets and an optional comparison against
 * the immediately preceding period of equal length.
 */
export function IncomeStatementPage() {
  const { defaultFrom, defaultTo } = useMemo(defaultRange, []);
  const [from, setFrom] = useState<string>(defaultFrom);
  const [to, setTo] = useState<string>(defaultTo);
  const [compare, setCompare] = useState(false);
  const money = useMoney();

  const valid = !!from && !!to && from <= to;
  const prior = useMemo(
    () => (compare && valid ? previousPeriod(from, to) : null),
    [compare, valid, from, to],
  );

  const q = useQuery({
    queryKey: ["finance", "income-statement", from, to],
    queryFn: () => api.getIncomeStatement(from, to),
    enabled: valid,
  });

  const priorQ = useQuery({
    queryKey: ["finance", "income-statement", prior?.from, prior?.to],
    queryFn: () => api.getIncomeStatement(prior!.from, prior!.to),
    enabled: !!prior,
  });

  const report = q.data;
  const priorReport = prior ? priorQ.data : undefined;
  const showCompare = !!prior && !!priorReport;

  const revenue = useMemo(
    () => mergeLines(report?.revenue ?? [], priorReport?.revenue, compare),
    [report, priorReport, compare],
  );
  const expense = useMemo(
    () => mergeLines(report?.expense ?? [], priorReport?.expense, compare),
    [report, priorReport, compare],
  );

  const exportCsv = () => {
    if (!report) return;
    const headers = ["Section", "Code", "Account", "Amount"];
    if (showCompare) headers.push("Previous", "Change");
    const lineRow = (section: string, l: MergedLine) => {
      const row = [section, l.code, l.name, l.current.toFixed(2)];
      if (showCompare) {
        row.push((l.prior ?? 0).toFixed(2), (l.current - (l.prior ?? 0)).toFixed(2));
      }
      return row;
    };
    const rows: string[][] = [
      ...revenue.map((l) => lineRow("Revenue", l)),
      totalRow("Total revenue", report.total_revenue, priorReport?.total_revenue, showCompare),
      ...expense.map((l) => lineRow("Expense", l)),
      totalRow("Total expenses", report.total_expense, priorReport?.total_expense, showCompare),
      totalRow("Net income", report.net_income, priorReport?.net_income, showCompare),
    ];
    downloadCsv(csvFilename(`income-statement_${from}_${to}`), headers, rows);
    toast.success("Income statement exported");
  };

  const netIncome = report ? parseAmount(report.net_income) || 0 : 0;

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Finance</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Income Statement
            </h1>
            <p className="mt-1 text-sm text-fg-muted">
              Revenue minus expenses over the period you choose.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              leadingIcon={<Download aria-hidden />}
              onClick={exportCsv}
              disabled={!report}
            >
              Export CSV
            </Button>
          </div>
        </div>

        <div className="flex flex-wrap items-end gap-3">
          <Field label="From">
            <Input
              type="date"
              value={from}
              max={to || undefined}
              onChange={(e) => setFrom(e.target.value)}
              className="w-auto"
            />
          </Field>
          <Field label="To">
            <Input
              type="date"
              value={to}
              min={from || undefined}
              onChange={(e) => setTo(e.target.value)}
              className="w-auto"
            />
          </Field>
          <div className="flex flex-wrap items-center gap-1.5">
            {PRESETS.map((p) => (
              <Button
                key={p.label}
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => {
                  const r = p.range();
                  setFrom(r.from);
                  setTo(r.to);
                }}
              >
                {p.label}
              </Button>
            ))}
          </div>
          <label className="flex items-center gap-2 text-sm text-fg">
            <input
              type="checkbox"
              checked={compare}
              onChange={(e) => setCompare(e.target.checked)}
              className="h-4 w-4 rounded border-border-strong text-accent focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
            />
            Compare to previous period
          </label>
        </div>
        {!valid && (
          <p className="text-sm text-danger">
            The “from” date must be on or before the “to” date.
          </p>
        )}
      </header>

      {q.isLoading && <TableSkeleton columns={3} />}

      {q.isError && (
        <FinanceError
          title="Couldn't load the income statement"
          error={q.error}
          onRetry={() => void q.refetch()}
        />
      )}

      {report && (
        <>
          <div className="grid gap-3 sm:grid-cols-3">
            <StatCard
              label="Total revenue"
              value={money(report.total_revenue)}
              trend={trendFor(report.total_revenue, priorReport?.total_revenue, "up-good", money, showCompare)}
            />
            <StatCard
              label="Total expenses"
              value={money(report.total_expense)}
              trend={trendFor(report.total_expense, priorReport?.total_expense, "up-bad", money, showCompare)}
            />
            <StatCard
              label="Net income"
              value={money(report.net_income)}
              sub={netIncome >= 0 ? "Profit for the period" : "Loss for the period"}
              trend={trendFor(report.net_income, priorReport?.net_income, "up-good", money, showCompare)}
            />
          </div>

          <LineSection
            title="Revenue"
            lines={revenue}
            total={report.total_revenue}
            priorTotal={priorReport?.total_revenue}
            showCompare={showCompare}
            money={money}
          />
          <LineSection
            title="Expenses"
            lines={expense}
            total={report.total_expense}
            priorTotal={priorReport?.total_expense}
            showCompare={showCompare}
            money={money}
          />

          <div className="flex items-center justify-between rounded-lg border border-border-strong bg-bg-subtle px-4 py-3">
            <span className="text-sm font-semibold text-fg">Net income</span>
            <span
              className={`text-lg font-semibold tabular-nums ${
                netIncome >= 0 ? "text-success" : "text-danger"
              }`}
            >
              {money(report.net_income)}
            </span>
          </div>
        </>
      )}
    </section>
  );
}

function LineSection({
  title,
  lines,
  total,
  priorTotal,
  showCompare,
  money,
}: {
  title: string;
  lines: MergedLine[];
  total: string;
  priorTotal?: string;
  showCompare: boolean;
  money: Money;
}) {
  const colSpan = showCompare ? 4 : 2;
  return (
    <div className="flex flex-col gap-2">
      <h2 className="text-sm font-semibold text-fg">{title}</h2>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-28">Code</TableHead>
            <TableHead>Account</TableHead>
            <TableHead className="text-right">Amount</TableHead>
            {showCompare && (
              <>
                <TableHead className="text-right">Previous</TableHead>
                <TableHead className="text-right">Change</TableHead>
              </>
            )}
          </TableRow>
        </TableHeader>
        <TableBody>
          {lines.length === 0 && (
            <TableRow>
              <TableCell colSpan={colSpan + 1} className="py-8 text-center text-fg-muted">
                No {title.toLowerCase()} recorded in this period.
              </TableCell>
            </TableRow>
          )}
          {lines.map((l) => {
            const change = l.current - (l.prior ?? 0);
            return (
              <TableRow key={l.code}>
                <TableCell className="font-mono text-xs text-fg-muted">
                  {l.code}
                </TableCell>
                <TableCell className="text-fg">{l.name}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {money(l.current, { blankZero: true })}
                </TableCell>
                {showCompare && (
                  <>
                    <TableCell className="text-right tabular-nums text-fg-muted">
                      {money(l.prior ?? 0, { blankZero: true })}
                    </TableCell>
                    <TableCell
                      className={`text-right tabular-nums ${
                        change > 0
                          ? "text-success"
                          : change < 0
                            ? "text-danger"
                            : "text-fg-muted"
                      }`}
                    >
                      {change === 0 ? "—" : money(change)}
                    </TableCell>
                  </>
                )}
              </TableRow>
            );
          })}
        </TableBody>
        <TableFooter>
          <TableRow>
            <TableCell colSpan={2} className="font-semibold">
              Total {title.toLowerCase()}
            </TableCell>
            <TableCell className="text-right font-semibold tabular-nums">
              {money(total)}
            </TableCell>
            {showCompare && (
              <>
                <TableCell className="text-right font-semibold tabular-nums text-fg-muted">
                  {money(priorTotal ?? 0)}
                </TableCell>
                <TableCell className="text-right font-semibold tabular-nums">
                  {money((parseAmount(total) || 0) - (parseAmount(priorTotal) || 0))}
                </TableCell>
              </>
            )}
          </TableRow>
        </TableFooter>
      </Table>
    </div>
  );
}

function mergeLines(
  current: IncomeStatementLine[],
  prior: IncomeStatementLine[] | undefined,
  compare: boolean,
): MergedLine[] {
  const map = new Map<string, MergedLine>();
  for (const l of current) {
    map.set(l.account_code, {
      code: l.account_code,
      name: l.account_name,
      current: parseAmount(l.amount) || 0,
      prior: compare ? 0 : undefined,
    });
  }
  if (compare && prior) {
    for (const l of prior) {
      const existing = map.get(l.account_code);
      const amount = parseAmount(l.amount) || 0;
      if (existing) existing.prior = amount;
      else
        map.set(l.account_code, {
          code: l.account_code,
          name: l.account_name,
          current: 0,
          prior: amount,
        });
    }
  }
  return [...map.values()].sort((a, b) => a.code.localeCompare(b.code));
}

function trendFor(
  current: string,
  prior: string | undefined,
  polarity: "up-good" | "up-bad",
  money: Money,
  showCompare: boolean,
): StatTrend | undefined {
  if (!showCompare || prior === undefined) return undefined;
  const cur = parseAmount(current) || 0;
  const prev = parseAmount(prior) || 0;
  const delta = cur - prev;
  const direction = delta > 0 ? "up" : delta < 0 ? "down" : "flat";
  const good = polarity === "up-good" ? delta > 0 : delta < 0;
  const intent = delta === 0 ? "neutral" : good ? "positive" : "negative";
  const pct =
    prev !== 0 ? `${delta >= 0 ? "+" : "−"}${Math.abs((delta / prev) * 100).toFixed(1)}%` : money(Math.abs(delta));
  return { direction, intent, value: pct };
}

function totalRow(
  label: string,
  total: string,
  priorTotal: string | undefined,
  showCompare: boolean,
): string[] {
  const cur = parseAmount(total) || 0;
  const row = ["", "", label, cur.toFixed(2)];
  if (showCompare) {
    const prev = parseAmount(priorTotal) || 0;
    row.push(prev.toFixed(2), (cur - prev).toFixed(2));
  }
  return row;
}

const PRESETS: { label: string; range: () => { from: string; to: string } }[] = [
  {
    label: "This year",
    range: () => {
      const now = new Date();
      return { from: `${now.getFullYear()}-01-01`, to: toISODate(now) };
    },
  },
  {
    label: "Last year",
    range: () => {
      const y = new Date().getFullYear() - 1;
      return { from: `${y}-01-01`, to: `${y}-12-31` };
    },
  },
  {
    label: "This quarter",
    range: () => {
      const now = new Date();
      const q = Math.floor(now.getMonth() / 3);
      const start = new Date(now.getFullYear(), q * 3, 1);
      return { from: toISODate(start), to: toISODate(now) };
    },
  },
  {
    label: "This month",
    range: () => {
      const now = new Date();
      const start = new Date(now.getFullYear(), now.getMonth(), 1);
      return { from: toISODate(start), to: toISODate(now) };
    },
  },
];

function parseISODate(s: string): Date {
  const [y, m, d] = s.split("-").map(Number);
  return new Date(y, (m ?? 1) - 1, d ?? 1);
}

function toISODate(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// previousPeriod returns the equal-length window immediately before
// [from, to], so a 6-month YTD compares against the prior 6 months.
function previousPeriod(from: string, to: string): { from: string; to: string } {
  const f = parseISODate(from);
  const t = parseISODate(to);
  const days = Math.round((t.getTime() - f.getTime()) / 86_400_000);
  const priorTo = new Date(f);
  priorTo.setDate(priorTo.getDate() - 1);
  const priorFrom = new Date(priorTo);
  priorFrom.setDate(priorFrom.getDate() - days);
  return { from: toISODate(priorFrom), to: toISODate(priorTo) };
}

// defaultRange returns year-to-date in the viewer's local calendar.
function defaultRange(): { defaultFrom: string; defaultTo: string } {
  const now = new Date();
  return {
    defaultFrom: `${now.getFullYear()}-01-01`,
    defaultTo: toISODate(now),
  };
}
