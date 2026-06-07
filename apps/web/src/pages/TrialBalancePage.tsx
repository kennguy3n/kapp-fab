import { useState } from "react";
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
 * TrialBalancePage shows every account balance as of a user-chosen
 * date. Total debits must equal total credits — any non-zero residual
 * is surfaced prominently since it signals a broken posting.
 */
export function TrialBalancePage() {
  const [asOf, setAsOf] = useState<string>(todayLocalISO);

  const q = useQuery({
    queryKey: ["finance", "trial-balance", asOf],
    queryFn: () => api.getTrialBalance(asOf),
  });

  const report = q.data;

  return (
    <section>
      <h1>Trial Balance</h1>
      <p className="text-fg-muted">
        Account-level summary of debits and credits as of the selected date.
      </p>

      <div className="my-3 flex items-center gap-2 text-[13px]">
        <label>As of:</label>
        <Input
          type="date"
          value={asOf}
          onChange={(e) => setAsOf(e.target.value)}
          className="w-auto"
        />
      </div>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load report: {(q.error as Error).message}
        </p>
      )}

      {report && (
        <Table className="mt-3 text-[13px]">
          <TableHeader>
            <TableRow>
              <TableHead>Code</TableHead>
              <TableHead>Account</TableHead>
              <TableHead>Type</TableHead>
              <TableHead className="text-right">Debit</TableHead>
              <TableHead className="text-right">Credit</TableHead>
              <TableHead className="text-right">Balance</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {report.rows.map((r) => (
              <TableRow key={r.account_code}>
                <TableCell>
                  <code>{r.account_code}</code>
                </TableCell>
                <TableCell>{r.account_name}</TableCell>
                <TableCell>{r.type}</TableCell>
                <TableCell className="text-right">{fmt(r.debit)}</TableCell>
                <TableCell className="text-right">{fmt(r.credit)}</TableCell>
                <TableCell className="text-right">{fmt(r.balance)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
          <TableFooter>
            <TableRow className="font-semibold">
              <TableCell colSpan={3}>Totals</TableCell>
              <TableCell className="text-right">{fmt(report.total_debit)}</TableCell>
              <TableCell className="text-right">{fmt(report.total_credit)}</TableCell>
              <TableCell
                className={`text-right ${
                  isBalanced(report.residual) ? "text-success" : "text-danger"
                }`}
              >
                {isBalanced(report.residual) ? "balanced" : "OUT OF BALANCE"}
              </TableCell>
            </TableRow>
          </TableFooter>
        </Table>
      )}
    </section>
  );
}

// isBalanced treats a trial balance as balanced when the backend-reported
// residual (total_debit - total_credit) is numerically zero. The backend
// emits residual as a decimal string.
function isBalanced(residual: string): boolean {
  return Number(residual) === 0;
}

// todayLocalISO returns YYYY-MM-DD in the viewer's local timezone. Using
// `new Date().toISOString().slice(0, 10)` is off-by-one for UTC+ zones
// because it formats the UTC instant, not the local calendar day.
function todayLocalISO(): string {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

function fmt(v: string | number): string {
  const n = typeof v === "string" ? Number(v) : v;
  if (!isFinite(n) || n === 0) return "—";
  return n.toFixed(2);
}


