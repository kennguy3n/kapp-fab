import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReportDefinition, ReportResult, SavedReport } from "@kapp/client";
import {
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

const BLANK_DEFINITION: ReportDefinition = {
  source: "ktype:crm.deal",
  columns: ["id", "name", "stage", "value"],
  filters: [],
  group_by: [],
  aggregations: [],
  sort: [{ column: "value", direction: "desc" }],
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
      <p className="text-fg-muted">
        Define a report with columns / filters / group-by / aggregations
        over any KType or ledger table. Hit Run to preview, Save to
        persist the definition for dashboards and scheduled exports.
      </p>

      <div className="flex gap-4">
        <aside className="flex-[0_0_220px]">
          <h3 className="text-sm">Saved reports</h3>
          {saved.isLoading && <p>Loading…</p>}
          {(saved.data?.reports ?? []).length === 0 && !saved.isLoading && (
            <p className="text-[13px] italic text-fg-subtle">
              No saved reports yet.
            </p>
          )}
          <ul className="m-0 list-none p-0 text-[13px]">
            {(saved.data?.reports ?? []).map((r) => (
              <li key={r.id} className="py-1">
                <Button
                  variant="link"
                  size="sm"
                  className="h-auto p-0"
                  onClick={() => loadSaved(r)}
                >
                  {r.name}
                </Button>
              </li>
            ))}
          </ul>
        </aside>

        <div className="flex-1">
          <div className="mb-2 flex gap-2 text-[13px]">
            <Input
              placeholder="report name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="flex-1"
            />
            <Input
              placeholder="description (optional)"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              className="flex-[2]"
            />
          </div>
          <textarea
            value={rawDef}
            onChange={(e) => setRawDef(e.target.value)}
            spellCheck={false}
            className="min-h-60 w-full rounded-md border border-border bg-bg p-2 font-mono text-xs text-fg outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
          />
          <div className="mt-2 flex gap-2">
            <Button onClick={run} disabled={runMutation.isPending}>
              {runMutation.isPending ? "Running…" : "Run"}
            </Button>
            <Button
              variant="outline"
              onClick={() => createMutation.mutate()}
              disabled={!name || createMutation.isPending}
            >
              {createMutation.isPending ? "Saving…" : "Save report"}
            </Button>
          </div>
          {error && (
            <p className="mt-2 text-[13px] text-danger">
              {error}
            </p>
          )}

          {result && (
            <div className="mt-4">
              <h3 className="text-sm">
                Result ({result.rows.length} rows)
              </h3>
              <div className="max-h-90 overflow-auto">
                <Table className="text-xs">
                  <TableHeader>
                    <TableRow>
                      {result.columns.map((c) => (
                        <TableHead key={c}>{c}</TableHead>
                      ))}
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {result.rows.slice(0, 500).map((row, i) => (
                      <TableRow key={i}>
                        {result.columns.map((col) => (
                          <TableCell key={col}>
                            {formatCell(row[col])}
                          </TableCell>
                        ))}
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
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
