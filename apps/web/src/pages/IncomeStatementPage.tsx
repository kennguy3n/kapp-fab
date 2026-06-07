import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Input,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * IncomeStatementPage shows revenue, expenses, and net income for a
 * user-chosen date range. Defaults to year-to-date.
 */
export function IncomeStatementPage() {
  const { defaultFrom, defaultTo } = useMemo(defaultRange, []);
  const [from, setFrom] = useState<string>(defaultFrom);
  const [to, setTo] = useState<string>(defaultTo);

  const q = useQuery({
    queryKey: ["finance", "income-statement", from, to],
    queryFn: () => api.getIncomeStatement(from, to),
    enabled: !!from && !!to,
  });

  const report = q.data;

  return (
    <section>
      <h1>Income Statement</h1>
      <p className="text-fg-muted">
        Revenue minus expenses for the selected period.
      </p>

      <div className="my-3 flex items-center gap-3 text-[13px]">
        <label className="flex items-center gap-2">
          From:
          <Input
            type="date"
            value={from}
            onChange={(e) => setFrom(e.target.value)}
            className="w-auto"
          />
        </label>
        <label className="flex items-center gap-2">
          To:
          <Input
            type="date"
            value={to}
            onChange={(e) => setTo(e.target.value)}
            className="w-auto"
          />
        </label>
      </div>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load report: {(q.error as Error).message}
        </p>
      )}

      {report && (
        <div className="mt-3 text-[13px]">
          <h2 className="text-sm">Revenue</h2>
          <LineTable lines={report.revenue} total={report.total_revenue} />
          <h2 className="mt-4 text-sm">Expenses</h2>
          <LineTable lines={report.expense} total={report.total_expense} />
          <div
            className={`mt-4 flex justify-between border-t-2 border-border p-3 text-sm font-semibold ${
              Number(report.net_income) >= 0 ? "text-success" : "text-danger"
            }`}
          >
            <span>Net income</span>
            <span>{fmt(report.net_income)}</span>
          </div>
        </div>
      )}
    </section>
  );
}

function LineTable({
  lines,
  total,
}: {
  lines: { account_code: string; account_name: string; amount: string }[];
  total: string;
}) {
  return (
    <Table className="text-[13px]">
      <TableHeader>
        <TableRow>
          <TableHead>Code</TableHead>
          <TableHead>Account</TableHead>
          <TableHead className="text-right">Amount</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {lines.map((l) => (
          <TableRow key={l.account_code}>
            <TableCell>
              <code>{l.account_code}</code>
            </TableCell>
            <TableCell>{l.account_name}</TableCell>
            <TableCell className="text-right">{fmt(l.amount)}</TableCell>
          </TableRow>
        ))}
        {lines.length === 0 && (
          <TableRow>
            <TableCell colSpan={3}>
              <em className="text-fg-subtle">No entries in this range.</em>
            </TableCell>
          </TableRow>
        )}
      </TableBody>
      <TableFooter>
        <TableRow className="font-semibold">
          <TableCell colSpan={2}>Total</TableCell>
          <TableCell className="text-right">{fmt(total)}</TableCell>
        </TableRow>
      </TableFooter>
    </Table>
  );
}

// defaultRange returns from/to in the viewer's local calendar. Formatting
// `toISOString()` of a local-constructed Date would shift the day in UTC+
// zones (e.g. Jan 1 local → Dec 31 UTC), bleeding prior-year entries into
// the default range.
function defaultRange(): { defaultFrom: string; defaultTo: string } {
  const now = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return {
    defaultFrom: `${now.getFullYear()}-01-01`,
    defaultTo: `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`,
  };
}

function fmt(v: string | number): string {
  const n = typeof v === "string" ? Number(v) : v;
  if (!isFinite(n)) return "—";
  return n.toFixed(2);
}


