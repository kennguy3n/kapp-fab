import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";

/**
 * JournalEntriesPage lists posted journal entries with their lines,
 * source linkage, and running totals. Read-only — new JEs are posted
 * via /finance/journal-entries (used by invoice/bill posting flows).
 */
export function JournalEntriesPage() {
  const q = useQuery({
    queryKey: ["finance", "journal-entries"],
    queryFn: () => api.listJournalEntries(),
  });

  const entries = q.data ?? [];

  return (
    <section>
      <h1>Journal Entries</h1>
      <p style={{ color: "#6b7280" }}>
        Posted double-entry journal transactions. Every entry is balanced
        (total debits equal total credits).
      </p>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load entries: {(q.error as Error).message}
        </p>
      )}

      {q.data && entries.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No journal entries yet.
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
                <tr key={l.id} style={{ borderTop: "1px solid #f3f4f6" }}>
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
