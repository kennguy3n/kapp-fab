import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import {
  Button,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * JournalEntriesPage lists posted journal entries with their lines,
 * source linkage, and running totals. Read-only — new JEs are posted
 * via the invoice/bill/payroll posting flows.
 *
 * Query-param filters (forwarded to GET /finance/journal-entries so
 * the row-set is narrowed server-side, not client-side):
 *
 *   - `account_code`: include only entries with at least one line on
 *     this account. Used by the BudgetPage variance drill-down.
 *   - `from` / `to`: posted_at RFC3339 lower / upper bounds. Also
 *     populated by the BudgetPage drill-down (calendar-month
 *     window of the variance row).
 *   - `source_ktype` / `source_id`: lookup by the document that
 *     posted the entry (e.g. `finance.ar_invoice` + invoice id).
 *
 * When any filter is active, a small banner above the list shows the
 * active filters and offers a one-click reset to clear them.
 */
export function JournalEntriesPage() {
  const [searchParams, setSearchParams] = useSearchParams();

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

  const entries = q.data ?? [];

  return (
    <section>
      <h1>Journal Entries</h1>
      <p className="text-fg-muted">
        Posted double-entry journal transactions. Every entry is balanced
        (total debits equal total credits).
      </p>

      {hasFilter && (
        <div className="mb-3 flex flex-wrap items-center gap-3 rounded border border-warning bg-warning/15 px-3 py-2 text-[13px] text-warning">
          <strong>Filtered:</strong>
          {filter.account_code && (
            <span>
              account <code>{filter.account_code}</code>
            </span>
          )}
          {filter.from && (
            <span>
              from <code>{filter.from.slice(0, 10)}</code>
            </span>
          )}
          {filter.to && (
            <span>
              to <code>{filter.to.slice(0, 10)}</code>
            </span>
          )}
          {filter.source_ktype && (
            <span>
              source <code>{filter.source_ktype}</code>
            </span>
          )}
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => setSearchParams({})}
            className="ml-auto"
          >
            Clear filters
          </Button>
        </div>
      )}

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load entries: {(q.error as Error).message}
        </p>
      )}

      {q.data && entries.length === 0 && (
        <p className="italic text-fg-subtle">
          {hasFilter
            ? "No journal entries match the current filters."
            : "No journal entries yet."}
        </p>
      )}

      {entries.map((e) => (
        <div
          key={e.id}
          className="mt-4 rounded border border-border p-3"
        >
          <div className="mb-1.5 flex justify-between text-[13px]">
            <div>
              <code>{e.id.slice(0, 8)}</code> — {e.memo || "(no memo)"}
            </div>
            <div className="text-fg-muted">
              {e.source_ktype ? (
                <>
                  src: <code>{e.source_ktype}</code>
                </>
              ) : (
                "manual"
              )}{" "}
              · {formatDate(e.posted_at)}
            </div>
          </div>
          <Table className="text-xs">
            <TableHeader>
              <TableRow>
                <TableHead>Account</TableHead>
                <TableHead>Debit</TableHead>
                <TableHead>Credit</TableHead>
                <TableHead>Memo</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {e.lines.map((l) => (
                <TableRow
                  key={l.id}
                  // Highlight the line(s) that match the active
                  // account_code filter so the user can immediately
                  // see why the entry surfaced.
                  className={
                    filter.account_code &&
                    l.account_code === filter.account_code
                      ? "bg-warning/15"
                      : undefined
                  }
                >
                  <TableCell>
                    <code>{l.account_code}</code>
                  </TableCell>
                  <TableCell>{amount(l.debit, l.currency)}</TableCell>
                  <TableCell>{amount(l.credit, l.currency)}</TableCell>
                  <TableCell>{l.memo || "—"}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ))}
    </section>
  );
}

function amount(value: string, currency: string): string {
  if (!value || value === "0" || Number(value) === 0) return "—";
  return `${Number(value).toFixed(2)} ${currency}`;
}

function formatDate(iso: string): string {
  if (!iso) return "";
  return iso.slice(0, 10);
}


