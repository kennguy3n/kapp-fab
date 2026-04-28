import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ConsolidatedTrialBalance, ConsolidationGroup } from "@kapp/client";
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
      <p style={{ color: "#6b7280" }}>
        Roll up trial balances across child tenants into a single presentation
        currency, eliminating inter-company balances. Admin only.
      </p>

      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 24 }}>
        <fieldset>
          <legend>Create group</legend>
          <div style={{ display: "grid", gap: 8 }}>
            <label>
              Name
              <input value={name} onChange={(e) => setName(e.target.value)} />
            </label>
            <label>
              Presentation currency
              <input
                value={currency}
                onChange={(e) => setCurrency(e.target.value.toUpperCase())}
                maxLength={3}
              />
            </label>
            <label>
              Member tenant IDs (comma-separated)
              <textarea
                value={members}
                onChange={(e) => setMembers(e.target.value)}
                rows={3}
              />
            </label>
            <button onClick={() => createMut.mutate()} disabled={createMut.isPending}>
              {createMut.isPending ? "Creating…" : "Create group"}
            </button>
            {createMut.error && (
              <span style={{ color: "#b91c1c" }}>{(createMut.error as Error).message}</span>
            )}
          </div>
        </fieldset>

        <fieldset>
          <legend>Run consolidation</legend>
          <div style={{ display: "grid", gap: 8 }}>
            <label>
              Group ID
              <input value={runGroup} onChange={(e) => setRunGroup(e.target.value)} />
            </label>
            <button
              onClick={() => runMut.mutate(runGroup)}
              disabled={!runGroup || runMut.isPending}
            >
              {runMut.isPending ? "Running…" : "Run"}
            </button>
            {runMut.error && (
              <span style={{ color: "#b91c1c" }}>{(runMut.error as Error).message}</span>
            )}
          </div>
        </fieldset>
      </div>

      {result && (
        <div style={{ marginTop: 24 }}>
          <h2>
            Consolidated trial balance — {result.presentation_currency} as of{" "}
            {new Date(result.as_of).toLocaleString()}
          </h2>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 14 }}>
            <thead>
              <tr style={{ background: "#f3f4f6" }}>
                <th style={th()}>Account</th>
                <th style={th()}>Debit</th>
                <th style={th()}>Credit</th>
                <th style={th()}>Balance</th>
              </tr>
            </thead>
            <tbody>
              {result.rows.map((row) => (
                <tr key={row.account_code}>
                  <td style={td()}>{row.account_code}</td>
                  <td style={tdRight()}>{row.debit}</td>
                  <td style={tdRight()}>{row.credit}</td>
                  <td style={tdRight()}>{row.balance}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr style={{ fontWeight: 600 }}>
                <td style={td()}>Total</td>
                <td style={tdRight()}>{result.total_debit}</td>
                <td style={tdRight()}>{result.total_credit}</td>
                <td style={tdRight()}></td>
              </tr>
            </tfoot>
          </table>

          {result.eliminated.length > 0 && (
            <div style={{ marginTop: 16 }}>
              <h3>Eliminated (inter-company)</h3>
              <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 14 }}>
                <thead>
                  <tr style={{ background: "#fef3c7" }}>
                    <th style={th()}>Account</th>
                    <th style={th()}>Debit</th>
                    <th style={th()}>Credit</th>
                  </tr>
                </thead>
                <tbody>
                  {result.eliminated.map((row) => (
                    <tr key={row.account_code}>
                      <td style={td()}>{row.account_code}</td>
                      <td style={tdRight()}>{row.debit}</td>
                      <td style={tdRight()}>{row.credit}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </section>
  );
}

function th(): React.CSSProperties {
  return { textAlign: "left", padding: 6, borderBottom: "1px solid #e5e7eb" };
}
function td(): React.CSSProperties {
  return { padding: 6, borderBottom: "1px solid #f3f4f6" };
}
function tdRight(): React.CSSProperties {
  return { ...td(), textAlign: "right", fontVariantNumeric: "tabular-nums" };
}
