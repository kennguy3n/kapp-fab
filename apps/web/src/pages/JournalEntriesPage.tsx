import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
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
      <p style={{ color: "#6b7280" }}>
        Posted double-entry journal transactions. Every entry is balanced
        (total debits equal total credits).
      </p>

      {hasFilter && (
        <div
          style={{
            background: "#fef3c7",
            border: "1px solid #fcd34d",
            color: "#78350f",
            padding: "8px 12px",
            borderRadius: 4,
            fontSize: 13,
            marginBottom: 12,
            display: "flex",
            gap: 12,
            alignItems: "center",
            flexWrap: "wrap",
          }}
        >
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
          <button
            type="button"
            onClick={() => setSearchParams({})}
            style={{
              marginLeft: "auto",
              padding: "4px 10px",
              border: "1px solid #fcd34d",
              background: "#fde68a",
              borderRadius: 4,
              cursor: "pointer",
              fontSize: 12,
            }}
          >
            Clear filters
          </button>
        </div>
      )}

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load entries: {(q.error as Error).message}
        </p>
      )}

      {q.data && entries.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          {hasFilter
            ? "No journal entries match the current filters."
            : "No journal entries yet."}
        </p>
      )}

      {entries.map((e) => (
        <div
          key={e.id}
          style={{
            marginTop: 16,
            padding: 12,
            border: "1px solid #e5e7eb",
            borderRadius: 4,
          }}
        >
          <div
            style={{
              display: "flex",
              justifyContent: "space-between",
              marginBottom: 6,
              fontSize: 13,
            }}
          >
            <div>
              <code>{e.id.slice(0, 8)}</code> — {e.memo || "(no memo)"}
            </div>
            <div style={{ color: "#6b7280" }}>
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
          <table
            style={{ width: "100%", borderCollapse: "collapse", fontSize: 12 }}
          >
            <thead>
              <tr style={{ textAlign: "left", color: "#6b7280" }}>
                <Th>Account</Th>
                <Th>Debit</Th>
                <Th>Credit</Th>
                <Th>Memo</Th>
              </tr>
            </thead>
            <tbody>
              {e.lines.map((l) => (
                <tr
                  key={l.id}
                  style={{
                    borderTop: "1px solid #f3f4f6",
                    // Highlight the line(s) that match the
                    // active account_code filter so the user can
                    // immediately see why the entry surfaced.
                    background:
                      filter.account_code &&
                      l.account_code === filter.account_code
                        ? "#fef3c7"
                        : undefined,
                  }}
                >
                  <Td>
                    <code>{l.account_code}</code>
                  </Td>
                  <Td>{amount(l.debit, l.currency)}</Td>
                  <Td>{amount(l.credit, l.currency)}</Td>
                  <Td>{l.memo || "—"}</Td>
                </tr>
              ))}
            </tbody>
          </table>
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

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th style={{ padding: "6px 8px", fontWeight: 500 }}>{children}</th>
  );
}

function Td({ children }: { children: React.ReactNode }) {
  return <td style={{ padding: "6px 8px" }}>{children}</td>;
}
