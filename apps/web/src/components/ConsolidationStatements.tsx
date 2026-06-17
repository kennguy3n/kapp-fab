import {
  Badge,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import type {
  ConsolidatedStatementRow,
  ConsolidatedStatements,
} from "./ConsolidationApi";
import { useFormatter } from "../lib/i18n/useFormatter";
import { formatMoney } from "./reconciliation";
import { ct } from "./ConsolidationStrings";

function StatementSection({
  title,
  rows,
  total,
  totalLabel,
}: {
  title: string;
  rows: ConsolidatedStatementRow[];
  total: string;
  totalLabel: string;
}) {
  const f = useFormatter();
  const money = (value: string) => formatMoney(f, Number(value));
  return (
    <div>
      <h4 className="mb-1 text-xs font-medium uppercase tracking-wide text-fg-subtle">
        {title}
      </h4>
      <Table className="text-sm">
        <TableHeader>
          <TableRow>
            <TableHead>{ct("consolidation.tb.account")}</TableHead>
            <TableHead className="text-right">
              {ct("consolidation.stmt.amount")}
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={r.account_code}>
              <TableCell>
                <code>{r.account_code}</code>
                {r.account_name ? (
                  <span className="ml-2 text-fg-muted">{r.account_name}</span>
                ) : null}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {money(r.amount)}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
        <TableFooter>
          <TableRow className="font-medium">
            <TableCell>{totalLabel}</TableCell>
            <TableCell className="text-right tabular-nums">
              {money(total)}
            </TableCell>
          </TableRow>
        </TableFooter>
      </Table>
    </div>
  );
}

export interface ConsolidationStatementsProps {
  statements: ConsolidatedStatements;
}

/**
 * Renders the consolidated income statement (P&L) and balance sheet
 * derived from the same balanced trial balance, with net income and a
 * balanced indicator broken out.
 */
export function ConsolidationStatements({
  statements,
}: ConsolidationStatementsProps) {
  const f = useFormatter();
  const money = (value: string) => formatMoney(f, Number(value));
  const is = statements.income_statement;
  const bs = statements.balance_sheet;
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle>{ct("consolidation.stmt.incomeStatement")}</CardTitle>
          <p className="text-sm text-fg-muted">
            {ct("consolidation.stmt.netIncome")}:{" "}
            <span className="font-medium tabular-nums">
              {money(is.net_income)}
            </span>{" "}
            {is.presentation_currency}
          </p>
        </CardHeader>
        <CardContent className="grid gap-3">
          <StatementSection
            title={ct("consolidation.stmt.revenue")}
            rows={is.revenue}
            total={is.total_revenue}
            totalLabel={ct("consolidation.stmt.totalRevenue")}
          />
          <StatementSection
            title={ct("consolidation.stmt.expense")}
            rows={is.expense}
            total={is.total_expense}
            totalLabel={ct("consolidation.stmt.totalExpense")}
          />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            {ct("consolidation.stmt.balanceSheet")}
            <Badge variant={bs.balanced ? "success" : "danger"}>
              {bs.balanced
                ? ct("consolidation.tb.balanced")
                : ct("consolidation.tb.unbalanced")}
            </Badge>
          </CardTitle>
          <p className="text-sm text-fg-muted">
            {ct("consolidation.stmt.totalAssets")}:{" "}
            <span className="font-medium tabular-nums">
              {money(bs.total_assets)}
            </span>{" "}
            · {ct("consolidation.stmt.totalLiabilities")}:{" "}
            <span className="font-medium tabular-nums">
              {money(bs.total_liabilities)}
            </span>{" "}
            · {ct("consolidation.stmt.totalEquity")}:{" "}
            <span className="font-medium tabular-nums">
              {money(bs.total_equity)}
            </span>
          </p>
        </CardHeader>
        <CardContent className="grid gap-3">
          <StatementSection
            title={ct("consolidation.stmt.assets")}
            rows={bs.assets}
            total={bs.total_assets}
            totalLabel={ct("consolidation.stmt.totalAssets")}
          />
          <StatementSection
            title={ct("consolidation.stmt.liabilities")}
            rows={bs.liabilities}
            total={bs.total_liabilities}
            totalLabel={ct("consolidation.stmt.totalLiabilities")}
          />
          <StatementSection
            title={ct("consolidation.stmt.equity")}
            rows={bs.equity}
            total={bs.total_equity}
            totalLabel={ct("consolidation.stmt.totalEquity")}
          />
        </CardContent>
      </Card>
    </div>
  );
}
