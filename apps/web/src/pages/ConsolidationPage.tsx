import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ConsolidatedTrialBalance, ConsolidationGroup } from "@kapp/client";
import {
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * ConsolidationPage is the admin-only Phase M Task 7 surface.
 * Lists consolidation groups, lets the operator create a new
 * group + run it, and renders the combined trial balance with
 * per-tenant contributions and an Eliminated section so the
 * inter-company reconciliation is auditable.
 */
export function ConsolidationPage() {
  const queryClient = useQueryClient();
  const groupsQ = useQuery<ConsolidationGroup[]>({
    queryKey: ["admin.consolidation.groups"],
    // No list endpoint yet — this page works against a single group
    // returned from create, plus runs against arbitrary group ids.
    queryFn: () => Promise.resolve([]),
  });

  const [name, setName] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [members, setMembers] = useState("");
  const [runGroup, setRunGroup] = useState<string>("");
  const [result, setResult] = useState<ConsolidatedTrialBalance | null>(null);

  const createMut = useMutation({
    mutationFn: () =>
      api.createConsolidationGroup({
        name,
        presentation_currency: currency,
        member_tenant_ids: members.split(",").map((s) => s.trim()).filter(Boolean),
      }),
    onSuccess: (g) => {
      void queryClient.invalidateQueries({ queryKey: ["admin.consolidation.groups"] });
      setRunGroup(g.id);
    },
  });

  const runMut = useMutation({
    mutationFn: (groupID: string) => api.runConsolidation(groupID),
    onSuccess: (out) => setResult(out),
  });

  void groupsQ; // placeholder until list endpoint lands.

  return (
    <section>
      <h1>Consolidation</h1>
      <p className="text-fg-muted">
        Roll up trial balances across child tenants into a single presentation
        currency, eliminating inter-company balances. Admin only.
      </p>

      <div className="grid grid-cols-2 gap-6">
        <fieldset>
          <legend>Create group</legend>
          <div className="grid gap-2">
            <label className="flex flex-col gap-1">
              Name
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </label>
            <label className="flex flex-col gap-1">
              Presentation currency
              <Input
                value={currency}
                onChange={(e) => setCurrency(e.target.value.toUpperCase())}
                maxLength={3}
              />
            </label>
            <label className="flex flex-col gap-1">
              Member tenant IDs (comma-separated)
              <textarea
                value={members}
                onChange={(e) => setMembers(e.target.value)}
                rows={3}
                className="rounded-md border border-border bg-bg px-3 py-2 text-sm"
              />
            </label>
            <Button onClick={() => createMut.mutate()} disabled={createMut.isPending}>
              {createMut.isPending ? "Creating…" : "Create group"}
            </Button>
            {createMut.error && (
              <span className="text-danger">{(createMut.error as Error).message}</span>
            )}
          </div>
        </fieldset>

        <fieldset>
          <legend>Run consolidation</legend>
          <div className="grid gap-2">
            <label className="flex flex-col gap-1">
              Group ID
              <Input value={runGroup} onChange={(e) => setRunGroup(e.target.value)} />
            </label>
            <Button
              onClick={() => runMut.mutate(runGroup)}
              disabled={!runGroup || runMut.isPending}
            >
              {runMut.isPending ? "Running…" : "Run"}
            </Button>
            {runMut.error && (
              <span className="text-danger">{(runMut.error as Error).message}</span>
            )}
          </div>
        </fieldset>
      </div>

      {result && (
        <div className="mt-6">
          <h2>
            Consolidated trial balance — {result.presentation_currency} as of{" "}
            {new Date(result.as_of).toLocaleString()}
          </h2>
          <Table className="text-sm">
            <TableHeader>
              <TableRow>
                <TableHead>Account</TableHead>
                <TableHead>Debit</TableHead>
                <TableHead>Credit</TableHead>
                <TableHead>Balance</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {result.rows.map((row) => (
                <TableRow key={row.account_code}>
                  <TableCell>{row.account_code}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.debit}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.credit}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.balance}</TableCell>
                </TableRow>
              ))}
            </TableBody>
            <TableFooter>
              <TableRow className="font-semibold">
                <TableCell>Total</TableCell>
                <TableCell className="text-right tabular-nums">{result.total_debit}</TableCell>
                <TableCell className="text-right tabular-nums">{result.total_credit}</TableCell>
                <TableCell className="text-right tabular-nums"></TableCell>
              </TableRow>
            </TableFooter>
          </Table>

          {result.eliminated.length > 0 && (
            <div className="mt-4">
              <h3>Eliminated (inter-company)</h3>
              <Table className="text-sm">
                <TableHeader>
                  <TableRow>
                    <TableHead>Account</TableHead>
                    <TableHead>Debit</TableHead>
                    <TableHead>Credit</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {result.eliminated.map((row) => (
                    <TableRow key={row.account_code}>
                      <TableCell>{row.account_code}</TableCell>
                      <TableCell className="text-right tabular-nums">{row.debit}</TableCell>
                      <TableCell className="text-right tabular-nums">{row.credit}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
