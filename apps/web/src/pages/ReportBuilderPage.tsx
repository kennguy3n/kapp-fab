import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download, Play, Save } from "lucide-react";
import type { ReportDefinition, ReportResult, SavedReport } from "@kapp/client";
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
import { useFormatter } from "../lib/i18n";
import { csvFilename, downloadCsv } from "../lib/finance/format";
import { FinanceError } from "../lib/finance/presentation";

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
 * A preset is a plain-language starting point for non-accountants: a
 * friendly title + one-line explanation that seeds the editor with a
 * working definition they can run immediately or tweak.
 */
interface ReportPreset {
  id: string;
  title: string;
  description: string;
  definition: ReportDefinition;
}

const PRESETS: ReportPreset[] = [
  {
    id: "deals-by-stage",
    title: "Sales pipeline by stage",
    description: "Total deal value grouped by pipeline stage.",
    definition: {
      source: "ktype:crm.deal",
      columns: ["stage", "value"],
      filters: [],
      group_by: ["stage"],
      aggregations: [{ column: "value", op: "sum" }],
      sort: [{ column: "value", direction: "desc" }],
      limit: 100,
    },
  },
  {
    id: "expenses-by-account",
    title: "Expenses by account",
    description: "Posted expense totals from the general ledger.",
    definition: {
      source: "table:finance.journal_lines",
      columns: ["account_code", "debit"],
      filters: [],
      group_by: ["account_code"],
      aggregations: [{ column: "debit", op: "sum" }],
      sort: [{ column: "debit", direction: "desc" }],
      limit: 100,
    },
  },
  {
    id: "open-invoices",
    title: "Open customer invoices",
    description: "Unpaid AR invoices with their balances due.",
    definition: {
      source: "ktype:finance.ar_invoice",
      columns: ["number", "customer_id", "total", "status"],
      filters: [{ column: "status", op: "neq", value: "paid" }],
      group_by: [],
      aggregations: [],
      sort: [{ column: "total", direction: "desc" }],
      limit: 100,
    },
  },
];

/**
 * ReportBuilderPage exposes the metadata-driven report grammar
 * (data source, columns, filters, group-by, aggregations, pivot,
 * chart) via plain-language presets plus an advanced JSON editor and
 * a run button. The runner validates the definition server-side
 * before emitting SQL so a bad definition fails fast with a 400.
 * Saved reports persist the definition so dashboards and scheduled
 * exports can replay them.
 */
export function ReportBuilderPage() {
  const qc = useQueryClient();
  const f = useFormatter();
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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["reports"] });
      toast.success("Report saved");
    },
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

  const save = (e: FormEvent) => {
    e.preventDefault();
    if (!name.trim()) return;
    createMutation.mutate();
  };

  const loadSaved = (r: SavedReport) => {
    setName(r.name);
    setDescription(r.description ?? "");
    setRawDef(JSON.stringify(r.definition, null, 2));
  };

  const loadPreset = (p: ReportPreset) => {
    setName(p.title);
    setDescription(p.description);
    setRawDef(JSON.stringify(p.definition, null, 2));
    setError(null);
    setResult(null);
  };

  // A column renders right-aligned with tabular figures only when
  // every value present in it is numeric, so mixed/text columns stay
  // left-aligned.
  const numericColumns = useMemo(() => {
    const set = new Set<string>();
    if (!result) return set;
    for (const col of result.columns) {
      let sawValue = false;
      let allNumeric = true;
      for (const row of result.rows) {
        const v = row[col];
        if (v === null || v === undefined) continue;
        sawValue = true;
        if (typeof v !== "number" || !Number.isFinite(v)) {
          allNumeric = false;
          break;
        }
      }
      if (sawValue && allNumeric) set.add(col);
    }
    return set;
  }, [result]);

  const exportResult = () => {
    if (!result) return;
    downloadCsv(
      csvFilename(name.trim() ? slug(name) : "report"),
      result.columns,
      result.rows.map((row) => result.columns.map((c) => formatCell(row[c]))),
    );
    toast.success("Report exported");
  };

  const reports = saved.data?.reports ?? [];

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-1">
        <Eyebrow>Reports</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Report Builder
        </h1>
        <p className="text-sm text-fg-muted">
          Start from a template or build your own report across any record
          type or ledger table. Preview the results, then save it for
          dashboards and scheduled exports.
        </p>
      </header>

      <div className="flex flex-col gap-6 lg:flex-row lg:items-start">
        {/* ---------- Saved reports ----------- */}
        <aside className="flex w-full flex-col gap-2 lg:max-w-[220px]">
          <h2 className="text-sm font-semibold text-fg">Saved reports</h2>
          {saved.isLoading && (
            <p className="text-sm text-fg-muted">Loading…</p>
          )}
          {saved.isError && (
            <FinanceError
              title="Couldn't load saved reports"
              error={saved.error}
              onRetry={() => void saved.refetch()}
            />
          )}
          {!saved.isLoading && !saved.isError && reports.length === 0 && (
            <p className="text-sm italic text-fg-subtle">
              No saved reports yet.
            </p>
          )}
          {reports.length > 0 && (
            <ul className="m-0 flex list-none flex-col gap-0.5 p-0">
              {reports.map((r) => (
                <li key={r.id}>
                  <Button
                    variant="link"
                    size="sm"
                    className="h-auto p-0 text-left"
                    onClick={() => loadSaved(r)}
                  >
                    {r.name}
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </aside>

        {/* ---------- Builder ----------- */}
        <div className="min-w-0 flex-1 flex-col gap-4">
          <div className="flex flex-col gap-2">
            <h2 className="text-sm font-semibold text-fg">
              Start from a template
            </h2>
            <div className="flex flex-wrap gap-2">
              {PRESETS.map((p) => (
                <Button
                  key={p.id}
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => loadPreset(p)}
                  title={p.description}
                >
                  {p.title}
                </Button>
              ))}
            </div>
          </div>

          <form onSubmit={save} className="mt-4 flex flex-col gap-3" noValidate>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Report name" required>
                <Input
                  placeholder="report name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </Field>
              <Field label="Description" help="Optional — shown next to the report.">
                <Input
                  placeholder="description (optional)"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                />
              </Field>
            </div>

            <div className="flex flex-col gap-1.5">
              <label
                htmlFor="report-definition"
                className="text-sm font-medium text-fg"
              >
                Definition{" "}
                <span className="font-normal text-fg-muted">(advanced)</span>
              </label>
              <textarea
                id="report-definition"
                value={rawDef}
                onChange={(e) => setRawDef(e.target.value)}
                spellCheck={false}
                aria-label="Report definition JSON"
                className="min-h-60 w-full rounded-md border border-border bg-bg p-2 font-mono text-xs text-fg outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
              />
              <p className="text-xs text-fg-muted">
                The data source, columns, filters, grouping and sort as JSON.
                Templates above fill this in for you.
              </p>
            </div>

            <div className="flex flex-wrap gap-2">
              <Button
                type="button"
                onClick={run}
                disabled={runMutation.isPending}
                leadingIcon={<Play aria-hidden />}
              >
                {runMutation.isPending ? "Running…" : "Run"}
              </Button>
              <Button
                type="submit"
                variant="outline"
                disabled={!name.trim() || createMutation.isPending}
                leadingIcon={<Save aria-hidden />}
              >
                {createMutation.isPending ? "Saving…" : "Save report"}
              </Button>
            </div>
          </form>

          {error && (
            <p
              role="alert"
              className="mt-3 rounded-md border border-danger/40 bg-danger/10 p-2 text-[13px] text-danger"
            >
              {error}
            </p>
          )}

          {result && (
            <div className="mt-4 flex flex-col gap-2">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <h2 className="flex items-center gap-2 text-sm font-semibold text-fg">
                  Result ({result.rows.length} rows)
                  <Badge variant="neutral" size="xs">
                    {result.columns.length} columns
                  </Badge>
                </h2>
                <Button
                  variant="secondary"
                  size="sm"
                  leadingIcon={<Download aria-hidden />}
                  onClick={exportResult}
                  disabled={result.rows.length === 0}
                >
                  Export CSV
                </Button>
              </div>
              {result.rows.length === 0 ? (
                <div className="rounded-lg border border-border p-8">
                  <p className="text-center text-sm text-fg-muted">
                    This report ran successfully but returned no rows. Try
                    widening the date range or relaxing a filter.
                  </p>
                </div>
              ) : (
                <div className="max-h-[36rem] overflow-auto rounded-lg border border-border">
                  <Table className="text-xs">
                    <TableHeader>
                      <TableRow>
                        {result.columns.map((c) => (
                          <TableHead
                            key={c}
                            className={
                              numericColumns.has(c) ? "text-right" : "text-left"
                            }
                          >
                            {c}
                          </TableHead>
                        ))}
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {result.rows.slice(0, 500).map((row, i) => (
                        <TableRow key={i}>
                          {result.columns.map((col) => {
                            const numeric = numericColumns.has(col);
                            const v = row[col];
                            return (
                              <TableCell
                                key={col}
                                className={
                                  numeric
                                    ? "text-right font-tabular tabular-nums"
                                    : "text-left"
                                }
                              >
                                {numeric && typeof v === "number"
                                  ? f.number(v)
                                  : formatCell(v)}
                              </TableCell>
                            );
                          })}
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}
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

function slug(s: string): string {
  return (
    s
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "_")
      .replace(/^_+|_+$/g, "") || "report"
  );
}
