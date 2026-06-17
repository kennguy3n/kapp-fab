import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Download, Search } from "lucide-react";
import type { AccountType, FinanceAccount } from "@kapp/client";
import {
  Badge,
  Button,
  Eyebrow,
  Field,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import { csvFilename, downloadCsv } from "../lib/finance/format";
import {
  AccountTypeBadge,
  accountTypeLabel,
  FinanceError,
  TableSkeleton,
} from "../lib/finance/presentation";

// Canonical statement ordering so the registry always reads
// assets → liabilities → equity → revenue → expenses.
const TYPE_ORDER: AccountType[] = [
  "asset",
  "liability",
  "equity",
  "revenue",
  "expense",
];

interface AccountNode {
  account: FinanceAccount;
  depth: number;
  children: AccountNode[];
}

/**
 * ChartOfAccountsPage is the per-tenant account registry. Accounts are
 * grouped by statement type and nested by parent so the hierarchy reads
 * at a glance; each code drills through to its journal activity. A
 * structure check flags any account whose parent is missing.
 */
export function ChartOfAccountsPage() {
  const [search, setSearch] = useState("");
  const q = useQuery({
    queryKey: ["finance", "accounts"],
    queryFn: () => api.listAccounts(),
  });

  const accounts = q.data ?? [];

  const { groups, activeCount, orphanCount } = useMemo(
    () => summarize(accounts),
    [accounts],
  );

  const term = search.trim().toLowerCase();
  const visibleGroups = useMemo(
    () =>
      groups
        .map((g) => {
          const forest = pruneForest(g.forest, term);
          // While searching, the group header must count the matches it
          // actually shows, not the unfiltered total.
          return { ...g, forest, count: term ? flatten(forest).length : g.count };
        })
        .filter((g) => g.forest.length > 0),
    [groups, term],
  );

  const exportCsv = () => {
    if (accounts.length === 0) return;
    const rows = [...accounts]
      .sort((a, b) => a.code.localeCompare(b.code))
      .map((a) => [
        a.code,
        a.name,
        accountTypeLabel(a.type),
        a.parent_code ?? "",
        a.active ? "Active" : "Inactive",
      ]);
    downloadCsv(
      csvFilename("chart-of-accounts"),
      ["Code", "Account", "Type", "Parent", "Status"],
      rows,
    );
    toast.success("Chart of accounts exported", {
      description: `${accounts.length} accounts.`,
    });
  };

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Finance</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Chart of Accounts
            </h1>
            <p className="mt-1 text-sm text-fg-muted">
              The account registry behind every double-entry posting,
              organised by statement type.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              leadingIcon={<Download aria-hidden />}
              onClick={exportCsv}
              disabled={accounts.length === 0}
            >
              Export CSV
            </Button>
          </div>
        </div>
        {accounts.length > 0 && (
          <div className="flex flex-wrap items-center gap-2 text-sm text-fg-muted">
            <Badge variant="neutral">{accounts.length} accounts</Badge>
            <Badge variant="info">{activeCount} active</Badge>
            {orphanCount === 0 ? (
              <Badge variant="success">Structure complete</Badge>
            ) : (
              <Badge variant="danger">
                {orphanCount} orphaned{" "}
                {orphanCount === 1 ? "account" : "accounts"}
              </Badge>
            )}
          </div>
        )}
        <div className="max-w-sm">
          <Field label="Search accounts" hideLabel>
            <Input
              type="search"
              placeholder="Search by code or name…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              leadingAddon={<Search aria-hidden className="h-4 w-4" />}
            />
          </Field>
        </div>
      </header>

      {q.isLoading && <TableSkeleton columns={3} />}

      {q.isError && (
        <FinanceError
          title="Couldn't load the chart of accounts"
          error={q.error}
          onRetry={() => void q.refetch()}
        />
      )}

      {q.data && accounts.length === 0 && (
        <div className="rounded-lg border border-border p-8">
          <p className="text-center text-sm text-fg-muted">
            No accounts yet. Accounts are created from the finance account
            setup; once added they'll appear here grouped by type.
          </p>
        </div>
      )}

      {q.data && accounts.length > 0 && visibleGroups.length === 0 && (
        <div className="rounded-lg border border-border p-8">
          <p className="text-center text-sm text-fg-muted">
            No accounts match “{search}”.
          </p>
        </div>
      )}

      {visibleGroups.map((g) => (
        <div key={g.type} className="flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <AccountTypeBadge type={g.type} />
            <span className="text-sm text-fg-muted">
              {g.count} {g.count === 1 ? "account" : "accounts"}
            </span>
          </div>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-28">Code</TableHead>
                <TableHead>Account</TableHead>
                <TableHead className="w-28 text-right">Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {flatten(g.forest).map(({ account, depth }) => (
                <TableRow key={account.code}>
                  <TableCell>
                    <Link
                      to={`/finance/journal?account_code=${encodeURIComponent(account.code)}`}
                      className="font-mono text-xs text-accent hover:underline focus-visible:underline focus-visible:outline-none"
                      title={`View journal entries for ${account.name}`}
                    >
                      {account.code}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <span
                      className="text-fg"
                      style={{ paddingInlineStart: `${depth * 1.25}rem` }}
                    >
                      {account.name}
                    </span>
                  </TableCell>
                  <TableCell className="text-right">
                    <Badge variant={account.active ? "success" : "outline"}>
                      {account.active ? "Active" : "Inactive"}
                    </Badge>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ))}
    </section>
  );
}

interface TypeGroup {
  type: AccountType;
  count: number;
  forest: AccountNode[];
}

function summarize(accounts: FinanceAccount[]): {
  groups: TypeGroup[];
  activeCount: number;
  orphanCount: number;
} {
  const codes = new Set(accounts.map((a) => a.code));
  let activeCount = 0;
  let orphanCount = 0;
  for (const a of accounts) {
    if (a.active) activeCount += 1;
    if (a.parent_code && !codes.has(a.parent_code)) orphanCount += 1;
  }

  const groups: TypeGroup[] = [];
  for (const type of TYPE_ORDER) {
    const ofType = accounts.filter((a) => a.type === type);
    if (ofType.length === 0) continue;
    groups.push({ type, count: ofType.length, forest: buildForest(ofType) });
  }
  return { groups, activeCount, orphanCount };
}

// Build a parent/child forest from a flat account list. A node roots
// when it has no parent or its parent lives outside this type group.
function buildForest(accounts: FinanceAccount[]): AccountNode[] {
  const byCode = new Map(accounts.map((a) => [a.code, a]));
  const childrenOf = new Map<string, FinanceAccount[]>();
  const roots: FinanceAccount[] = [];
  for (const a of accounts) {
    if (a.parent_code && byCode.has(a.parent_code)) {
      const list = childrenOf.get(a.parent_code) ?? [];
      list.push(a);
      childrenOf.set(a.parent_code, list);
    } else {
      roots.push(a);
    }
  }
  const byCodeAsc = (a: FinanceAccount, b: FinanceAccount) =>
    a.code.localeCompare(b.code);
  const toNode = (account: FinanceAccount, depth: number): AccountNode => ({
    account,
    depth,
    children: (childrenOf.get(account.code) ?? [])
      .sort(byCodeAsc)
      .map((child) => toNode(child, depth + 1)),
  });
  return roots.sort(byCodeAsc).map((r) => toNode(r, 0));
}

// Keep a node when it (or any descendant) matches the search term, so
// ancestors remain visible for hierarchy context.
function pruneForest(forest: AccountNode[], term: string): AccountNode[] {
  if (!term) return forest;
  const visit = (node: AccountNode): AccountNode | null => {
    const children = node.children
      .map(visit)
      .filter((c): c is AccountNode => c !== null);
    const selfMatch =
      node.account.code.toLowerCase().includes(term) ||
      node.account.name.toLowerCase().includes(term);
    if (selfMatch || children.length > 0) return { ...node, children };
    return null;
  };
  return forest.map(visit).filter((n): n is AccountNode => n !== null);
}

function flatten(forest: AccountNode[]): AccountNode[] {
  const out: AccountNode[] = [];
  const walk = (nodes: AccountNode[]) => {
    for (const n of nodes) {
      out.push(n);
      walk(n.children);
    }
  };
  walk(forest);
  return out;
}
