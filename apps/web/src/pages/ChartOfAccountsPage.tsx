import { useQuery } from "@tanstack/react-query";
import type { FinanceAccount } from "@kapp/client";
import { api } from "../lib/api";

/**
 * ChartOfAccountsPage lists every active + inactive account grouped by
 * account type. Read-only for now; creation happens via the KType
 * form for finance.account.
 */
export function ChartOfAccountsPage() {
  const q = useQuery({
    queryKey: ["finance", "accounts"],
    queryFn: () => api.listAccounts(),
  });

  const byType = groupByType(q.data ?? []);

  return (
    <section>
      <h1>Chart of Accounts</h1>
      <p style={{ color: "#6b7280" }}>
        Per-tenant account registry used for double-entry postings.
      </p>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load accounts: {(q.error as Error).message}
        </p>
      )}

      {q.data && q.data.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No accounts yet. Create one via the finance.account KType.
        </p>
      )}

      {q.data &&
        (Object.keys(byType) as (keyof typeof byType)[]).map((type) => (
          <div key={type} style={{ marginTop: 16 }}>
            <h2 style={{ fontSize: 14, textTransform: "capitalize" }}>{type}</h2>
            <table
              style={{
                width: "100%",
                borderCollapse: "collapse",
                fontSize: 13,
              }}
            >
              <thead>
                <tr style={{ textAlign: "left", color: "#6b7280" }}>
                  <Th>Code</Th>
                  <Th>Name</Th>
                  <Th>Parent</Th>
                  <Th>Active</Th>
                </tr>
              </thead>
              <tbody>
                {byType[type].map((a) => (
                  <tr key={a.code} style={{ borderTop: "1px solid #e5e7eb" }}>
                    <Td>
                      <code>{a.code}</code>
                    </Td>
                    <Td>{a.name}</Td>
                    <Td>{a.parent_code ?? "—"}</Td>
                    <Td>{a.active ? "yes" : "no"}</Td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </section>
  );
}

function groupByType(accounts: FinanceAccount[]): Record<string, FinanceAccount[]> {
  const out: Record<string, FinanceAccount[]> = {};
  for (const a of accounts) {
    (out[a.type] ??= []).push(a);
  }
  for (const type of Object.keys(out)) {
    out[type].sort((x, y) => (x.code < y.code ? -1 : x.code > y.code ? 1 : 0));
  }
  return out;
}

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th style={{ padding: "6px 8px", fontWeight: 500, fontSize: 12 }}>
      {children}
    </th>
  );
}

function Td({ children }: { children: React.ReactNode }) {
  return <td style={{ padding: "8px" }}>{children}</td>;
}
