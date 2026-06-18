// Insights — visual query builder.
//
// Composes a saved insights query (extends reporting.Definition with
// calculated columns) without writing SQL. The source picker covers
// both KType-backed sources (`ktype:<name>`) and the canonical ledger
// / inventory tables; filters, group-by, aggregations and calculated
// columns are added through structured controls. When the chosen
// source is a KType we know its fields, so columns/filters/group-by
// become field pickers (humanized labels) rather than free text. The
// live preview hits POST /api/v1/insights/queries/{id}/run after a
// Save. An opt-in SQL tab (gated server-side on `insights_sql_editor`)
// stays available for power users.

import { useEffect, useMemo, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import type {
  CalculatedColumn,
  FieldSpec,
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
  Badge,
  Button,
  ConfirmDialog,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  cn,
} from "@kapp/ui";
import { api } from "../lib/api";
import { humanizeLabel, ktypeSingular } from "../lib/ktypeView";
import { Viz } from "../components/insights/Charts";
import { ShareModal } from "../components/insights/ShareModal";

// Curated list of non-KType ledger / inventory / helpdesk tables the
// reporting runner will accept as a `source`. Mirrors
// internal/reporting.AllowedTables — kept short on purpose; the more
// exotic surfaces are reachable via the SQL editor.
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

// Plain-language label for each viz type (never surface the raw token).
const VIZ_LABELS: Record<InsightsVizType, string> = {
  table: "Table",
  bar: "Bar chart",
  line: "Line chart",
  pie: "Pie chart",
  donut: "Donut chart",
  funnel: "Funnel",
  number_card: "Single number",
  pivot: "Pivot table",
};

const FILTER_OPS = ["=", "!=", ">", ">=", "<", "<=", "in", "not_in", "ilike"];

// Human phrasing for each operator so a filter reads like a sentence
// ("Stage is any of …") instead of exposing `in` / `ilike`.
const FILTER_OP_LABELS: Record<string, string> = {
  "=": "equals",
  "!=": "doesn’t equal",
  ">": "greater than",
  ">=": "at least",
  "<": "less than",
  "<=": "at most",
  in: "is any of",
  not_in: "is none of",
  ilike: "contains",
};

const AGG_OPS: ReportAggregation["op"][] = [
  "count",
  "sum",
  "avg",
  "min",
  "max",
];

const AGG_OP_LABELS: Record<ReportAggregation["op"], string> = {
  count: "Count",
  sum: "Sum",
  avg: "Average",
  min: "Minimum",
  max: "Maximum",
};

// A multi-value operator takes a comma-separated list rather than a
// single value.
const MULTI_VALUE_OPS = new Set(["in", "not_in"]);

function sourceLabel(source: string): string {
  if (source.startsWith("ktype:")) return ktypeSingular(source.slice(6));
  return humanizeLabel(source);
}

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
    return { ktypeOptions, ledger: LEDGER_SOURCES };
  }, [ktypesQuery.data]);

  // Fields for the selected KType source (drives the column / filter /
  // group-by pickers). Empty for ledger sources — those fall back to
  // free-text entry so a power user can still name any column.
  const sourceFields = useMemo<FieldSpec[]>(() => {
    if (!form.source.startsWith("ktype:")) return [];
    const name = form.source.slice("ktype:".length);
    const kt = (ktypesQuery.data ?? []).find((k) => k.name === name);
    return kt?.schema.fields ?? [];
  }, [form.source, ktypesQuery.data]);
  const hasFieldMeta = sourceFields.length > 0;
  const fieldByName = useMemo(() => {
    const m = new Map<string, FieldSpec>();
    for (const f of sourceFields) m.set(f.name, f);
    return m;
  }, [sourceFields]);

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

  const addColumn = () => {
    if (hasFieldMeta) {
      const unused = sourceFields.find((f) => !form.columns.includes(f.name));
      setForm({ ...form, columns: [...form.columns, unused?.name ?? ""] });
    } else {
      setForm({ ...form, columns: [...form.columns, "new_column"] });
    }
  };

  const saving = createMut.isPending || updateMut.isPending;
  const queries = queriesQuery.data?.queries ?? [];

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Insights</Eyebrow>
            <h1 className="mt-1 text-2xl font-semibold tracking-tight text-fg">
              Query Builder
            </h1>
            <p className="mt-1 max-w-prose text-sm text-fg-muted">
              Ask a question of your data — pick a source, choose the fields
              and filters you care about, then preview it as a chart or table.
              No SQL required.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button onClick={onSave} disabled={saving}>
              {saving
                ? "Saving…"
                : selectedId
                  ? "Update query"
                  : "Save"}
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
                  Share
                </Button>
                <Button
                  variant="ghost"
                  className="text-danger hover:text-danger"
                  onClick={() => setDeleteOpen(true)}
                >
                  Delete
                </Button>
              </>
            )}
          </div>
        </div>
      </header>

      <div className="flex flex-col gap-5 lg:flex-row">
        <aside className="lg:flex-[0_0_240px]">
          <div className="flex items-center justify-between gap-2">
            <h2 className="text-sm font-semibold text-fg">Saved queries</h2>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => {
                setSelectedId(null);
                setForm(blankForm());
                setPreview(null);
                setError(null);
              }}
            >
              + New
            </Button>
          </div>
          {queriesQuery.isLoading ? (
            <div className="mt-2 flex flex-col gap-1.5" aria-hidden>
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-4/5" />
            </div>
          ) : queriesQuery.isError ? (
            <div className="mt-2 rounded-lg border border-border p-3 text-sm">
              <p className="text-fg-muted">Couldn’t load your saved queries.</p>
              <Button
                size="sm"
                variant="outline"
                className="mt-2"
                onClick={() => queriesQuery.refetch()}
              >
                Try again
              </Button>
            </div>
          ) : queries.length === 0 ? (
            <p className="mt-2 text-sm text-fg-muted">
              No saved queries yet. Build one on the right and save it to pin
              it here.
            </p>
          ) : (
            <ul className="mt-2 flex list-none flex-col gap-0.5 p-0">
              {queries.map((q) => (
                <li key={q.id}>
                  <button
                    onClick={() => setSelectedId(q.id)}
                    className={cn(
                      "w-full cursor-pointer truncate rounded-md px-2.5 py-1.5 text-left text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                      selectedId === q.id
                        ? "bg-bg-muted font-medium text-fg"
                        : "text-fg-muted hover:bg-bg-subtle hover:text-fg",
                    )}
                  >
                    {q.name}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </aside>

        <div className="flex min-w-0 flex-1 flex-col gap-4">
          <div className="grid gap-3 sm:grid-cols-[2fr_3fr]">
            <Field label="Query name" required>
              <Input
                placeholder="Query name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </Field>
            <Field label="Description" help="Optional — shown on dashboards and shares.">
              <Input
                placeholder="What does this answer?"
                value={form.description}
                onChange={(e) =>
                  setForm({ ...form, description: e.target.value })
                }
              />
            </Field>
          </div>

          {/* Phase M visual / SQL tab switch. Both tabs always render
              client-side; the server enforces the `insights_sql_editor`
              feature flag on POST /run-sql so a non-enterprise plan
              that hits the SQL tab will see a 403 envelope when it
              tries to run, not when it switches tabs. */}
          <div role="tablist" className="flex gap-1 border-b border-border">
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
                Runs under the per-tenant <code>app.tenant_id</code> GUC with{" "}
                <code>SET LOCAL statement_timeout</code>. Row-level security
                pins every read to your tenant. Use <code>$1</code>,{" "}
                <code>$2</code>, … for parameters.
              </p>
              <textarea
                value={form.raw_sql}
                onChange={(e) => setForm({ ...form, raw_sql: e.target.value })}
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
                className="w-full resize-y rounded-md border border-border bg-bg px-3 py-2 font-mono text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
              />
            </Section>
          )}

          {form.mode === "visual" && (
            <>
              <Section
                title="Source"
                hint="The records or ledger table this query reads from."
              >
                <Select
                  aria-label="Source"
                  value={form.source}
                  onChange={(e) => setForm({ ...form, source: e.target.value })}
                >
                  <optgroup label="Records">
                    {sourceOptions.ktypeOptions.map((s) => (
                      <option key={s} value={s}>
                        {sourceLabel(s)}
                      </option>
                    ))}
                  </optgroup>
                  <optgroup label="Ledger & operations">
                    {sourceOptions.ledger.map((s) => (
                      <option key={s} value={s}>
                        {sourceLabel(s)}
                      </option>
                    ))}
                  </optgroup>
                </Select>
              </Section>

              <Section
                title="Columns"
                hint={
                  hasFieldMeta
                    ? "Choose the fields to show. Drag to reorder."
                    : "Name the columns to show. Drag to reorder."
                }
              >
                {form.columns.length === 0 ? (
                  <p className="mb-2 text-sm text-fg-muted">
                    No columns yet — add one below.
                  </p>
                ) : (
                  <ul className="m-0 flex list-none flex-col gap-1.5 p-0">
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
                          const from = Number(
                            e.dataTransfer.getData("text/plain"),
                          );
                          moveColumn(from, i);
                        }}
                        className="flex cursor-grab items-center gap-2 rounded-md border border-border bg-bg-subtle px-2 py-1.5"
                      >
                        <span className="select-none text-fg-subtle" aria-hidden>
                          ⠿
                        </span>
                        {hasFieldMeta ? (
                          <FieldSelect
                            ariaLabel={`Column ${i + 1}`}
                            value={c}
                            fields={sourceFields}
                            onChange={(value) => {
                              const next = [...form.columns];
                              next[i] = value;
                              setForm({ ...form, columns: next });
                            }}
                          />
                        ) : (
                          <Input
                            aria-label={`Column ${i + 1}`}
                            value={c}
                            onChange={(e) => {
                              const next = [...form.columns];
                              next[i] = e.target.value;
                              setForm({ ...form, columns: next });
                            }}
                            className="flex-1"
                          />
                        )}
                        <Button
                          size="sm"
                          variant="ghost"
                          aria-label="Move column up"
                          disabled={i === 0}
                          onClick={() => moveColumn(i, i - 1)}
                        >
                          ↑
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          aria-label="Move column down"
                          disabled={i === form.columns.length - 1}
                          onClick={() => moveColumn(i, i + 1)}
                        >
                          ↓
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          className="text-danger hover:text-danger"
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
                )}
                <Button
                  size="sm"
                  variant="outline"
                  className="mt-2"
                  onClick={addColumn}
                >
                  + Add column
                </Button>
              </Section>

              <Section
                title="Filters"
                hint="Narrow the rows. All filters must match."
              >
                {form.filters.length > 0 && (
                  <ul className="m-0 flex list-none flex-col gap-1.5 p-0">
                    {form.filters.map((f, i) => {
                      const spec = fieldByName.get(f.column);
                      const enumValues = spec?.values ?? [];
                      const multi = MULTI_VALUE_OPS.has(f.op);
                      return (
                        <li key={i} className="flex flex-wrap items-center gap-1.5">
                          {hasFieldMeta ? (
                            <FieldSelect
                              ariaLabel={`Filter ${i + 1} field`}
                              value={f.column}
                              fields={sourceFields}
                              allowEmpty
                              emptyLabel="Choose a field"
                              onChange={(value) => {
                                const next = [...form.filters];
                                next[i] = { ...f, column: value };
                                setForm({ ...form, filters: next });
                              }}
                            />
                          ) : (
                            <Input
                              aria-label={`Filter ${i + 1} field`}
                              placeholder="Field"
                              value={f.column}
                              onChange={(e) => {
                                const next = [...form.filters];
                                next[i] = { ...f, column: e.target.value };
                                setForm({ ...form, filters: next });
                              }}
                              className="flex-[2]"
                            />
                          )}
                          <Select
                            aria-label={`Filter ${i + 1} condition`}
                            value={f.op}
                            onChange={(e) => {
                              const next = [...form.filters];
                              next[i] = { ...f, op: e.target.value };
                              setForm({ ...form, filters: next });
                            }}
                          >
                            {FILTER_OPS.map((op) => (
                              <option key={op} value={op}>
                                {FILTER_OP_LABELS[op] ?? op}
                              </option>
                            ))}
                          </Select>
                          {enumValues.length > 0 && !multi ? (
                            <Select
                              aria-label={`Filter ${i + 1} value`}
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
                            >
                              <option value="">Any</option>
                              {enumValues.map((v) => (
                                <option key={v} value={v}>
                                  {humanizeLabel(v)}
                                </option>
                              ))}
                            </Select>
                          ) : (
                            <Input
                              aria-label={`Filter ${i + 1} value`}
                              placeholder={multi ? "value, value, …" : "Value"}
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
                          )}
                          <Button
                            size="sm"
                            variant="ghost"
                            className="text-danger hover:text-danger"
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
                      );
                    })}
                  </ul>
                )}
                <Button
                  size="sm"
                  variant="outline"
                  className="mt-2"
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

              <Section
                title="Group by"
                hint="Roll rows up by a field — e.g. group deals by stage."
              >
                {form.group_by.length > 0 && (
                  <div className="mb-2 flex flex-wrap gap-1.5">
                    {form.group_by.map((g) => (
                      <Badge key={g} variant="neutral" className="gap-1">
                        {hasFieldMeta ? humanizeLabel(g) : g}
                        <button
                          type="button"
                          aria-label={`Remove ${humanizeLabel(g)} grouping`}
                          className="cursor-pointer text-fg-subtle hover:text-fg focus-visible:outline-none"
                          onClick={() =>
                            setForm({
                              ...form,
                              group_by: form.group_by.filter((x) => x !== g),
                            })
                          }
                        >
                          ✕
                        </button>
                      </Badge>
                    ))}
                  </div>
                )}
                {hasFieldMeta ? (
                  <FieldSelect
                    ariaLabel="Add a grouping field"
                    value=""
                    allowEmpty
                    emptyLabel="Add a field to group by…"
                    fields={sourceFields.filter(
                      (f) => !form.group_by.includes(f.name),
                    )}
                    onChange={(value) => {
                      if (!value) return;
                      setForm({ ...form, group_by: [...form.group_by, value] });
                    }}
                  />
                ) : (
                  <Input
                    aria-label="Group by columns"
                    placeholder="Comma-separated columns"
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
                )}
              </Section>

              <Section
                title="Summarise"
                hint="Totals and counts for each group — e.g. sum of deal value."
              >
                {form.aggregations.length > 0 && (
                  <ul className="m-0 flex list-none flex-col gap-1.5 p-0">
                    {form.aggregations.map((agg, i) => (
                      <li key={i} className="flex flex-wrap items-center gap-1.5">
                        <Select
                          aria-label={`Summary ${i + 1} function`}
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
                              {AGG_OP_LABELS[op]}
                            </option>
                          ))}
                        </Select>
                        {agg.op === "count" ? (
                          <span className="text-sm text-fg-muted">of rows</span>
                        ) : hasFieldMeta ? (
                          <FieldSelect
                            ariaLabel={`Summary ${i + 1} field`}
                            value={agg.column ?? ""}
                            allowEmpty
                            emptyLabel="Choose a field"
                            fields={sourceFields}
                            onChange={(value) => {
                              const next = [...form.aggregations];
                              next[i] = { ...agg, column: value };
                              setForm({ ...form, aggregations: next });
                            }}
                          />
                        ) : (
                          <Input
                            aria-label={`Summary ${i + 1} field`}
                            placeholder="Field"
                            value={agg.column ?? ""}
                            onChange={(e) => {
                              const next = [...form.aggregations];
                              next[i] = { ...agg, column: e.target.value };
                              setForm({ ...form, aggregations: next });
                            }}
                            className="flex-1"
                          />
                        )}
                        <Input
                          aria-label={`Summary ${i + 1} label`}
                          placeholder="Label (optional)"
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
                          className="text-danger hover:text-danger"
                          aria-label="Remove summary"
                          onClick={() =>
                            setForm({
                              ...form,
                              aggregations: form.aggregations.filter(
                                (_, j) => j !== i,
                              ),
                            })
                          }
                        >
                          ✕
                        </Button>
                      </li>
                    ))}
                  </ul>
                )}
                <Button
                  size="sm"
                  variant="outline"
                  className="mt-2"
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
                  + Add summary
                </Button>
              </Section>

              <Section
                title="Calculated columns"
                hint="Derive a new value with a formula — e.g. price × quantity."
              >
                {form.calculated_columns.length > 0 && (
                  <ul className="m-0 flex list-none flex-col gap-1.5 p-0">
                    {form.calculated_columns.map((c, i) => (
                      <li key={i} className="flex flex-wrap items-center gap-1.5">
                        <Input
                          aria-label={`Calculated column ${i + 1} name`}
                          placeholder="Name"
                          value={c.name}
                          onChange={(e) => {
                            const next = [...form.calculated_columns];
                            next[i] = { ...c, name: e.target.value };
                            setForm({ ...form, calculated_columns: next });
                          }}
                          className="flex-1"
                        />
                        <Input
                          aria-label={`Calculated column ${i + 1} formula`}
                          placeholder="Formula, e.g. price * qty"
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
                          className="text-danger hover:text-danger"
                          aria-label="Remove calculated column"
                          onClick={() =>
                            setForm({
                              ...form,
                              calculated_columns: form.calculated_columns.filter(
                                (_, j) => j !== i,
                              ),
                            })
                          }
                        >
                          ✕
                        </Button>
                      </li>
                    ))}
                  </ul>
                )}
                <Button
                  size="sm"
                  variant="outline"
                  className="mt-2"
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

              <Section title="Sort, limit & caching">
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
                  <Field label="Sort by">
                    {hasFieldMeta ? (
                      <FieldSelect
                        ariaLabel="Sort by"
                        value={form.sort[0]?.column ?? ""}
                        allowEmpty
                        emptyLabel="Default order"
                        fields={sourceFields}
                        onChange={(value) =>
                          setForm({
                            ...form,
                            sort: value
                              ? [
                                  {
                                    column: value,
                                    direction: form.sort[0]?.direction ?? "asc",
                                  },
                                ]
                              : [],
                          })
                        }
                      />
                    ) : (
                      <Input
                        placeholder="Column"
                        value={form.sort[0]?.column ?? ""}
                        onChange={(e) =>
                          setForm({
                            ...form,
                            sort: e.target.value
                              ? [
                                  {
                                    column: e.target.value,
                                    direction: form.sort[0]?.direction ?? "asc",
                                  },
                                ]
                              : [],
                          })
                        }
                      />
                    )}
                  </Field>
                  <Field label="Direction">
                    <Select
                      value={form.sort[0]?.direction ?? "asc"}
                      disabled={form.sort.length === 0}
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
                      <option value="asc">Ascending</option>
                      <option value="desc">Descending</option>
                    </Select>
                  </Field>
                  <Field label="Row limit">
                    <Input
                      type="number"
                      min={1}
                      value={form.limit}
                      onChange={(e) =>
                        setForm({ ...form, limit: Number(e.target.value) })
                      }
                    />
                  </Field>
                  <Field
                    label="Cache results"
                    help="Seconds to reuse a result before re-running."
                  >
                    <Input
                      type="number"
                      min={0}
                      value={form.cache_ttl_seconds}
                      onChange={(e) =>
                        setForm({
                          ...form,
                          cache_ttl_seconds: Number(e.target.value),
                        })
                      }
                    />
                  </Field>
                </div>
              </Section>
            </>
          )}

          {error && (
            <div
              role="alert"
              className="rounded-md border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-danger"
            >
              {error}
            </div>
          )}

          <Section title="Preview">
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <Field label="Show as" hideLabel>
                <Select
                  aria-label="Visualisation"
                  className="w-auto"
                  value={previewVizType}
                  onChange={(e) =>
                    setPreviewVizType(e.target.value as InsightsVizType)
                  }
                >
                  {VIZ_OPTIONS.map((v) => (
                    <option key={v} value={v}>
                      {VIZ_LABELS[v]}
                    </option>
                  ))}
                </Select>
              </Field>
              {preview && (
                <>
                  <Badge variant={preview.cache_hit ? "neutral" : "success"}>
                    {preview.cache_hit ? "Cached" : "Live"}
                  </Badge>
                  <span className="ml-auto text-sm text-fg-muted">
                    {preview.result.rows.length}{" "}
                    {preview.result.rows.length === 1 ? "row" : "rows"}
                  </span>
                </>
              )}
            </div>
            {runMut.isPending ? (
              <Skeleton className="h-64 w-full" />
            ) : !selectedId ? (
              <p className="py-8 text-center text-sm text-fg-muted">
                Save this query, then run it to preview the result here.
              </p>
            ) : !preview ? (
              <div className="flex flex-col items-center gap-3 py-8 text-center">
                <p className="text-sm text-fg-muted">
                  Ready to preview — run the query to see your data.
                </p>
                <Button variant="secondary" onClick={onRun}>
                  Run query
                </Button>
              </div>
            ) : preview.result.rows.length === 0 ? (
              <p className="py-8 text-center text-sm text-fg-muted">
                No rows matched. Try loosening your filters.
              </p>
            ) : (
              <Viz vizType={previewVizType} result={preview.result} />
            )}
          </Section>
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

// A field picker that always preserves the current value even when it
// isn't one of the source's known fields (so a custom column typed in
// a different mode survives a round-trip). Labels are humanized; the
// option value stays the raw field key the runner expects.
function FieldSelect({
  value,
  fields,
  onChange,
  ariaLabel,
  allowEmpty,
  emptyLabel,
}: {
  value: string;
  fields: FieldSpec[];
  onChange: (value: string) => void;
  ariaLabel: string;
  allowEmpty?: boolean;
  emptyLabel?: string;
}) {
  const known = fields.some((f) => f.name === value);
  return (
    <Select
      aria-label={ariaLabel}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      className="flex-1"
    >
      {allowEmpty && <option value="">{emptyLabel ?? "—"}</option>}
      {!known && value && (
        <option value={value}>{humanizeLabel(value)} (custom)</option>
      )}
      {fields.map((f) => (
        <option key={f.name} value={f.name}>
          {humanizeLabel(f.name)}
        </option>
      ))}
    </Select>
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
  hint,
  children,
}: {
  title: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <fieldset className="rounded-lg border border-border bg-bg-elevated p-4">
      <legend className="px-1 text-sm font-semibold text-fg">{title}</legend>
      {hint && <p className="mb-3 mt-0 text-xs text-fg-muted">{hint}</p>}
      {children}
    </fieldset>
  );
}
