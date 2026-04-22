import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";

/**
 * TrialBalancePage shows every account balance as of a user-chosen
 * date. Total debits must equal total credits — any non-zero residual
 * is surfaced prominently since it signals a broken posting.
 */
export function TrialBalancePage() {
  const [asOf, setAsOf] = useState<string>(() => new Date().toISOString().slice(0, 10));

  const q = useQuery({
    queryKey: ["finance", "trial-balance", asOf],
    queryFn: () => api.getTrialBalance(asOf),
  });

  const report = q.data;

  return (
    <section>
      <h1>Trial Balance</h1>
      <p style={{ color: "#6b7280" }}>
        Account-level summary of debits and credits as of the selected date.
      </p>

      <div style={{ margin: "12px 0", fontSize: 13 }}>
        <label style={{ marginRight: 8 }}>As of:</label>
        <input
          type="date"
          value={asOf}
          onChange={(e) => setAsOf(e.target.value)}
        />
      </div>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load report: {(q.error as Error).message}
        </p>
      )}

      {report && (
        <>
          <table
            style={{
              width: "100%",
              borderCollapse: "collapse",
              marginTop: 12,
              fontSize: 13,
            }}
          >
            <thead>
              <tr style={{ textAlign: "left", color: "#6b7280" }}>
                <Th>Code</Th>
                <Th>Account</Th>
                <Th>Type</Th>
                <Th style={{ textAlign: "right" }}>Debit</Th>
                <Th style={{ textAlign: "right" }}>Credit</Th>
                <Th style={{ textAlign: "right" }}>Balance</Th>
              </tr>
            </thead>
            <tbody>
              {report.rows.map((r) => (
                <tr
                  key={r.account_code}
                  style={{ borderTop: "1px solid #e5e7eb" }}
                >
                  <Td>
                    <code>{r.account_code}</code>
                  </Td>
                  <Td>{r.account_name}</Td>
                  <Td>{r.account_type}</Td>
                  <Td style={{ textAlign: "right" }}>{fmt(r.debit)}</Td>
                  <Td style={{ textAlign: "right" }}>{fmt(r.credit)}</Td>
                  <Td style={{ textAlign: "right" }}>{fmt(r.balance)}</Td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr
                style={{
                  borderTop: "2px solid #d1d5db",
                  fontWeight: 600,
                }}
              >
                <Td colSpan={3}>Totals</Td>
                <Td style={{ textAlign: "right" }}>{fmt(report.total_debit)}</Td>
                <Td style={{ textAlign: "right" }}>{fmt(report.total_credit)}</Td>
                <Td
                  style={{
                    textAlign: "right",
                    color: report.balanced ? "#059669" : "#b91c1c",
                  }}
                >
                  {report.balanced ? "balanced" : "OUT OF BALANCE"}
                </Td>
              </tr>
            </tfoot>
          </table>
        </>
      )}
    </section>
  );
}

function fmt(v: string | number): string {
  const n = typeof v === "string" ? Number(v) : v;
  if (!isFinite(n) || n === 0) return "—";
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
