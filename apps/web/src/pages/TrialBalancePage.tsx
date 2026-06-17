import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";
import {
  Button,
  Eyebrow,
  Field,
  Input,
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
  parseAmount,
  useMoney,
} from "../lib/finance/format";
import {
  AccountTypeBadge,
  accountTypeLabel,
  BalancedBadge,
  FinanceError,
  TableSkeleton,
} from "../lib/finance/presentation";

/**
 * TrialBalancePage shows every account balance as of a user-chosen
 * date. Total debits must equal total credits — any non-zero residual
 * is surfaced prominently since it signals a broken posting. Each
 * account links through to the journal entries that produced its
 * balance.
 */
export function TrialBalancePage() {
  const [asOf, setAsOf] = useState<string>(todayLocalISO);
  const money = useMoney();

  const q = useQuery({
    queryKey: ["finance", "trial-balance", asOf],
    queryFn: () => api.getTrialBalance(asOf),
  });

  const report = q.data;
  const balanced = report ? parseAmount(report.residual) === 0 : true;

  const exportCsv = () => {
    if (!report) return;
    const rows = report.rows.map((r) => [
      r.account_code,
      r.account_name,
      accountTypeLabel(r.type),
      r.debit,
      r.credit,
      r.balance,
    ]);
    rows.push([
      "",
      "Totals",
      "",
      report.total_debit,
      report.total_credit,
      report.residual,
    ]);
    downloadCsv(
      csvFilename("trial-balance", asOf),
      ["Code", "Account", "Type", "Debit", "Credit", "Balance"],
      rows,
    );
    toast.success("Trial balance exported", {
      description: `${report.rows.length} accounts as of ${asOf}.`,
    });
  };

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Finance</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Trial Balance
            </h1>
            <p className="mt-1 text-sm text-fg-muted">
              Account-level summary of debits and credits as of the selected
              date. Totals should tie out exactly.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            {report && <BalancedBadge balanced={balanced} />}
            <Button
              variant="outline"
              leadingIcon={<Download aria-hidden />}
              onClick={exportCsv}
              disabled={!report || report.rows.length === 0}
            >
              Export CSV
            </Button>
          </div>
        </div>
        <div className="max-w-xs">
          <Field label="As of date">
            <Input
              type="date"
              value={asOf}
              onChange={(e) => setAsOf(e.target.value)}
            />
          </Field>
        </div>
      </header>

      {q.isLoading && <TableSkeleton columns={6} />}

      {q.isError && (
        <FinanceError error={q.error} onRetry={() => void q.refetch()} />
      )}

      {report && report.rows.length === 0 && (
        <Table>
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
            <TableRow>
              <TableCell
                colSpan={6}
                className="py-8 text-center text-fg-muted"
              >
                No posted balances as of {asOf}. Post a journal entry to see
                accounts appear here.
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      )}

      {report && report.rows.length > 0 && (
        <Table>
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
                  <Link
                    to={`/finance/journal?account_code=${encodeURIComponent(r.account_code)}`}
                    className="font-mono text-xs text-accent hover:underline focus-visible:underline focus-visible:outline-none"
                    title={`View journal entries for ${r.account_name}`}
                  >
                    {r.account_code}
                  </Link>
                </TableCell>
                <TableCell className="text-fg">{r.account_name}</TableCell>
                <TableCell>
                  <AccountTypeBadge type={r.type} />
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {money(r.debit, { blankZero: true })}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {money(r.credit, { blankZero: true })}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {money(r.balance, { blankZero: true })}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
          <TableFooter>
            <TableRow>
              <TableCell colSpan={3} className="font-semibold">
                Totals
              </TableCell>
              <TableCell className="text-right font-semibold tabular-nums">
                {money(report.total_debit)}
              </TableCell>
              <TableCell className="text-right font-semibold tabular-nums">
                {money(report.total_credit)}
              </TableCell>
              <TableCell
                className={`text-right font-semibold tabular-nums ${
                  balanced ? "text-success" : "text-danger"
                }`}
              >
                {money(report.residual)}
              </TableCell>
            </TableRow>
          </TableFooter>
        </Table>
      )}
    </section>
  );
}

// todayLocalISO returns YYYY-MM-DD in the viewer's local timezone. Using
// `new Date().toISOString().slice(0, 10)` is off-by-one for UTC+ zones
// because it formats the UTC instant, not the local calendar day.
function todayLocalISO(): string {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}
