import { useQuery } from "@tanstack/react-query";
import type { FinanceAccount } from "@kapp/client";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
      <p className="text-fg-muted">
        Per-tenant account registry used for double-entry postings.
      </p>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load accounts: {(q.error as Error).message}
        </p>
      )}

      {q.data && q.data.length === 0 && (
        <p className="italic text-fg-subtle">
          No accounts yet. Create one via the finance.account KType.
        </p>
      )}

      {q.data &&
        (Object.keys(byType) as (keyof typeof byType)[]).map((type) => (
          <div key={type} className="mt-4">
            <h2 className="text-sm capitalize">{type}</h2>
            <Table className="text-[13px]">
              <TableHeader>
                <TableRow>
                  <TableHead>Code</TableHead>
                  <TableHead>Name</TableHead>
                  <TableHead>Parent</TableHead>
                  <TableHead>Active</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {byType[type].map((a) => (
                  <TableRow key={a.code}>
                    <TableCell>
                      <code>{a.code}</code>
                    </TableCell>
                    <TableCell>{a.name}</TableCell>
                    <TableCell>{a.parent_code ?? "—"}</TableCell>
                    <TableCell>{a.active ? "yes" : "no"}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
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

