import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { Download, X } from "lucide-react";
import type { JournalEntry } from "@kapp/client";
import {
  Badge,
  Button,
  Eyebrow,
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
import { ktypeSingular } from "../lib/ktypeView";
import { useFormatter } from "../lib/i18n";
import {
  csvFilename,
  downloadCsv,
  parseAmount,
  useMoney,
} from "../lib/finance/format";
import {
  BalancedBadge,
  FinanceError,
  TableSkeleton,
} from "../lib/finance/presentation";

// Float tolerance for the debit == credit posting check.
const EPSILON = 0.005;

function entryTotals(entry: JournalEntry): {
  debit: number;
  credit: number;
  balanced: boolean;
} {
  let debit = 0;
  let credit = 0;
  for (const l of entry.lines) {
    debit += parseAmount(l.debit) || 0;
    credit += parseAmount(l.credit) || 0;
  }
  return { debit, credit, balanced: Math.abs(debit - credit) < EPSILON };
}

/**
 * JournalEntriesPage lists posted journal entries with their lines,
 * source linkage, and per-entry balance check. Read-only — new JEs are
 * posted via the invoice/bill/payroll flows. Filters arrive via query
 * params (account drill-down, date window, source document) and are
 * shown as a clearable banner.
 */
export function JournalEntriesPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const f = useFormatter();
  const money = useMoney();

  const filter = useMemo(
    () => ({
      account_code: searchParams.get("account_code") ?? undefined,
      from: searchParams.get("from") ?? undefined,
      to: searchParams.get("to") ?? undefined,
      source_ktype: searchParams.get("source_ktype") ?? undefined,
      source_id: searchParams.get("source_id") ?? undefined,
    }),
    [searchParams],
  );

  const hasFilter = Object.values(filter).some(
    (v) => v !== undefined && v !== "",
  );

  const q = useQuery({
    queryKey: ["finance", "journal-entries", filter],
    queryFn: () => api.listJournalEntries(filter),
  });

  // Resolve account codes to names so lines read "1020 · Accounts
  // Receivable" rather than a bare code.
  const accountsQ = useQuery({
    queryKey: ["finance", "accounts"],
    queryFn: () => api.listAccounts(),
  });
  const accountName = useMemo(() => {
    const map = new Map<string, string>();
    for (const a of accountsQ.data ?? []) map.set(a.code, a.name);
    return map;
  }, [accountsQ.data]);

  const entries = q.data ?? [];
  const fmtDate = (iso: string) => (iso ? f.date(new Date(iso)) : "—");

  const exportCsv = () => {
    if (entries.length === 0) return;
    const rows = entries.flatMap((e) =>
      e.lines.map((l) => [
        fmtDate(e.posted_at),
        e.memo || "",
        e.source_ktype ? ktypeSingular(e.source_ktype) : "Manual",
        l.account_code,
        accountName.get(l.account_code) ?? "",
        l.debit,
        l.credit,
        l.currency,
        l.memo || "",
      ]),
    );
    downloadCsv(
      csvFilename("journal-entries"),
      [
        "Date",
        "Entry",
        "Source",
        "Account code",
        "Account",
        "Debit",
        "Credit",
        "Currency",
        "Line memo",
      ],
      rows,
    );
    toast.success("Journal entries exported", {
      description: `${entries.length} ${entries.length === 1 ? "entry" : "entries"}.`,
    });
  };

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Finance</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Journal Entries
            </h1>
            <p className="mt-1 text-sm text-fg-muted">
              Posted double-entry transactions. Every entry must balance —
              total debits equal total credits.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              leadingIcon={<Download aria-hidden />}
              onClick={exportCsv}
              disabled={entries.length === 0}
            >
              Export CSV
            </Button>
          </div>
        </div>

        {hasFilter && (
          <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-bg-subtle px-3 py-2 text-sm">
            <span className="font-medium text-fg-muted">Filtered by</span>
            {filter.account_code && (
              <Badge variant="accent">
                Account {filter.account_code}
                {accountName.get(filter.account_code)
                  ? ` · ${accountName.get(filter.account_code)}`
                  : ""}
              </Badge>
            )}
            {filter.from && (
              <Badge variant="neutral">
                From {fmtDate(filter.from)}
              </Badge>
            )}
            {filter.to && <Badge variant="neutral">To {fmtDate(filter.to)}</Badge>}
            {filter.source_ktype && (
              <Badge variant="neutral">
                Source {ktypeSingular(filter.source_ktype)}
              </Badge>
            )}
            <Button
              type="button"
              size="sm"
              variant="ghost"
              leadingIcon={<X aria-hidden />}
              onClick={() => setSearchParams({})}
              className="ml-auto"
            >
              Clear filters
            </Button>
          </div>
        )}
      </header>

      {q.isLoading && <TableSkeleton columns={4} rows={8} />}

      {q.isError && (
        <FinanceError
          title="Couldn't load journal entries"
          error={q.error}
          onRetry={() => void q.refetch()}
        />
      )}

      {q.data && entries.length === 0 && (
        <div className="rounded-lg border border-border p-8">
          <p className="text-center text-sm text-fg-muted">
            {hasFilter
              ? "No journal entries match the current filters. Try clearing them."
              : "No journal entries have been posted yet. Posting an invoice, bill, or payroll run will create entries here."}
          </p>
        </div>
      )}

      <div className="flex flex-col gap-4">
        {entries.map((e) => {
          const totals = entryTotals(e);
          const currency = e.lines[0]?.currency ?? "USD";
          return (
            <article
              key={e.id}
              className="overflow-hidden rounded-lg border border-border bg-bg-elevated"
            >
              <div className="flex flex-wrap items-start justify-between gap-2 border-b border-border px-4 py-3">
                <div className="min-w-0">
                  <h2 className="truncate text-sm font-semibold text-fg">
                    {e.memo || "Journal entry"}
                  </h2>
                  <p className="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs text-fg-muted">
                    <span>{fmtDate(e.posted_at)}</span>
                    <span aria-hidden>·</span>
                    <span>
                      {e.source_ktype && e.source_ktype !== "manual"
                        ? ktypeSingular(e.source_ktype)
                        : "Manual entry"}
                    </span>
                  </p>
                </div>
                <BalancedBadge balanced={totals.balanced} />
              </div>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Account</TableHead>
                    <TableHead className="text-right">Debit</TableHead>
                    <TableHead className="text-right">Credit</TableHead>
                    <TableHead>Memo</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {e.lines.map((l) => {
                    const highlighted =
                      filter.account_code &&
                      l.account_code === filter.account_code;
                    return (
                      <TableRow
                        key={l.id}
                        className={highlighted ? "bg-accent/10" : undefined}
                      >
                        <TableCell>
                          <Link
                            to={`/finance/journal?account_code=${encodeURIComponent(l.account_code)}`}
                            className="font-mono text-xs text-accent hover:underline focus-visible:underline focus-visible:outline-none"
                          >
                            {l.account_code}
                          </Link>
                          {accountName.get(l.account_code) && (
                            <span className="ml-2 text-fg">
                              {accountName.get(l.account_code)}
                            </span>
                          )}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {money(l.debit, { currency: l.currency, blankZero: true })}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {money(l.credit, { currency: l.currency, blankZero: true })}
                        </TableCell>
                        <TableCell className="text-fg-muted">
                          {l.memo || "—"}
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
                <TableFooter>
                  <TableRow>
                    <TableCell className="font-semibold">Totals</TableCell>
                    <TableCell className="text-right font-semibold tabular-nums">
                      {money(totals.debit, { currency })}
                    </TableCell>
                    <TableCell className="text-right font-semibold tabular-nums">
                      {money(totals.credit, { currency })}
                    </TableCell>
                    <TableCell>
                      {!totals.balanced && (
                        <span className="text-xs font-medium text-danger">
                          Debits and credits don't match
                        </span>
                      )}
                    </TableCell>
                  </TableRow>
                </TableFooter>
              </Table>
            </article>
          );
        })}
      </div>
    </section>
  );
}
