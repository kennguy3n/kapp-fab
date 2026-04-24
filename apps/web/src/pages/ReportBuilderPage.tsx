import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReportDefinition, ReportResult, SavedReport } from "@kapp/client";
import { api } from "../lib/api";

const BLANK_DEFINITION: ReportDefinition = {
  source: "ktype:crm.deal",
  columns: ["id", "data.name", "data.stage", "data.value"],
  filters: [],
  group_by: [],
  aggregations: [],
  sort: [{ column: "data.value", direction: "desc" }],
  limit: 100,
};

/**
 * ReportBuilderPage exposes the metadata-driven report grammar
 * (data source, columns, filters, group-by, aggregations, pivot,
 * chart) via a JSON editor and a run button. The runner validates
 * the definition server-side before emitting SQL so a bad definition
 * fails fast with a 400. Saved reports persist the definition so
 * dashboards and scheduled exports can replay them.
 */
export function ReportBuilderPage() {
  const qc = useQueryClient();
  const saved = useQuery<{ reports: SavedReport[] }>({
    queryKey: ["reports"],
    queryFn: () => api.listReports(),
  });

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [rawDef, setRawDef] = useState(JSON.stringify(BLANK_DEFINITION, null, 2));
  const [result, setResult] = useState<ReportResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  const runMutation = useMutation({
    mutationFn: (def: ReportDefinition) => api.runAdhocReport(def),
    onSuccess: (res) => {
      setResult(res);
      setError(null);
    },
    onError: (err: Error) => {
      setError(err.message);
      setResult(null);
    },
  });

  const createMutation = useMutation({
    mutationFn: () => {
      const def = parseDef();
      return api.createReport({ name, description, definition: def });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["reports"] }),
    onError: (err: Error) => setError(err.message),
  });

  const parseDef = (): ReportDefinition => {
    const parsed = JSON.parse(rawDef) as ReportDefinition;
    return parsed;
  };

  const run = () => {
    try {
      const def = parseDef();
      runMutation.mutate(def);
    } catch (e) {
      setError(`Invalid JSON: ${(e as Error).message}`);
    }
  };

  const loadSaved = (r: SavedReport) => {
    setName(r.name);
    setDescription(r.description ?? "");
    setRawDef(JSON.stringify(r.definition, null, 2));
  };

  return (
    <section>
      <h1>Report Builder</h1>
      <p style={{ color: "#6b7280" }}>
        Define a report with columns / filters / group-by / aggregations
        over any KType or ledger table. Hit Run to preview, Save to
        persist the definition for dashboards and scheduled exports.
      </p>

      <div style={{ display: "flex", gap: 16 }}>
        <aside style={{ flex: "0 0 220px" }}>
          <h3 style={{ fontSize: 14 }}>Saved reports</h3>
          {saved.isLoading && <p>Loading…</p>}
          {(saved.data?.reports ?? []).length === 0 && !saved.isLoading && (
            <p style={{ color: "#9ca3af", fontStyle: "italic", fontSize: 13 }}>
              No saved reports yet.
            </p>
          )}
          <ul style={{ listStyle: "none", padding: 0, margin: 0, fontSize: 13 }}>
            {(saved.data?.reports ?? []).map((r) => (
              <li key={r.id} style={{ padding: "4px 0" }}>
                <button
                  onClick={() => loadSaved(r)}
                  style={{
                    background: "none",
                    border: "none",
                    color: "#2563eb",
                    cursor: "pointer",
                    padding: 0,
                  }}
                >
                  {r.name}
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div style={{ flex: 1 }}>
          <div style={{ display: "flex", gap: 8, marginBottom: 8, fontSize: 13 }}>
            <input
              placeholder="report name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              style={{ flex: 1 }}
            />
            <input
              placeholder="description (optional)"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              style={{ flex: 2 }}
            />
          </div>
          <textarea
            value={rawDef}
            onChange={(e) => setRawDef(e.target.value)}
            spellCheck={false}
            style={{
              width: "100%",
              minHeight: 240,
              fontFamily: "monospace",
              fontSize: 12,
              padding: 8,
            }}
          />
          <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
            <button onClick={run} disabled={runMutation.isPending}>
              {runMutation.isPending ? "Running…" : "Run"}
            </button>
            <button
              onClick={() => createMutation.mutate()}
              disabled={!name || createMutation.isPending}
            >
              {createMutation.isPending ? "Saving…" : "Save report"}
            </button>
          </div>
          {error && (
            <p style={{ color: "#b91c1c", fontSize: 13, marginTop: 8 }}>
              {error}
            </p>
          )}

          {result && (
            <div style={{ marginTop: 16 }}>
              <h3 style={{ fontSize: 14 }}>
                Result ({result.rows.length} rows)
              </h3>
              <div style={{ overflow: "auto", maxHeight: 360 }}>
                <table style={{ fontSize: 12, borderCollapse: "collapse" }}>
                  <thead>
                    <tr style={{ borderBottom: "1px solid #e5e7eb" }}>
                      {result.columns.map((c) => (
                        <th key={c} style={{ padding: "2px 8px", textAlign: "left" }}>
                          {c}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {result.rows.slice(0, 500).map((row, i) => (
                      <tr key={i} style={{ borderBottom: "1px solid #f3f4f6" }}>
                        {row.map((cell, j) => (
                          <td key={j} style={{ padding: "2px 8px" }}>
                            {formatCell(cell)}
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

function formatCell(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
