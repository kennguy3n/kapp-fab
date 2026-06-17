import { Fragment, useMemo, useState } from "react";
import {
  Badge,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  StatCard,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import type {
  ConsolidatedRow,
  ConsolidatedTrialBalance,
} from "./ConsolidationApi";
import { useFormatter } from "../lib/i18n/useFormatter";
import { formatMoney, shortId } from "./reconciliation";
import { accountTypeLabel, ct, ctp } from "./ConsolidationStrings";

function isZero(amount: string | undefined): boolean {
  if (!amount) return true;
  return Number(amount) === 0;
}

export interface ConsolidationTrialBalanceProps {
  result: ConsolidatedTrialBalance;
  /** The CTA equity account code configured for the active group, so
   *  the matching row can be visually flagged. Defaults to 3900. */
  ctaAccountCode?: string;
}

/**
 * Renders a consolidated trial balance with one column per member
 * entity, a per-row drill-down to the contributing per-entity amounts
 * and any intercompany eliminations applied to that account, and the
 * CTA / residual / balanced status broken out so the consolidation is
 * auditable end to end.
 */
export function ConsolidationTrialBalance({
  result,
  ctaAccountCode,
}: ConsolidationTrialBalanceProps) {
  const [open, setOpen] = useState<string | null>(null);
  const f = useFormatter();
  // Presentation-currency figures render as grouped two-decimal numbers
  // (the currency itself is stated once in the card title), right-aligned
  // with tabular figures so columns line up to the cent.
  const money = (value: string | number) =>
    formatMoney(f, typeof value === "number" ? value : Number(value));

  // Stable union of every tenant that contributes to any row — these
  // become the per-entity columns.
  const entities = useMemo(() => {
    const seen = new Set<string>();
    const ordered: string[] = [];
    for (const row of result.rows) {
      for (const c of row.contributions ?? []) {
        if (!seen.has(c.tenant_id)) {
          seen.add(c.tenant_id);
          ordered.push(c.tenant_id);
        }
      }
    }
    return ordered;
  }, [result.rows]);

  // Member tenants surface as "Entity 1…N" (the raw tenant UUID stays in
  // the column's title for auditing) so no machine id leaks into a label.
  const entityNo = useMemo(() => {
    const m = new Map<string, number>();
    entities.forEach((id, i) => m.set(id, i + 1));
    return m;
  }, [entities]);
  const entityName = (id: string) => {
    const n = entityNo.get(id);
    return n ? ctp("consolidation.tb.entityN", { n }) : shortId(id);
  };

  const cta = ctaAccountCode ?? "3900";
  const residual = result.residual ?? "0";
  const balanced = isZero(residual);

  // Eliminations grouped by account_code so a drill-down can show the
  // intercompany entries netted out of that line.
  const eliminationsByAccount = useMemo(() => {
    const map = new Map<string, ConsolidatedRow[]>();
    for (const e of result.eliminated) {
      const list = map.get(e.account_code) ?? [];
      list.push(e);
      map.set(e.account_code, list);
    }
    return map;
  }, [result.eliminated]);

  const colSpan = 2 + entities.length + 1;

  return (
    <Card>
      <CardHeader>
        <CardTitle>
          {ct("consolidation.tb.heading")} — {result.presentation_currency}
        </CardTitle>
        <p className="text-sm text-fg-muted">
          {ct("consolidation.tb.asOf")}: {f.date(new Date(result.as_of))}
        </p>
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
          <StatCard
            label={ct("consolidation.tb.totalDebit")}
            value={money(result.total_debit)}
          />
          <StatCard
            label={ct("consolidation.tb.totalCredit")}
            value={money(result.total_credit)}
          />
          <StatCard
            label={ct("consolidation.tb.cta")}
            value={money(result.cta ?? "0")}
          />
          <StatCard
            label={ct("consolidation.tb.residual")}
            value={money(residual)}
            sub={
              <Badge variant={balanced ? "success" : "danger"}>
                {balanced
                  ? ct("consolidation.tb.balanced")
                  : ct("consolidation.tb.unbalanced")}
              </Badge>
            }
          />
        </div>

        <p className="text-xs text-fg-subtle">{ct("consolidation.tb.drillHint")}</p>

        <Table className="text-sm">
          <TableHeader>
            <TableRow>
              <TableHead>{ct("consolidation.tb.account")}</TableHead>
              <TableHead>{ct("consolidation.tb.type")}</TableHead>
              {entities.map((id) => (
                <TableHead key={id} className="text-right" title={id}>
                  {entityName(id)}
                </TableHead>
              ))}
              <TableHead className="text-right">
                {ct("consolidation.tb.balance")}
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {result.rows.map((row) => {
              const isCta = row.account_code === cta;
              const contribById = new Map(
                (row.contributions ?? []).map((c) => [c.tenant_id, c]),
              );
              const expanded = open === row.account_code;
              const elims = eliminationsByAccount.get(row.account_code) ?? [];
              return (
                <Fragment key={row.account_code}>
                  <TableRow
                    className={`cursor-pointer ${isCta ? "font-medium" : ""}`}
                    onClick={() =>
                      setOpen(expanded ? null : row.account_code)
                    }
                    data-testid={`tb-row-${row.account_code}`}
                  >
                    <TableCell>
                      <span className="inline-flex items-center gap-2">
                        <span aria-hidden>{expanded ? "▾" : "▸"}</span>
                        <code>{row.account_code}</code>
                        {row.account_name ? (
                          <span className="text-fg-muted">{row.account_name}</span>
                        ) : null}
                        {isCta ? (
                          <Badge variant="info" size="xs">
                            {ct("consolidation.tb.ctaRow")}
                          </Badge>
                        ) : null}
                      </span>
                    </TableCell>
                    <TableCell className="text-fg-muted">
                      {row.type ? accountTypeLabel(row.type) : ""}
                    </TableCell>
                    {entities.map((id) => {
                      const c = contribById.get(id);
                      return (
                        <TableCell
                          key={id}
                          className="text-right tabular-nums text-fg-muted"
                        >
                          {c ? money(Number(c.debit) - Number(c.credit)) : ""}
                        </TableCell>
                      );
                    })}
                    <TableCell className="text-right font-medium tabular-nums">
                      {money(row.balance)}
                    </TableCell>
                  </TableRow>

                  {expanded ? (
                    <TableRow data-testid={`tb-drill-${row.account_code}`}>
                      <TableCell colSpan={colSpan} className="bg-bg-muted">
                        <div className="grid gap-3 py-1">
                          <div>
                            <p className="mb-1 text-xs font-medium uppercase tracking-wide text-fg-subtle">
                              {ct("consolidation.tb.contributions")}
                            </p>
                            {(row.contributions ?? []).length === 0 ? (
                              <p className="text-xs italic text-fg-subtle">
                                {ct("consolidation.tb.noContributions")}
                              </p>
                            ) : (
                              <Table className="text-xs">
                                <TableHeader>
                                  <TableRow>
                                    <TableHead>
                                      {ct("consolidation.tb.entity")}
                                    </TableHead>
                                    <TableHead className="text-right">
                                      {ct("consolidation.tb.debit")}
                                    </TableHead>
                                    <TableHead className="text-right">
                                      {ct("consolidation.tb.credit")}
                                    </TableHead>
                                  </TableRow>
                                </TableHeader>
                                <TableBody>
                                  {(row.contributions ?? []).map((c) => (
                                    <TableRow key={c.tenant_id}>
                                      <TableCell title={c.tenant_id}>
                                        {entityName(c.tenant_id)}
                                      </TableCell>
                                      <TableCell className="text-right tabular-nums">
                                        {money(c.debit)}
                                      </TableCell>
                                      <TableCell className="text-right tabular-nums">
                                        {money(c.credit)}
                                      </TableCell>
                                    </TableRow>
                                  ))}
                                </TableBody>
                              </Table>
                            )}
                          </div>

                          {elims.length > 0 ? (
                            <div>
                              <p className="mb-1 text-xs font-medium uppercase tracking-wide text-fg-subtle">
                                {ct("consolidation.tb.eliminationsApplied")}
                              </p>
                              <Table className="text-xs">
                                <TableHeader>
                                  <TableRow>
                                    <TableHead>
                                      {ct("consolidation.tb.account")}
                                    </TableHead>
                                    <TableHead className="text-right">
                                      {ct("consolidation.tb.debit")}
                                    </TableHead>
                                    <TableHead className="text-right">
                                      {ct("consolidation.tb.credit")}
                                    </TableHead>
                                  </TableRow>
                                </TableHeader>
                                <TableBody>
                                  {elims.map((e, i) => (
                                    <TableRow key={`${e.account_code}-${i}`}>
                                      <TableCell>
                                        <code>{e.account_code}</code>
                                      </TableCell>
                                      <TableCell className="text-right tabular-nums">
                                        {money(e.debit)}
                                      </TableCell>
                                      <TableCell className="text-right tabular-nums">
                                        {money(e.credit)}
                                      </TableCell>
                                    </TableRow>
                                  ))}
                                </TableBody>
                              </Table>
                            </div>
                          ) : null}
                        </div>
                      </TableCell>
                    </TableRow>
                  ) : null}
                </Fragment>
              );
            })}
          </TableBody>
          <TableFooter>
            <TableRow className="font-semibold">
              <TableCell>{ct("consolidation.tb.total")}</TableCell>
              <TableCell />
              {entities.map((id) => (
                <TableCell key={id} />
              ))}
              <TableCell className="text-right tabular-nums">
                {money(result.total_debit)} / {money(result.total_credit)}
              </TableCell>
            </TableRow>
          </TableFooter>
        </Table>

        {result.eliminated.length > 0 ? (
          <div>
            <h3 className="mb-1 text-sm font-medium">
              {ct("consolidation.tb.eliminated")}
            </h3>
            <Table className="text-sm">
              <TableHeader>
                <TableRow>
                  <TableHead>{ct("consolidation.tb.account")}</TableHead>
                  <TableHead className="text-right">
                    {ct("consolidation.tb.debit")}
                  </TableHead>
                  <TableHead className="text-right">
                    {ct("consolidation.tb.credit")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {result.eliminated.map((row, i) => (
                  <TableRow key={`${row.account_code}-${i}`}>
                    <TableCell>
                      <code>{row.account_code}</code>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {money(row.debit)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {money(row.credit)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
