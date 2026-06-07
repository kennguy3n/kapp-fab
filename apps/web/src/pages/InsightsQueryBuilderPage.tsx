// Phase L Insights — visual query builder.
//
// Composes a saved insights query (extends reporting.Definition with
// calculated columns) without dropping into JSON editing. Source picker
// covers both KType-backed sources (`ktype:<name>`) and the canonical
// ledger / inventory tables. Filters / aggregations / calculated
// columns are added through structured controls, and the live preview
// hits POST /api/v1/insights/queries/{id}/run after a Save.

import { useEffect, useMemo, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import type {
  CalculatedColumn,
  InsightsQuery,
  InsightsQueryDefinition,
  InsightsQueryMode,
  InsightsRunResult,
  InsightsVizType,
  KType,
  ReportAggregation,
  ReportFilter,
  ReportSort,
} from "@kapp/client";
import {
  Button,
  ConfirmDialog,
  Input,
  Select,
  cn,
} from "@kapp/ui";
import { api } from "../lib/api";
import { Viz } from "../components/insights/Charts";
import { ShareModal } from "../components/insights/ShareModal";

// Curated list of non-KType ledger / inventory / helpdesk tables the
// reporting runner will accept as a `source`. Mirrors
// internal/reporting.AllowedTables — kept short on purpose; the more
// exotic surfaces are reachable via the JSON-editor escape hatch.
const LEDGER_SOURCES = [
  "journal_entries",
  "journal_lines",
  "ar_invoices",
  "ap_bills",
  "payments",
  "accounts",
  "fiscal_periods",
  "inventory_moves",
  "stock_levels",
  "ticket_sla_log",
];

const VIZ_OPTIONS: InsightsVizType[] = [
  "table",
  "bar",
  "line",
  "pie",
  "donut",
  "funnel",
  "number_card",
  "pivot",
];

const FILTER_OPS = ["=", "!=", ">", ">=", "<", "<=", "in", "not_in", "ilike"];

const AGG_OPS: ReportAggregation["op"][] = [
  "count",
  "sum",
  "avg",
  "min",
  "max",
];

interface QueryFormState {
  name: string;
  description: string;
  source: string;
  columns: string[];
  filters: ReportFilter[];
  group_by: string[];
  aggregations: ReportAggregation[];
  sort: ReportSort[];
  limit: number;
  calculated_columns: CalculatedColumn[];
  cache_ttl_seconds: number;
  // Phase M raw-SQL editor mode. "visual" hides raw_sql and uses the
  // structured builder below; "sql" hides every visual section and
  // posts to /run-sql. The toggle is gated on the
  // `insights_sql_editor` feature flag — when off, the SQL tab
  // button just doesn't render.
  mode: InsightsQueryMode;
  raw_sql: string;
}

const blankForm = (): QueryFormState => ({
  name: "",
  description: "",
  source: "ktype:crm.deal",
  columns: ["id", "name"],
  filters: [],
  group_by: [],
  aggregations: [],
  sort: [],
  limit: 100,
  calculated_columns: [],
  cache_ttl_seconds: 300,
  mode: "visual",
  raw_sql: "",
});

function fromQuery(q: InsightsQuery): QueryFormState {
  const def = q.definition;
  return {
    name: q.name,
    description: q.description ?? "",
    source: def.source,
    columns: def.columns ?? [],
    filters: def.filters ?? [],
    group_by: def.group_by ?? [],
    aggregations: def.aggregations ?? [],
    sort: def.sort ?? [],
    limit: def.limit ?? 100,
    calculated_columns: def.calculated_columns ?? [],
    cache_ttl_seconds: q.cache_ttl_seconds ?? 300,
    mode: q.mode ?? "visual",
    raw_sql: q.raw_sql ?? "",
  };
}

function buildDefinition(state: QueryFormState): InsightsQueryDefinition {
  return {
    source: state.source,
    columns: state.columns,
    filters: state.filters,
    group_by: state.group_by.length > 0 ? state.group_by : undefined,
    aggregations:
      state.aggregations.length > 0 ? state.aggregations : undefined,
    sort: state.sort.length > 0 ? state.sort : undefined,
    limit: state.limit > 0 ? state.limit : undefined,
    calculated_columns:
      state.calculated_columns.length > 0
        ? state.calculated_columns
        : undefined,
  };
}

export function InsightsQueryBuilderPage() {
  const qc = useQueryClient();
  const queriesQuery = useQuery<{ queries: InsightsQuery[] }>({
    queryKey: ["insights-queries"],
    queryFn: () => api.listInsightsQueries(),
  });
  const ktypesQuery = useQuery<KType[]>({
    queryKey: ["ktypes"],
    queryFn: () => api.listKTypes(),
    staleTime: 60_000,
  });

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [form, setForm] = useState<QueryFormState>(blankForm());
  const [previewVizType, setPreviewVizType] =
    useState<InsightsVizType>("table");
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<InsightsRunResult | null>(null);
  const [shareOpen, setShareOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);

  // When a saved query is picked from the sidebar, hydrate the form
  // from it. Re-runs preview for live cache-aware result.
  useEffect(() => {
    if (!selectedId) return;
    const q = queriesQuery.data?.queries.find((q) => q.id === selectedId);
    if (q) setForm(fromQuery(q));
  }, [selectedId, queriesQuery.data]);

  const sourceOptions = useMemo(() => {
    const ktypeOptions = (ktypesQuery.data ?? []).map((k) => `ktype:${k.name}`);
    return [...ktypeOptions, ...LEDGER_SOURCES];
  }, [ktypesQuery.data]);

  // Visual mode persists a structured definition; SQL mode persists
  // mode="sql" + raw_sql and lets the server apply the column-level
  // CHECK in migrations/000045_insights_sql_mode.sql.
  const buildInput = () =>
    form.mode === "sql"
      ? {
          name: form.name,
          description: form.description,
          definition: buildDefinition(form),
          cache_ttl_seconds: form.cache_ttl_seconds,
          mode: "sql" as const,
          raw_sql: form.raw_sql,
        }
      : {
          name: form.name,
          description: form.description,
          definition: buildDefinition(form),
          cache_ttl_seconds: form.cache_ttl_seconds,
          mode: "visual" as const,
        };

  const createMut = useMutation({
    mutationFn: () => api.createInsightsQuery(buildInput()),
    onSuccess: (saved) => {
      setSelectedId(saved.id);
      setError(null);
      qc.invalidateQueries({ queryKey: ["insights-queries"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const updateMut = useMutation({
    mutationFn: (id: string) => api.updateInsightsQuery(id, buildInput()),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["insights-queries"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const runMut = useMutation({
    mutationFn: (id: string) =>
      form.mode === "sql"
        ? api.runInsightsQuerySQL(id, { raw_sql: form.raw_sql })
        : api.runInsightsQuery(id, { bypass_cache: false }),
    onSuccess: (res) => {
      setPreview(res);
      setError(null);
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.deleteInsightsQuery(id),
    onSuccess: () => {
      setSelectedId(null);
      setForm(blankForm());
      setPreview(null);
      qc.invalidateQueries({ queryKey: ["insights-queries"] });
    },
    onError: (err: Error) => setError(err.message),
    // Await-mutation pattern: keep the dialog open showing "Working…"
    // until the delete settles, then close it regardless of outcome
    // (errors surface in the page-level error banner).
    onSettled: () => setDeleteOpen(false),
  });

  const onSave = () => {
    if (!form.name.trim()) {
      setError("query name required");
      return;
    }
    if (form.mode === "sql" && !form.raw_sql.trim()) {
      setError("sql body required for sql-mode queries");
      return;
    }
    if (selectedId) updateMut.mutate(selectedId);
    else createMut.mutate();
  };

  const onRun = () => {
    if (!selectedId) {
      setError("save the query before running it");
      return;
    }
    runMut.mutate(selectedId);
  };

  const moveColumn = (from: number, to: number) => {
    if (to < 0 || to >= form.columns.length) return;
    const next = [...form.columns];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved);
    setForm({ ...form, columns: next });
  };

  return (
    <section>
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Insights — Query Builder
      </h1>
      <p className="text-fg-muted">
        Compose a saved query over a KType or ledger table. Filters,
        group-by, aggregations and calculated columns are validated
        server-side before SQL is emitted.
      </p>

      <div className="flex gap-4">
        <aside className="flex-[0_0_220px]">
          <div className="flex items-center justify-between">
            <h3 className="text-sm font-semibold text-fg">Saved queries</h3>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => {
                setSelectedId(null);
                setForm(blankForm());
                setPreview(null);
              }}
            >
              + New
            </Button>
          </div>
          {queriesQuery.isLoading && (
            <p className="text-sm text-fg-muted">Loading…</p>
          )}
          <ul className="m-0 list-none p-0 text-sm">
            {(queriesQuery.data?.queries ?? []).map((q) => (
              <li key={q.id} className="py-1">
                <button
                  onClick={() => setSelectedId(q.id)}
                  className={cn(
                    "cursor-pointer rounded text-left transition-colors hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                    selectedId === q.id
                      ? "font-semibold text-fg"
                      : "text-accent",
                  )}
                >
                  {q.name}
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div className="flex flex-1 flex-col gap-3">
          <div className="flex gap-2">
            <Input
              placeholder="query name"
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              className="flex-1"
            />
            <Input
              placeholder="description (optional)"
              value={form.description}
              onChange={(e) =>
                setForm({ ...form, description: e.target.value })
              }
              className="flex-[2]"
            />
            <Button
              onClick={onSave}
              disabled={createMut.isPending || updateMut.isPending}
            >
              {selectedId ? "Update" : "Save"}
            </Button>
            {selectedId && (
              <>
                <Button
                  variant="secondary"
                  onClick={onRun}
                  disabled={runMut.isPending}
                >
                  {runMut.isPending ? "Running…" : "Run"}
                </Button>
                <Button variant="outline" onClick={() => setShareOpen(true)}>
                  Share…
                </Button>
                <Button
                  variant="destructive"
                  onClick={() => setDeleteOpen(true)}
                >
                  Delete
                </Button>
              </>
            )}
          </div>

          {/* Phase M visual / SQL tab switch. Both tabs always render
              client-side; the server enforces the `insights_sql_editor`
              feature flag on POST /run-sql so a non-enterprise plan
              that hits the SQL tab will see a 403 envelope when it
              tries to run, not when it switches tabs. */}
          <div
            role="tablist"
            className="flex gap-1 border-b border-border"
          >
            <TabButton
              active={form.mode === "visual"}
              onClick={() => setForm({ ...form, mode: "visual", raw_sql: "" })}
            >
              Visual builder
            </TabButton>
            <TabButton
              active={form.mode === "sql"}
              onClick={() => setForm({ ...form, mode: "sql" })}
            >
              SQL editor
            </TabButton>
          </div>

          {form.mode === "sql" && (
            <Section title="SQL (parameterised)">
              <p className="mt-0 text-xs text-fg-muted">
                Runs under the per-tenant <code>app.tenant_id</code> GUC
                with <code>SET LOCAL statement_timeout</code>. RLS pins
                every read to your tenant. Use <code>$1</code>,{" "}
                <code>$2</code>, … for parameters and pass them in the
                params field below.
              </p>
              <textarea
                value={form.raw_sql}
                onChange={(e) =>
                  setForm({ ...form, raw_sql: e.target.value })
                }
                onKeyDown={(e) => {
                  // Tab inserts two spaces instead of leaving the
                  // editor — minimum-viable code-editor behaviour
                  // without dragging in Monaco/CodeMirror.
                  if (e.key === "Tab") {
                    e.preventDefault();
                    const t = e.currentTarget;
                    const start = t.selectionStart;
                    const end = t.selectionEnd;
                    const next =
                      form.raw_sql.slice(0, start) +
                      "  " +
                      form.raw_sql.slice(end);
                    setForm({ ...form, raw_sql: next });
                    requestAnimationFrame(() => {
                      t.selectionStart = t.selectionEnd = start + 2;
                    });
                  }
                }}
                placeholder="SELECT id, name FROM crm_deals WHERE stage = $1"
                rows={14}
                spellCheck={false}
                className="w-full resize-y rounded-md border border-border bg-bg px-2 py-2 font-mono text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
              />
            </Section>
          )}

          {form.mode === "visual" && (
          <>
          <Section title="Source">
            <Select
              value={form.source}
              onChange={(e) => setForm({ ...form, source: e.target.value })}
            >
              {sourceOptions.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </Select>
          </Section>

          <Section title="Columns (drag to reorder)">
            <ul className="m-0 list-none p-0">
              {form.columns.map((c, i) => (
                <li
                  key={`${c}-${i}`}
                  draggable
                  onDragStart={(e) =>
                    e.dataTransfer.setData("text/plain", String(i))
                  }
                  onDragOver={(e) => e.preventDefault()}
                  onDrop={(e) => {
                    e.preventDefault();
                    const from = Number(e.dataTransfer.getData("text/plain"));
                    moveColumn(from, i);
                  }}
                  className="mb-1 flex cursor-grab items-center gap-2 border border-border bg-bg-subtle p-1"
                >
                  <span className="text-fg-subtle">⋮⋮</span>
                  <Input
                    value={c}
                    onChange={(e) => {
                      const next = [...form.columns];
                      next[i] = e.target.value;
                      setForm({ ...form, columns: next });
                    }}
                    className="flex-1"
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label="Move column up"
                    onClick={() => moveColumn(i, i - 1)}
                  >
                    ↑
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label="Move column down"
                    onClick={() => moveColumn(i, i + 1)}
                  >
                    ↓
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-danger"
                    aria-label="Remove column"
                    onClick={() =>
                      setForm({
                        ...form,
                        columns: form.columns.filter((_, j) => j !== i),
                      })
                    }
                  >
                    ✕
                  </Button>
                </li>
              ))}
            </ul>
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                setForm({ ...form, columns: [...form.columns, "new_column"] })
              }
            >
              + Add column
            </Button>
          </Section>

          <Section title="Filters">
            <ul className="m-0 list-none p-0">
              {form.filters.map((f, i) => (
                <li key={i} className="mb-1 flex gap-1.5">
                  <Input
                    placeholder="column"
                    value={f.column}
                    onChange={(e) => {
                      const next = [...form.filters];
                      next[i] = { ...f, column: e.target.value };
                      setForm({ ...form, filters: next });
                    }}
                    className="flex-[2]"
                  />
                  <Select
                    value={f.op}
                    onChange={(e) => {
                      const next = [...form.filters];
                      next[i] = { ...f, op: e.target.value };
                      setForm({ ...form, filters: next });
                    }}
                  >
                    {FILTER_OPS.map((op) => (
                      <option key={op} value={op}>
                        {op}
                      </option>
                    ))}
                  </Select>
                  <Input
                    placeholder="value"
                    value={
                      f.value === undefined || f.value === null
                        ? ""
                        : String(f.value)
                    }
                    onChange={(e) => {
                      const next = [...form.filters];
                      next[i] = { ...f, value: e.target.value };
                      setForm({ ...form, filters: next });
                    }}
                    className="flex-[2]"
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-danger"
                    aria-label="Remove filter"
                    onClick={() =>
                      setForm({
                        ...form,
                        filters: form.filters.filter((_, j) => j !== i),
                      })
                    }
                  >
                    ✕
                  </Button>
                </li>
              ))}
            </ul>
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                setForm({
                  ...form,
                  filters: [
                    ...form.filters,
                    { column: "", op: "=", value: "" },
                  ],
                })
              }
            >
              + Add filter
            </Button>
          </Section>

          <Section title="Group by">
            <Input
              placeholder="comma-separated columns"
              value={form.group_by.join(", ")}
              onChange={(e) =>
                setForm({
                  ...form,
                  group_by: e.target.value
                    .split(",")
                    .map((s) => s.trim())
                    .filter(Boolean),
                })
              }
            />
          </Section>

          <Section title="Aggregations">
            <ul className="m-0 list-none p-0">
              {form.aggregations.map((agg, i) => (
                <li key={i} className="mb-1 flex gap-1.5">
                  <Select
                    value={agg.op}
                    onChange={(e) => {
                      const next = [...form.aggregations];
                      next[i] = {
                        ...agg,
                        op: e.target.value as ReportAggregation["op"],
                      };
                      setForm({ ...form, aggregations: next });
                    }}
                  >
                    {AGG_OPS.map((op) => (
                      <option key={op} value={op}>
                        {op}
                      </option>
                    ))}
                  </Select>
                  <Input
                    placeholder="column"
                    value={agg.column ?? ""}
                    onChange={(e) => {
                      const next = [...form.aggregations];
                      next[i] = { ...agg, column: e.target.value };
                      setForm({ ...form, aggregations: next });
                    }}
                    className="flex-1"
                  />
                  <Input
                    placeholder="alias"
                    value={agg.alias ?? ""}
                    onChange={(e) => {
                      const next = [...form.aggregations];
                      next[i] = { ...agg, alias: e.target.value };
                      setForm({ ...form, aggregations: next });
                    }}
                    className="flex-1"
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-danger"
                    aria-label="Remove aggregation"
                    onClick={() =>
                      setForm({
                        ...form,
                        aggregations: form.aggregations.filter(
                          (_, j) => j !== i
                        ),
                      })
                    }
                  >
                    ✕
                  </Button>
                </li>
              ))}
            </ul>
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                setForm({
                  ...form,
                  aggregations: [
                    ...form.aggregations,
                    { op: "count", alias: "count" },
                  ],
                })
              }
            >
              + Add aggregation
            </Button>
          </Section>

          <Section title="Calculated columns">
            <ul className="m-0 list-none p-0">
              {form.calculated_columns.map((c, i) => (
                <li key={i} className="mb-1 flex gap-1.5">
                  <Input
                    placeholder="name"
                    value={c.name}
                    onChange={(e) => {
                      const next = [...form.calculated_columns];
                      next[i] = { ...c, name: e.target.value };
                      setForm({ ...form, calculated_columns: next });
                    }}
                    className="flex-1"
                  />
                  <Input
                    placeholder="expression e.g. price * qty"
                    value={c.expression}
                    onChange={(e) => {
                      const next = [...form.calculated_columns];
                      next[i] = { ...c, expression: e.target.value };
                      setForm({ ...form, calculated_columns: next });
                    }}
                    className="flex-[3]"
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-danger"
                    aria-label="Remove calculated column"
                    onClick={() =>
                      setForm({
                        ...form,
                        calculated_columns: form.calculated_columns.filter(
                          (_, j) => j !== i
                        ),
                      })
                    }
                  >
                    ✕
                  </Button>
                </li>
              ))}
            </ul>
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                setForm({
                  ...form,
                  calculated_columns: [
                    ...form.calculated_columns,
                    { name: "", expression: "" },
                  ],
                })
              }
            >
              + Add calculated column
            </Button>
          </Section>

          <Section title="Sort + limit + cache">
            <div className="flex flex-wrap items-center gap-1.5">
              <label className="text-sm text-fg">Sort:</label>
              <Input
                placeholder="column"
                className="w-auto"
                value={form.sort[0]?.column ?? ""}
                onChange={(e) => {
                  const next: ReportSort[] = e.target.value
                    ? [{ column: e.target.value, direction: form.sort[0]?.direction ?? "asc" }]
                    : [];
                  setForm({ ...form, sort: next });
                }}
              />
              <Select
                className="w-auto"
                value={form.sort[0]?.direction ?? "asc"}
                onChange={(e) => {
                  if (form.sort.length === 0) return;
                  const next = [...form.sort];
                  next[0] = {
                    ...next[0],
                    direction: e.target.value as "asc" | "desc",
                  };
                  setForm({ ...form, sort: next });
                }}
              >
                <option value="asc">asc</option>
                <option value="desc">desc</option>
              </Select>
              <label className="ml-3 text-sm text-fg">Limit:</label>
              <Input
                type="number"
                value={form.limit}
                onChange={(e) =>
                  setForm({ ...form, limit: Number(e.target.value) })
                }
                className="w-20"
              />
              <label className="ml-3 text-sm text-fg">Cache TTL (s):</label>
              <Input
                type="number"
                value={form.cache_ttl_seconds}
                onChange={(e) =>
                  setForm({
                    ...form,
                    cache_ttl_seconds: Number(e.target.value),
                  })
                }
                className="w-20"
              />
            </div>
          </Section>
          </>
          )}

          {error && <div className="text-sm text-danger">{error}</div>}

          {preview && (
            <Section title={`Preview (${preview.cache_hit ? "cache" : "live"})`}>
              <div className="mb-2 flex items-center gap-2">
                <label className="text-sm text-fg">Visualisation:</label>
                <Select
                  className="w-auto"
                  value={previewVizType}
                  onChange={(e) =>
                    setPreviewVizType(e.target.value as InsightsVizType)
                  }
                >
                  {VIZ_OPTIONS.map((v) => (
                    <option key={v} value={v}>
                      {v}
                    </option>
                  ))}
                </Select>
                <span className="ml-auto text-fg-muted">
                  {preview.result.rows.length} rows
                </span>
              </div>
              <Viz vizType={previewVizType} result={preview.result} />
            </Section>
          )}
        </div>
      </div>

      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={(open) => {
          if (!open && deleteMut.isPending) return;
          setDeleteOpen(open);
        }}
        title="Delete this query?"
        description="This permanently removes the saved query."
        destructive
        loading={deleteMut.isPending}
        onConfirm={() => {
          if (selectedId) deleteMut.mutate(selectedId);
        }}
      />

      {shareOpen && selectedId && (
        <ShareModal
          resource="query"
          resourceId={selectedId}
          resourceName={form.name}
          onClose={() => setShareOpen(false)}
        />
      )}
    </section>
  );
}

function TabButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={active}
      onClick={onClick}
      className={cn(
        "-mb-px cursor-pointer border-b-2 px-3 py-2 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
        active
          ? "border-accent font-semibold text-fg"
          : "border-transparent text-fg-muted hover:text-fg",
      )}
    >
      {children}
    </button>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <fieldset className="rounded-md border border-border p-3">
      <legend className="px-1.5 text-sm font-semibold text-fg">
        {title}
      </legend>
      {children}
    </fieldset>
  );
}
