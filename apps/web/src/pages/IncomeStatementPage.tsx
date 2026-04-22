import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
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
      <p style={{ color: "#6b7280" }}>
        Revenue minus expenses for the selected period.
      </p>

      <div style={{ margin: "12px 0", fontSize: 13, display: "flex", gap: 12 }}>
        <label>
          From:{" "}
          <input
            type="date"
            value={from}
            onChange={(e) => setFrom(e.target.value)}
          />
        </label>
        <label>
          To:{" "}
          <input
            type="date"
            value={to}
            onChange={(e) => setTo(e.target.value)}
          />
        </label>
      </div>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load report: {(q.error as Error).message}
        </p>
      )}

      {report && (
        <div style={{ marginTop: 12, fontSize: 13 }}>
          <h2 style={{ fontSize: 14 }}>Revenue</h2>
          <LineTable lines={report.revenue} total={report.total_revenue} />
          <h2 style={{ fontSize: 14, marginTop: 16 }}>Expenses</h2>
          <LineTable lines={report.expense} total={report.total_expense} />
          <div
            style={{
              marginTop: 16,
              padding: "12px",
              borderTop: "2px solid #d1d5db",
              fontWeight: 600,
              fontSize: 14,
              display: "flex",
              justifyContent: "space-between",
              color: Number(report.net_income) >= 0 ? "#059669" : "#b91c1c",
            }}
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
    <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
      <thead>
        <tr style={{ textAlign: "left", color: "#6b7280" }}>
          <Th>Code</Th>
          <Th>Account</Th>
          <Th style={{ textAlign: "right" }}>Amount</Th>
        </tr>
      </thead>
      <tbody>
        {lines.map((l) => (
          <tr
            key={l.account_code}
            style={{ borderTop: "1px solid #e5e7eb" }}
          >
            <Td>
              <code>{l.account_code}</code>
            </Td>
            <Td>{l.account_name}</Td>
            <Td style={{ textAlign: "right" }}>{fmt(l.amount)}</Td>
          </tr>
        ))}
        {lines.length === 0 && (
          <tr>
            <Td colSpan={3}>
              <em style={{ color: "#9ca3af" }}>No entries in this range.</em>
            </Td>
          </tr>
        )}
      </tbody>
      <tfoot>
        <tr style={{ borderTop: "2px solid #d1d5db", fontWeight: 600 }}>
          <Td colSpan={2}>Total</Td>
          <Td style={{ textAlign: "right" }}>{fmt(total)}</Td>
        </tr>
      </tfoot>
    </table>
  );
}

function defaultRange(): { defaultFrom: string; defaultTo: string } {
  const now = new Date();
  const yearStart = new Date(now.getFullYear(), 0, 1);
  return {
    defaultFrom: yearStart.toISOString().slice(0, 10),
    defaultTo: now.toISOString().slice(0, 10),
  };
}

function fmt(v: string | number): string {
  const n = typeof v === "string" ? Number(v) : v;
  if (!isFinite(n)) return "—";
  return n.toFixed(2);
}

function Th({
  children,
  style,
}: {
  children: React.ReactNode;
  style?: React.CSSProperties;
}) {
  return (
    <th style={{ padding: "6px 8px", fontWeight: 500, fontSize: 12, ...style }}>
      {children}
    </th>
  );
}

function Td({
  children,
  style,
  colSpan,
}: {
  children: React.ReactNode;
  style?: React.CSSProperties;
  colSpan?: number;
}) {
  return (
    <td style={{ padding: "8px", ...style }} colSpan={colSpan}>
      {children}
    </td>
  );
}
